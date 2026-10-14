// Package config holds gawk-sampler's configuration (docs/71 §5).
//
// Precedence: command-line flag > GAWK_SAMPLER_* environment variable >
// default, the contract every gawk binary keeps. Every knob lands in the
// start-up log with secrets reduced to whether they are set — the operator's
// only confirmation of what a pod actually parsed (the R2 lesson, CLAUDE.md).
package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-sampler/internal/detect"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// Config is the fully-resolved configuration of the `run` command.
type Config struct {
	Mode string

	AdminURL         string
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string // GAWK_SAMPLER_OIDC_CLIENT_SECRET only; never a flag, never logged
	OIDCScope        string
	ScreeningToken   string

	Model

	Threshold  float64
	Window     int
	WindowHits int

	Interval          time.Duration
	Workers           int
	DecodeTimeout     time.Duration
	DecodeMemoryLimit int64
	TargetsRefresh    time.Duration
	FFmpeg            string

	SamplerID     string
	ShardIndex    int
	ShardCount    int
	ReportSamples int
	SampleFloor   float64

	MetricsAddr string // empty when disabled ("off")

	LogLevel  slog.Level
	LogFormat string
}

// Model is the scoring half of the configuration, shared by `run` and
// `score`.
type Model struct {
	Path           string
	Input          int
	Labels         []string
	ONNXRuntimeLib string
}

// Rule is the D7 window rule.
func (c Config) Rule() screening.WindowRule {
	return screening.WindowRule{Threshold: c.Threshold, Size: c.Window, Hits: c.WindowHits}
}

// Env reads GAWK_SAMPLER_<name> through getenv with a default.
type Env func(key, def string) string

// NewEnv wraps getenv: an empty variable is unset.
func NewEnv(getenv func(string) string) Env {
	return func(key, def string) string {
		if v := getenv("GAWK_SAMPLER_" + key); v != "" {
			return v
		}
		return def
	}
}

// ModelFlags registers the model knobs on fs; Resolve finishes them after
// fs.Parse.
type ModelFlags struct {
	path, input, labels, lib *string
}

// RegisterModelFlags adds -model, -model-input, -labels and -onnxruntime-lib.
func RegisterModelFlags(fs *flag.FlagSet, env Env) *ModelFlags {
	return &ModelFlags{
		path:   fs.String("model", env("MODEL", "/models/nudenet.onnx"), "ONNX model path (D5: fetched at deploy time, never in the image)"),
		input:  fs.String("model-input", env("MODEL_INPUT", "320"), "square model input size: 320 or 640"),
		labels: fs.String("labels", env("LABELS", strings.Join(screening.DefaultLabels(), ",")), "comma-separated flagged labels (D7)"),
		lib:    fs.String("onnxruntime-lib", env("ONNXRUNTIME_LIB", "/usr/lib/libonnxruntime.so"), "ONNX Runtime shared library"),
	}
}

// Resolve validates the parsed model flags.
func (m *ModelFlags) Resolve() (Model, error) {
	input, err := strconv.Atoi(strings.TrimSpace(*m.input))
	if err != nil || (input != 320 && input != 640) {
		return Model{}, fmt.Errorf("invalid -model-input %q (want 320 or 640)", *m.input)
	}
	labels, err := ParseLabels(*m.labels)
	if err != nil {
		return Model{}, err
	}
	if strings.TrimSpace(*m.path) == "" {
		return Model{}, errors.New("-model is required")
	}
	return Model{Path: *m.path, Input: input, Labels: labels, ONNXRuntimeLib: *m.lib}, nil
}

// ParseLabels splits and validates a -labels value: distinct names the model
// knows.
func ParseLabels(v string) ([]string, error) {
	var labels []string
	for _, l := range strings.Split(v, ",") {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, l)
		}
	}
	if err := screening.ValidateLabels(labels); err != nil {
		return nil, fmt.Errorf("invalid -labels %q: %w", v, err)
	}
	for _, l := range labels {
		if _, ok := detect.LabelIndex(l); !ok {
			return nil, fmt.Errorf("invalid -labels: %s is not a label the detector outputs", l)
		}
	}
	return labels, nil
}

