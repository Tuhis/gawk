package config

import (
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func baseEnv() map[string]string {
	return map[string]string{
		"POD_NAME":                        "gawk-sampler-1",
		"GAWK_SAMPLER_ADMIN_URL":          "http://gawk-admin:8090/",
		"GAWK_SAMPLER_OIDC_ISSUER":        "https://id.example/realms/r/",
		"GAWK_SAMPLER_OIDC_CLIENT_ID":     "gawk-sampler",
		"GAWK_SAMPLER_OIDC_CLIENT_SECRET": "s3cret",
		"GAWK_SAMPLER_SCREENING_TOKEN":    "tok",
		"GAWK_SAMPLER_SHARD_COUNT":        "2",
	}
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse(nil, envOf(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != "observe" || c.Threshold != 0.8 || c.Window != 6 || c.WindowHits != 4 ||
		c.Interval != 5*time.Second || c.DecodeTimeout != 2*time.Second || c.TargetsRefresh != 5*time.Second ||
		c.ReportSamples != 12 || c.SampleFloor != 0.3 || c.MetricsAddr != ":9090" || c.Model.Input != 320 ||
		c.Model.Path != "/models/nudenet.onnx" || c.FFmpeg != "ffmpeg" {
		t.Fatalf("defaults %+v", c)
	}
	if c.AdminURL != "http://gawk-admin:8090" || c.OIDCIssuer != "https://id.example/realms/r" {
		t.Fatalf("trailing slashes kept: %q %q", c.AdminURL, c.OIDCIssuer)
	}
	if strings.Join(c.Model.Labels, ",") != "FEMALE_GENITALIA_EXPOSED,MALE_GENITALIA_EXPOSED,ANUS_EXPOSED" {
		t.Fatalf("labels %v", c.Model.Labels)
	}
	// D22: the shard comes from the StatefulSet pod name, and so does the
	// sampler ID.
	if c.ShardIndex != 1 || c.ShardCount != 2 || c.SamplerID != "gawk-sampler-1" {
		t.Fatalf("shard %d of %d, id %q", c.ShardIndex, c.ShardCount, c.SamplerID)
	}
}

// Flag beats env beats default.
func TestParsePrecedence(t *testing.T) {
	env := baseEnv()
	env["GAWK_SAMPLER_THRESHOLD"] = "0.7"
	env["GAWK_SAMPLER_MODE"] = "flag"
	c, err := Parse([]string{"-threshold", "0.9"}, envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.Threshold != 0.9 || c.Mode != "flag" {
		t.Fatalf("threshold %v mode %s", c.Threshold, c.Mode)
	}
}

func TestParseShards(t *testing.T) {
	// No ordinal and no explicit index: refuse rather than guess.
	env := baseEnv()
	env["POD_NAME"] = "gawk-sampler"
	if _, err := Parse(nil, envOf(env)); err == nil || !strings.Contains(err.Error(), "ordinal") {
		t.Fatalf("err = %v, want a refusal naming the ordinal", err)
	}
	// An explicit index works without one.
	if c, err := Parse([]string{"-shard-index", "0"}, envOf(env)); err != nil || c.ShardIndex != 0 {
		t.Fatalf("explicit index: %v %+v", err, c)
	}
	// An index at or past the count is refused.
	env = baseEnv()
	env["POD_NAME"] = "gawk-sampler-2"
	if _, err := Parse(nil, envOf(env)); err == nil {
		t.Fatal("gawk-sampler-2 of 2 started")
	}
	if _, err := Parse([]string{"-shard-index", "1", "-shard-count", "1"}, envOf(baseEnv())); err == nil {
		t.Fatal("index 1 of 1 started")
	}
}

func TestShardFromPodName(t *testing.T) {
	for name, want := range map[string]int{"gawk-sampler-0": 0, "gawk-sampler-1": 1, "x-12": 12} {
		if got, ok := ShardFromPodName(name); !ok || got != want {
			t.Errorf("%s: %d %v", name, got, ok)
		}
	}
	for _, name := range []string{"", "gawk-sampler", "gawk-sampler-", "gawk-sampler-1a"} {
		if _, ok := ShardFromPodName(name); ok {
			t.Errorf("%q has an ordinal", name)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]struct {
		args []string
		env  func(map[string]string)
	}{
		"unknown mode":         {[]string{"-mode", "page"}, nil},
		"no admin url":         {nil, func(e map[string]string) { delete(e, "GAWK_SAMPLER_ADMIN_URL") }},
		"relative admin url":   {[]string{"-admin-url", "gawk-admin:8090"}, nil},
		"no secret":            {nil, func(e map[string]string) { delete(e, "GAWK_SAMPLER_OIDC_CLIENT_SECRET") }},
		"no screening token":   {nil, func(e map[string]string) { delete(e, "GAWK_SAMPLER_SCREENING_TOKEN") }},
		"unknown label":        {[]string{"-labels", "FACE_FEMALE,NOT_A_LABEL"}, nil},
		"empty labels":         {[]string{"-labels", " , "}, nil},
		"input 416":            {[]string{"-model-input", "416"}, nil},
		"hits over window":     {[]string{"-window", "3", "-window-hits", "4"}, nil},
		"threshold above 1":    {[]string{"-threshold", "1.5"}, nil},
		"floor above thresh":   {[]string{"-sample-floor", "0.9"}, nil},
		"report samples 65":    {[]string{"-report-samples", "65"}, nil},
		"report samples 0":     {[]string{"-report-samples", "0"}, nil},
		"zero workers":         {[]string{"-workers", "0"}, nil},
		"tiny memory limit":    {[]string{"-decode-memory-limit", "1000"}, nil},
		"bad interval":         {[]string{"-interval", "5"}, nil},
		"sampler id with tab":  {[]string{"-sampler-id", "a\tb"}, nil},
		"stray argument":       {[]string{"extra"}, nil},
		"bad log format":       {[]string{"-log-format", "xml"}, nil},
		"secret as a flag too": {[]string{"-oidc-client-secret", "x"}, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := baseEnv()
			if c.env != nil {
				c.env(env)
			}
			if _, err := Parse(c.args, envOf(env)); err == nil {
				t.Fatal("parsed")
			}
		})
	}
}

// The start-up log names every knob and never a secret.
func TestLogAttrsHideSecrets(t *testing.T) {
	c, err := Parse(nil, envOf(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	attrs := c.LogAttrs()
	var keys []string
	for i := 0; i < len(attrs); i += 2 {
		keys = append(keys, attrs[i].(string))
		if s, ok := attrs[i+1].(string); ok && (s == "s3cret" || s == "tok") {
			t.Fatalf("%s logged in the clear", attrs[i])
		}
	}
	for _, want := range []string{"mode", "threshold", "window", "windowHits", "interval", "workers", "shardIndex", "shardCount", "sampleFloor", "reportSamples", "labels", "modelInput", "decodeMemoryLimit", "screeningToken", "oidcClientSecret"} {
		found := false
		for _, k := range keys {
			found = found || k == want
		}
		if !found {
			t.Errorf("start-up log lacks %s", want)
		}
	}
}
