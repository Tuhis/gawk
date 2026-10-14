package screening

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func validFlag() ContentFlag {
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	return ContentFlag{
		Schema:         SchemaContentFlag,
		BroadcastID:    "ABC234",
		PublisherEpoch: "5f2c9a0b11d4e6f7",
		SamplerID:      "gawk-sampler-0",
		Mode:           ModeFlag,
		State:          StateTripped,
		Window:         WindowSummary{Start: t0, End: t0.Add(25 * time.Second), Samples: 6, FlaggedSamples: 4},
		Scores:         Scores{Max: 0.97, Mean: 0.81},
		Labels:         []string{"FEMALE_GENITALIA_EXPOSED"},
		LabelCounts:    map[string]int{"FEMALE_GENITALIA_EXPOSED": 4},
		Recent:         []float64{0.1, 0.2, 0.9, 0.95, 0.97, 0.91},
		Summary:        "sustained explicit signal",
	}
}

// The §4.11 body round-trips with the R40 additions, under the frozen
// property names.
func TestContentFlagRoundTrip(t *testing.T) {
	f := validFlag()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"schema":"gawk.content-flag.v1"`, `"broadcastId"`, `"samplerId"`, `"window"`, `"flaggedSamples"`, `"scores"`, `"labels"`, `"summary"`, `"mode"`, `"state"`, `"recent"`, `"publisherEpoch"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("marshalled body lacks %s: %s", key, raw)
		}
	}
	var back ContentFlag
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f, back) {
		t.Fatalf("round trip changed the body:\n got %+v\nwant %+v", back, f)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("valid body refused: %v", err)
	}
}

func TestContentFlagValidateRejects(t *testing.T) {
	cases := map[string]func(*ContentFlag){
		"wrong schema":        func(f *ContentFlag) { f.Schema = "gawk.content-flag.v2" },
		"absent mode":         func(f *ContentFlag) { f.Mode = "" },
		"unknown mode":        func(f *ContentFlag) { f.Mode = "page" },
		"absent state":        func(f *ContentFlag) { f.State = "" },
		"unknown state":       func(f *ContentFlag) { f.State = "open" },
		"bad broadcast id":    func(f *ContentFlag) { f.BroadcastID = "ABC" },
		"no epoch":            func(f *ContentFlag) { f.PublisherEpoch = "" },
		"epoch with a slash":  func(f *ContentFlag) { f.PublisherEpoch = "a/b" },
		"no sampler":          func(f *ContentFlag) { f.SamplerID = "" },
		"recent over 64":      func(f *ContentFlag) { f.Recent = make([]float64, MaxRecent+1) },
		"recent above 1":      func(f *ContentFlag) { f.Recent = []float64{0.5, 1.01} },
		"recent below 0":      func(f *ContentFlag) { f.Recent = []float64{-0.01} },
		"max above 1":         func(f *ContentFlag) { f.Scores.Max = 1.5 },
		"flagged > samples":   func(f *ContentFlag) { f.Window.FlaggedSamples = 7 },
		"window backwards":    func(f *ContentFlag) { f.Window.End = f.Window.Start.Add(-time.Second) },
		"lower-case label":    func(f *ContentFlag) { f.Labels = []string{"anus_exposed"} },
		"repeated label":      func(f *ContentFlag) { f.Labels = []string{"ANUS_EXPOSED", "ANUS_EXPOSED"} },
		"negative labelCount": func(f *ContentFlag) { f.LabelCounts = map[string]int{"ANUS_EXPOSED": -1} },
		"long summary":        func(f *ContentFlag) { f.Summary = strings.Repeat("x", MaxSummaryLen+1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := validFlag()
			mutate(&f)
			if err := f.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() = %v, want ErrInvalid", err)
			}
		})
	}
	// Exactly 64 recent entries, at the edges of 0–1, are fine.
	f := validFlag()
	f.Recent = make([]float64, MaxRecent)
	f.Recent[0], f.Recent[63] = 0, 1
	if err := f.Validate(); err != nil {
		t.Fatalf("64 recent entries refused: %v", err)
	}
	// A cleared POST carries no recent at all.
	f = validFlag()
	f.State, f.Recent = StateCleared, nil
	if err := f.Validate(); err != nil {
		t.Fatalf("cleared body refused: %v", err)
	}
}