// podOrdinal is a StatefulSet pod name's trailing ordinal.
var podOrdinal = regexp.MustCompile(`-([0-9]+)$`)

// ShardFromPodName is the ordinal of a StatefulSet pod name
// ("gawk-sampler-1" ⇒ 1), or false when the name has none (D22).
func ShardFromPodName(name string) (int, bool) {
	m := podOrdinal.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// Parse resolves the `run` configuration from args (without the program name
// or subcommand) and getenv.
func Parse(args []string, getenv func(string) string) (Config, error) {
	env := NewEnv(getenv)
	fs := flag.NewFlagSet("gawk-sampler run", flag.ContinueOnError)
	mode := fs.String("mode", env("MODE", screening.ModeObserve), "observe (score and record, page no one) or flag (D19)")
	adminURL := fs.String("admin-url", env("ADMIN_URL", ""), "gawk-admin base URL, e.g. http://gawk-admin:8090 (required)")
	issuer := fs.String("oidc-issuer", env("OIDC_ISSUER", ""), "OIDC issuer for the client-credentials grant (required)")
	clientID := fs.String("oidc-client-id", env("OIDC_CLIENT_ID", ""), "OIDC client ID; its secret comes from GAWK_SAMPLER_OIDC_CLIENT_SECRET (required)")
	scope := fs.String("oidc-scope", env("OIDC_SCOPE", ""), "scope to request with the client-credentials grant; empty requests none")
	token := fs.String("screening-token", env("SCREENING_TOKEN", ""), "bearer for the relay keyframe route; prefer GAWK_SAMPLER_SCREENING_TOKEN (required)")
	model := RegisterModelFlags(fs, env)
	threshold := fs.String("threshold", env("THRESHOLD", "0.8"), "sample score at or above which a sample is a hit (D7)")
	window := fs.String("window", env("WINDOW", "6"), "samples in a broadcast's window (D7)")
	hits := fs.String("window-hits", env("WINDOW_HITS", "4"), "hits in the window that trip it (D7)")
	interval := fs.String("interval", env("INTERVAL", "5s"), "target per-broadcast sample interval (D6)")
	workers := fs.String("workers", env("WORKERS", strconv.Itoa(runtime.GOMAXPROCS(0))), "concurrent fetch+decode+infer slots")
	decodeTimeout := fs.String("decode-timeout", env("DECODE_TIMEOUT", "2s"), "per-sample ffmpeg wall clock (D4)")
	memLimit := fs.String("decode-memory-limit", env("DECODE_MEMORY_LIMIT", "1073741824"), "RLIMIT_AS for each ffmpeg, in bytes (D4)")
	refresh := fs.String("targets-refresh", env("TARGETS_REFRESH", "5s"), "how often the target list is re-read")
	ffmpeg := fs.String("ffmpeg", env("FFMPEG", "ffmpeg"), "the minimal ffmpeg (D5)")
	samplerID := fs.String("sampler-id", env("SAMPLER_ID", getenv("POD_NAME")), "samplerId in flags; defaults to $POD_NAME")
	shardIndex := fs.String("shard-index", env("SHARD_INDEX", ""), "this pod's share (D22); defaults to the ordinal of $POD_NAME")
	shardCount := fs.String("shard-count", env("SHARD_COUNT", "1"), "how many shares there are: the StatefulSet's replicas (D22)")
	reportSamples := fs.String("report-samples", env("REPORT_SAMPLES", "12"), "recent per-sample scores each tripped flag carries (D14), 1-64")
	floor := fs.String("sample-floor", env("SAMPLE_FLOOR", "0.3"), "a sample below it in every flagged label is not stored (D20)")
	metricsAddr := fs.String("metrics-addr", env("METRICS_ADDR", ":9090"), "listen address for /metrics, /healthz and /readyz; \"off\" disables it")
	logLevel := fs.String("log-level", env("LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	logFormat := fs.String("log-format", env("LOG_FORMAT", "text"), "log format: text|json")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	c := Config{
		Mode:             strings.TrimSpace(*mode),
		AdminURL:         strings.TrimRight(strings.TrimSpace(*adminURL), "/"),
		OIDCIssuer:       strings.TrimRight(strings.TrimSpace(*issuer), "/"),
		OIDCClientID:     strings.TrimSpace(*clientID),
		OIDCClientSecret: getenv("GAWK_SAMPLER_OIDC_CLIENT_SECRET"),
		OIDCScope:        strings.TrimSpace(*scope),
		ScreeningToken:   strings.TrimSpace(*token),
		FFmpeg:           strings.TrimSpace(*ffmpeg),
		SamplerID:        strings.TrimSpace(*samplerID),
		LogFormat:        strings.TrimSpace(*logFormat),
	}
	var err error
	if c.Model, err = model.Resolve(); err != nil {
		return Config{}, err
	}
	if !screening.ValidMode(c.Mode) {
		return Config{}, fmt.Errorf("invalid -mode %q (want %s or %s)", c.Mode, screening.ModeObserve, screening.ModeFlag)
	}
	if err := requireURL("-admin-url", c.AdminURL); err != nil {
		return Config{}, err
	}
	if err := requireURL("-oidc-issuer", c.OIDCIssuer); err != nil {
		return Config{}, err
	}
	switch {
	case c.OIDCClientID == "":
		return Config{}, errors.New("-oidc-client-id is required")
	case c.OIDCClientSecret == "":
		return Config{}, errors.New("GAWK_SAMPLER_OIDC_CLIENT_SECRET is required")
	case c.ScreeningToken == "":
		return Config{}, errors.New("-screening-token (GAWK_SAMPLER_SCREENING_TOKEN) is required")
	case c.FFmpeg == "":
		return Config{}, errors.New("-ffmpeg is required")
	case c.SamplerID == "":
		return Config{}, errors.New("-sampler-id is required when $POD_NAME is unset")
	}
	// The content-flag body's rule for samplerId, checked here so a bad value
	// fails at start-up rather than as a 400 on the first flag.
	if len(c.SamplerID) > screening.MaxSamplerIDLen || strings.IndexFunc(c.SamplerID, func(r rune) bool { return r <= 0x20 || r > 0x7e }) >= 0 {
		return Config{}, fmt.Errorf("invalid -sampler-id %q (want 1-%d printable ASCII characters)", c.SamplerID, screening.MaxSamplerIDLen)
	}

	if c.Threshold, err = unitFloat("-threshold", *threshold); err != nil {
		return Config{}, err
	}
	if c.Window, err = atoi("-window", *window); err != nil {
		return Config{}, err
	}
	if c.WindowHits, err = atoi("-window-hits", *hits); err != nil {
		return Config{}, err
	}
	if err := c.Rule().Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid window rule: %w", err)
	}
	if c.SampleFloor, err = unitFloat("-sample-floor", *floor); err != nil {
		return Config{}, err
	}
	// Replay refuses a threshold under the floor (D21), so a floor above the
	// running threshold would make the running configuration the one setting
	// the stored data could never replay.
	if c.SampleFloor > c.Threshold {
		return Config{}, fmt.Errorf("-sample-floor %v is above -threshold %v", c.SampleFloor, c.Threshold)
	}
	if c.Interval, err = positiveDuration("-interval", *interval, 100*time.Millisecond); err != nil {
		return Config{}, err
	}
	if c.DecodeTimeout, err = positiveDuration("-decode-timeout", *decodeTimeout, 100*time.Millisecond); err != nil {
		return Config{}, err
	}
	if c.TargetsRefresh, err = positiveDuration("-targets-refresh", *refresh, time.Second); err != nil {
		return Config{}, err
	}
	if c.Workers, err = atoi("-workers", *workers); err != nil || c.Workers < 1 {
		return Config{}, fmt.Errorf("invalid -workers %q (want at least 1)", *workers)
	}
	if c.DecodeMemoryLimit, err = strconv.ParseInt(strings.TrimSpace(*memLimit), 10, 64); err != nil || c.DecodeMemoryLimit < 64<<20 {
		return Config{}, fmt.Errorf("invalid -decode-memory-limit %q (want bytes, at least 64 MiB)", *memLimit)
	}
	if c.ReportSamples, err = atoi("-report-samples", *reportSamples); err != nil || c.ReportSamples < 1 || c.ReportSamples > screening.MaxRecent {
		return Config{}, fmt.Errorf("invalid -report-samples %q (want 1-%d)", *reportSamples, screening.MaxRecent)
	}

	if c.ShardCount, err = atoi("-shard-count", *shardCount); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(*shardIndex) != "" {
		if c.ShardIndex, err = atoi("-shard-index", *shardIndex); err != nil {
			return Config{}, err
		}
	} else {
		n, ok := ShardFromPodName(getenv("POD_NAME"))
		if !ok {
			// D22: a pod name without an ordinal must not guess a share —
			// guessing 0 on two pods would screen one share twice and the
			// other never.
			return Config{}, fmt.Errorf("-shard-index is unset and $POD_NAME %q has no ordinal", getenv("POD_NAME"))
		}
		c.ShardIndex = n
	}
	if err := screening.ValidateShard(c.ShardIndex, c.ShardCount); err != nil {
		return Config{}, fmt.Errorf("invalid shard: -shard-index %d of -shard-count %d: %w", c.ShardIndex, c.ShardCount, err)
	}

	c.MetricsAddr = strings.TrimSpace(*metricsAddr)
	if c.MetricsAddr == "off" {
		c.MetricsAddr = ""
	}
	if err := c.LogLevel.UnmarshalText([]byte(strings.TrimSpace(*logLevel))); err != nil {
		return Config{}, fmt.Errorf("invalid -log-level %q", *logLevel)
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return Config{}, fmt.Errorf("invalid -log-format %q (want text or json)", *logFormat)
	}
	return c, nil
}

func requireURL(name, v string) error {
	if v == "" {
		return fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid %s %q (want an absolute http(s) URL)", name, v)
	}
	return nil
}

func atoi(name, v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q (want an integer)", name, v)
	}
	return n, nil
}

