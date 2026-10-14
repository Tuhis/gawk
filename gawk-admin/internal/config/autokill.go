package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-server/screening"
)

// AutoKillPolicy is R40's auto-kill tier (docs/71 D14, OD1): kill when at
// least Hits of a flag's last Of per-sample scores are at or above Score.
// Off by default; nil means off.
//
// It is evaluated by gawk-admin over the `recent` scores a tripped POST
// carries — never by the sampler, whose `flagger` role is flag-only (docs/42
// D17) — and it needs a full Of of them: a new publisher session or a
// restarted sampler starts `recent` empty, so a restart can delay a kill and
// never cause one.
type AutoKillPolicy struct {
	Score float64
	Hits  int
	Of    int
}

// ParseAutoKillPolicy reads `off` (or empty) or a policy such as
// `score>=0.9,hits>=10/12`. The score may be anything in (0, 1] — the
// sampler's storage floor does not apply, because `recent` is not filtered
// by it — and Of may not exceed what a POST can carry (screening.MaxRecent),
// or the policy could never match and the operator would believe it armed.
func ParseAutoKillPolicy(raw string) (*AutoKillPolicy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "off" {
		return nil, nil
	}
	var (
		p                AutoKillPolicy
		haveScore, haveN bool
	)
	for _, part := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), ">=")
		if !ok {
			return nil, fmt.Errorf("auto-kill policy %q: %q is not key>=value", raw, part)
		}
		switch strings.TrimSpace(key) {
		case "score":
			v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || v <= 0 || v > 1 {
				return nil, fmt.Errorf("auto-kill policy %q: score must be in (0, 1]", raw)
			}
			p.Score, haveScore = v, true
		case "hits":
			hits, of, ok := strings.Cut(strings.TrimSpace(value), "/")
			h, err1 := strconv.Atoi(strings.TrimSpace(hits))
			n, err2 := strconv.Atoi(strings.TrimSpace(of))
			if !ok || err1 != nil || err2 != nil || h < 1 || n < h {
				return nil, fmt.Errorf("auto-kill policy %q: hits must be N/M with 1 <= N <= M", raw)
			}
			if n > screening.MaxRecent {
				return nil, fmt.Errorf("auto-kill policy %q: %d samples is more than a content flag carries (%d)", raw, n, screening.MaxRecent)
			}
			p.Hits, p.Of, haveN = h, n, true
		default:
			return nil, fmt.Errorf("auto-kill policy %q: unknown key %q (want score and hits)", raw, key)
		}
	}
	if !haveScore || !haveN {
		return nil, fmt.Errorf("auto-kill policy %q: both score>= and hits>= are required", raw)
	}
	return &p, nil
}

// String renders the policy as it is parsed, for the startup log and the
// audit trail.
func (p *AutoKillPolicy) String() string {
	if p == nil {
		return "off"
	}
	return fmt.Sprintf("score>=%s,hits>=%d/%d", strconv.FormatFloat(p.Score, 'f', -1, 64), p.Hits, p.Of)
}

// Match reports whether recent — oldest first — holds at least Of scores and
// at least Hits of its last Of are at or above Score. A nil policy never
// matches.
func (p *AutoKillPolicy) Match(recent []float64) bool {
	if p == nil || len(recent) < p.Of {
		return false
	}
	hits := 0
	for _, s := range recent[len(recent)-p.Of:] {
		if s >= p.Score {
			hits++
		}
	}
	return hits >= p.Hits
}

// parseRetention reads a duration that may also be written in whole days
// ("30d"), which is how a retention is naturally said and how docs/71 D17
// writes the default. Go's duration syntax stops at hours.
func parseRetention(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%q is not a positive number of days", raw)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q is not a positive duration", raw)
	}
	return d, nil
}