// An absent mode is refused even when the JSON simply omits it — the decoded
// zero value is not a default.
func TestContentFlagAbsentModeFromJSON(t *testing.T) {
	var f ContentFlag
	body := `{"schema":"gawk.content-flag.v1","broadcastId":"ABC234","publisherEpoch":"e1","samplerId":"s","state":"tripped","window":{"samples":1,"flaggedSamples":1},"scores":{"max":0.9,"mean":0.9},"labels":[]}`
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		t.Fatal(err)
	}
	if err := f.Validate(); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("Validate() = %v, want an ErrInvalid naming mode", err)
	}
}

func validBatch() SampleBatch {
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	labels := DefaultLabels()
	return SampleBatch{
		Schema:    SchemaSamples,
		SamplerID: "gawk-sampler-1",
		Mode:      ModeObserve,
		Floor:     0.3,
		Labels:    labels,
		Rule:      &BatchRule{Threshold: 0.5, Window: 6, WindowHits: 3},
		Sessions: []SessionSamples{{
			BroadcastID:    "ABC234",
			BroadcastKey:   "0123456789ab",
			PublisherEpoch: "5f2c9a0b11d4e6f7",
			Taken:          3,
			FirstAt:        t0,
			LastAt:         t0.Add(10 * time.Second),
			Samples: []Sample{{
				Position: 2,
				At:       t0.Add(5 * time.Second),
				Scores:   map[string]float64{labels[0]: 0.31, labels[1]: 0, labels[2]: 0.05},
			}},
		}},
	}
}