func unitFloat(name, v string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 || f > 1 {
		return 0, fmt.Errorf("invalid %s %q (want 0-1)", name, v)
	}
	return f, nil
}

func positiveDuration(name, v string, floor time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d < floor {
		return 0, fmt.Errorf("invalid %s %q (want a duration of at least %s)", name, v, floor)
	}
	return d, nil
}

// LogAttrs is the start-up log line: every knob, secrets as set/unset.
func (c Config) LogAttrs() []any {
	set := func(v string) string {
		if v == "" {
			return "<unset>"
		}
		return "<set>"
	}
	return []any{
		"mode", c.Mode,
		"adminUrl", c.AdminURL,
		"oidcIssuer", c.OIDCIssuer,
		"oidcClientId", c.OIDCClientID,
		"oidcClientSecret", set(c.OIDCClientSecret),
		"oidcScope", c.OIDCScope,
		"screeningToken", set(c.ScreeningToken),
		"model", c.Model.Path,
		"modelInput", c.Model.Input,
		"labels", strings.Join(c.Model.Labels, ","),
		"onnxruntimeLib", c.Model.ONNXRuntimeLib,
		"threshold", c.Threshold,
		"window", c.Window,
		"windowHits", c.WindowHits,
		"interval", c.Interval.String(),
		"workers", c.Workers,
		"decodeTimeout", c.DecodeTimeout.String(),
		"decodeMemoryLimit", c.DecodeMemoryLimit,
		"targetsRefresh", c.TargetsRefresh.String(),
		"ffmpeg", c.FFmpeg,
		"samplerId", c.SamplerID,
		"shardIndex", c.ShardIndex,
		"shardCount", c.ShardCount,
		"reportSamples", c.ReportSamples,
		"sampleFloor", c.SampleFloor,
		"metricsAddr", c.MetricsAddr,
		"logLevel", c.LogLevel.String(),
		"logFormat", c.LogFormat,
	}
}