func TestSampleBatchValidate(t *testing.T) {
	b := validBatch()
	if err := b.Validate(); err != nil {
		t.Fatalf("valid batch refused: %v", err)
	}
	// A sampler older than R70 sends no rule; that batch is still valid
	// (docs/72 D1).
	b.Rule = nil
	if err := b.Validate(); err != nil {
		t.Fatalf("batch without a rule refused: %v", err)
	}
	cases := map[string]func(*SampleBatch){
		"wrong schema":         func(b *SampleBatch) { b.Schema = SchemaContentFlag },
		"no mode":              func(b *SampleBatch) { b.Mode = "" },
		"floor above 1":        func(b *SampleBatch) { b.Floor = 2 },
		"no labels":            func(b *SampleBatch) { b.Labels = nil },
		"zero taken":           func(b *SampleBatch) { b.Sessions[0].Taken = 0 },
		"position past taken":  func(b *SampleBatch) { b.Sessions[0].Samples[0].Position = 4 },
		"position zero":        func(b *SampleBatch) { b.Sessions[0].Samples[0].Position = 0 },
		"below the floor":      func(b *SampleBatch) { b.Sessions[0].Samples[0].Scores[DefaultLabels()[0]] = 0.29 },
		"unknown label":        func(b *SampleBatch) { b.Sessions[0].Samples[0].Scores["FACE_FEMALE"] = 0.5 },
		"missing label":        func(b *SampleBatch) { delete(b.Sessions[0].Samples[0].Scores, DefaultLabels()[2]) },
		"upper-case key":       func(b *SampleBatch) { b.Sessions[0].BroadcastKey = "0123456789AB" },
		"lastAt before first":  func(b *SampleBatch) { b.Sessions[0].LastAt = b.Sessions[0].FirstAt.Add(-time.Second) },
		"more sent than taken": func(b *SampleBatch) { b.Sessions[0].Taken = 0 },
		"repeated session": func(b *SampleBatch) {
			b.Sessions = append(b.Sessions, b.Sessions[0])
		},
		"positions out of order": func(b *SampleBatch) {
			s := b.Sessions[0].Samples[0]
			s.Position = 1
			b.Sessions[0].Samples = append(b.Sessions[0].Samples, s)
		},
		// docs/72 D1: the rule is optional, and checked when present.
		"rule threshold zero":        func(b *SampleBatch) { b.Rule.Threshold = 0 },
		"rule threshold above 1":     func(b *SampleBatch) { b.Rule.Threshold = 1.5 },
		"rule hits above window":     func(b *SampleBatch) { b.Rule.WindowHits = 7 },
		"rule window too large":      func(b *SampleBatch) { b.Rule.Window = MaxWindow + 1 },
		"rule threshold below floor": func(b *SampleBatch) { b.Rule.Threshold = 0.2 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := validBatch()
			mutate(&b)
			if err := b.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestReplayRequestValidate(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	ok := ReplayRequest{Threshold: 0.8, Window: 6, WindowHits: 4, From: t0, To: t0.Add(24 * time.Hour)}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	for name, r := range map[string]ReplayRequest{
		"zero threshold":   {Threshold: 0, Window: 6, WindowHits: 4, From: t0, To: t0.Add(time.Hour)},
		"hits over window": {Threshold: 0.8, Window: 6, WindowHits: 7, From: t0, To: t0.Add(time.Hour)},
		"window too big":   {Threshold: 0.8, Window: MaxWindow + 1, WindowHits: 4, From: t0, To: t0.Add(time.Hour)},
		"backwards range":  {Threshold: 0.8, Window: 6, WindowHits: 4, From: t0, To: t0},
		"range too long":   {Threshold: 0.8, Window: 6, WindowHits: 4, From: t0, To: t0.Add(MaxReplayRange + time.Hour)},
		"bad label":        {Threshold: 0.8, Window: 6, WindowHits: 4, Labels: []string{"x"}, From: t0, To: t0.Add(time.Hour)},
	} {
		if err := r.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrInvalid", name, err)
		}
	}
}

func TestKeyframeRouteContract(t *testing.T) {
	if got := KeyframePath("ABC234"); got != "/internal/screening/broadcasts/ABC234/keyframe" {
		t.Fatalf("KeyframePath = %q", got)
	}
	if !strings.HasSuffix(KeyframeRoutePattern, "/internal/screening/broadcasts/{id}/keyframe") {
		t.Fatalf("route pattern %q does not match KeyframePath", KeyframeRoutePattern)
	}
	if a, b := KeyframeETag("e1", 7), KeyframeETag("e2", 7); a == b {
		t.Fatalf("two sessions share ETag %s", a)
	}
	if got := KeyframeETag("e1", 7); got != `"e1-7"` {
		t.Fatalf("KeyframeETag = %s", got)
	}
}

// --- Shard (D22) ------------------------------------------------------------

// Golden values. A change to Shard reshuffles every live broadcast between
// samplers on the next rollout; if that is intended, update these and say so
// in the PR.
func TestShardGolden(t *testing.T) {
	cases := []struct {
		key  string
		n    int
		want int
	}{
		{"0123456789ab", 2, 1},
		{"0123456789ab", 3, 1},
		{"0123456789ab", 8, 7},
		{"ffffffffffff", 2, 1},
		{"ffffffffffff", 5, 3},
		{"a1b2c3d4e5f6", 2, 0},
		{"a1b2c3d4e5f6", 7, 4},
		{"", 4, 0},
	}
	for _, c := range cases {
		if got := Shard(c.key, c.n); got != c.want {
			t.Errorf("Shard(%q, %d) = %d, want %d", c.key, c.n, got, c.want)
		}
	}
}

func TestShardRangeAndDeterminism(t *testing.T) {
	keys := shardKeys(2000)
	for n := -1; n <= 64; n++ {
		for _, k := range keys {
			got := Shard(k, n)
			if n <= 1 {
				if got != 0 {
					t.Fatalf("Shard(%q, %d) = %d, want 0", k, n, got)
				}
				continue
			}
			if got < 0 || got >= n {
				t.Fatalf("Shard(%q, %d) = %d, out of range", k, n, got)
			}
			if again := Shard(k, n); again != got {
				t.Fatalf("Shard(%q, %d) not deterministic: %d then %d", k, n, got, again)
			}
		}
	}
}

// The split is uniform: a chi-square goodness-of-fit test over a fixed set of
// 100 000 keys shaped like real broadcast keys (12 hex characters of an
// HMAC-SHA256), for n = 2…8, must not reject at p = 1e-6. The set is fixed —
// generated from a constant HMAC key, never randomly — so the test is not
// flaky, and 1e-6 is a bound a fair hash fails about once in a million key
// sets, so it was not tuned to pass on this one.
func TestShardUniformity(t *testing.T) {
	keys := shardKeys(100_000)
	for n := 2; n <= 8; n++ {
		counts := make([]float64, n)
		for _, k := range keys {
			counts[Shard(k, n)]++
		}
		expected := float64(len(keys)) / float64(n)
		chi2 := 0.0
		for _, c := range counts {
			d := c - expected
			chi2 += d * d / expected
		}
		p := chiSquareSurvival(chi2, float64(n-1))
		if p <= 1e-6 {
			t.Errorf("n=%d: chi2=%.2f, p=%.3g ≤ 1e-6 (counts %v)", n, chi2, p, counts)
		}
	}
}

// shardKeys derives n broadcast-key-shaped strings the way the relay does
// (Registry.ObfuscateID: hex of the first 6 bytes of an HMAC-SHA256), under a
// constant key so the set is fixed.
func shardKeys(n int) []string {
	out := make([]string, n)
	for i := range out {
		mac := hmac.New(sha256.New, []byte("gawk-r40-shard-fixture"))
		mac.Write([]byte("broadcast-" + strconv.Itoa(i)))
		out[i] = hex.EncodeToString(mac.Sum(nil)[:6])
	}
	return out
}

// chiSquareSurvival is P(X ≥ x) for a chi-square variable with k degrees of
// freedom: 1 − P(k/2, x/2), the regularized lower incomplete gamma by its
// power series (convergent for every x; fine at the small k used here).
func chiSquareSurvival(x, k float64) float64 {
	a, z := k/2, x/2
	if z <= 0 {
		return 1
	}
	lg, _ := math.Lgamma(a + 1)
	term := math.Exp(a*math.Log(z) - z - lg)
	sum := term
	for i := 1; i < 10000; i++ {
		term *= z / (a + float64(i))
		sum += term
		if term < sum*1e-16 {
			break
		}
	}
	return math.Max(0, 1-sum)
}

func TestChiSquareSurvivalKnownValues(t *testing.T) {
	// Critical values at p = 1e-6 from standard tables.
	for k, x := range map[float64]float64{1: 23.928, 2: 27.631, 7: 40.522} {
		if p := chiSquareSurvival(x, k); math.Abs(math.Log10(p)+6) > 0.01 {
			t.Errorf("survival(%v, %v) = %.4g, want ≈1e-6", x, k, p)
		}
	}
}

func TestValidateShard(t *testing.T) {
	for _, ok := range [][2]int{{0, 1}, {1, 2}, {63, 64}} {
		if err := ValidateShard(ok[0], ok[1]); err != nil {
			t.Errorf("ValidateShard(%d, %d) = %v", ok[0], ok[1], err)
		}
	}
	for _, bad := range [][2]int{{0, 0}, {2, 2}, {-1, 2}, {0, MaxShards + 1}} {
		if err := ValidateShard(bad[0], bad[1]); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateShard(%d, %d) = %v, want ErrInvalid", bad[0], bad[1], err)
		}
	}
}
