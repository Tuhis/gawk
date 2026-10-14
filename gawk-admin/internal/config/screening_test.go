package config

import (
	"strings"
	"testing"
	"time"
)

// R40's knobs (docs/71 §5): defaults, env and flag, and the startup log.
func TestScreeningKnobDefaults(t *testing.T) {
	cfg, err := ParseFlags(nil, envFrom(minimal()))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Screening {
		t.Error("Screening = true by default, want off")
	}
	if cfg.ContentFlagRepage != 10*time.Minute || cfg.ContentFlagMute != 30*time.Minute || cfg.ContentFlagStaleAfter != 2*time.Minute {
		t.Errorf("repage/mute/stale = %v/%v/%v, want 10m/30m/2m", cfg.ContentFlagRepage, cfg.ContentFlagMute, cfg.ContentFlagStaleAfter)
	}
	// Both automated kills are built and OFF by default (OD1, OD2).
	if cfg.ContentFlagAutoKill != nil || cfg.ContentFlagDeadline != 0 {
		t.Errorf("auto-kill/deadline = %v/%v, want both off", cfg.ContentFlagAutoKill, cfg.ContentFlagDeadline)
	}
	if cfg.ScreeningSampleRetention != 30*24*time.Hour {
		t.Errorf("sample retention = %v, want 30d", cfg.ScreeningSampleRetention)
	}
	attrs := strings.Join(asStrings(cfg.LogAttrs()), " ")
	for _, want := range []string{"screening false", "contentFlagRepage 10m0s", "contentFlagMute 30m0s",
		"contentFlagStaleAfter 2m0s", "contentFlagAutoKill off", "contentFlagDeadline off",
		"screeningSampleRetention 720h0m0s", "flaggerRole flagger"} {
		if !strings.Contains(attrs, want) {
			t.Errorf("LogAttrs lacks %q: %s", want, attrs)
		}
	}
}

func TestScreeningKnobsFromEnvAndFlags(t *testing.T) {
	env := minimal()
	env["GAWK_ADMIN_SCREENING"] = "true"
	env["GAWK_ADMIN_CONTENT_FLAG_REPAGE"] = "5m"
	env["GAWK_ADMIN_CONTENT_FLAG_MUTE"] = "1h"
	env["GAWK_ADMIN_CONTENT_FLAG_STALE_AFTER"] = "90s"
	env["GAWK_ADMIN_CONTENT_FLAG_AUTO_KILL"] = "score>=0.9,hits>=10/12"
	env["GAWK_ADMIN_CONTENT_FLAG_DEADLINE"] = "30m"
	env["GAWK_ADMIN_SCREENING_SAMPLE_RETENTION"] = "7d"
	cfg, err := ParseFlags(nil, envFrom(env))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if !cfg.Screening || cfg.ContentFlagRepage != 5*time.Minute || cfg.ContentFlagMute != time.Hour ||
		cfg.ContentFlagStaleAfter != 90*time.Second || cfg.ContentFlagDeadline != 30*time.Minute ||
		cfg.ScreeningSampleRetention != 7*24*time.Hour {
		t.Errorf("env did not reach the config: %+v", cfg)
	}
	if p := cfg.ContentFlagAutoKill; p == nil || p.Score != 0.9 || p.Hits != 10 || p.Of != 12 || p.String() != "score>=0.9,hits>=10/12" {
		t.Errorf("auto-kill = %v", cfg.ContentFlagAutoKill)
	}
	cfg, err = ParseFlags([]string{"-screening", "false", "-content-flag-deadline", "off", "-content-flag-auto-kill", "off"}, envFrom(env))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Screening || cfg.ContentFlagDeadline != 0 || cfg.ContentFlagAutoKill != nil {
		t.Errorf("flags did not beat the env: %+v", cfg)
	}
}

func TestScreeningKnobRejections(t *testing.T) {
	for _, args := range [][]string{
		{"-screening", "sometimes"},
		{"-content-flag-repage", "0s"},
		{"-content-flag-mute", "soon"},
		{"-content-flag-stale-after", "-1m"},
		{"-content-flag-deadline", "never"},
		{"-content-flag-auto-kill", "score>=0.9"},
		{"-content-flag-auto-kill", "hits>=10/12"},
		{"-content-flag-auto-kill", "score>=1.5,hits>=10/12"},
		{"-content-flag-auto-kill", "score>=0.9,hits>=13/12"},
		{"-content-flag-auto-kill", "score>=0.9,hits>=0/12"},
		{"-content-flag-auto-kill", "score>=0.9,hits>=10/12,viewers>=3"},
		// More samples than a POST may carry would never match: refuse to
		// start rather than let an operator believe it armed (D14).
		{"-content-flag-auto-kill", "score>=0.9,hits>=60/65"},
		{"-screening-sample-retention", "0d"},
		{"-screening-sample-retention", "a month"},
	} {
		if _, err := ParseFlags(args, envFrom(minimal())); err == nil {
			t.Errorf("ParseFlags(%v) succeeded, want an error", args)
		}
	}
}

// D14: a policy needs a full window of recent scores, counts only the last
// Of of them, and never matches below its hit count.
func TestAutoKillPolicyMatch(t *testing.T) {
	p, err := ParseAutoKillPolicy("score>=0.9, hits>=10/12")
	if err != nil {
		t.Fatal(err)
	}
	hi, lo := 0.95, 0.5
	series := func(hits, total int) []float64 {
		out := make([]float64, 0, total)
		for i := 0; i < total; i++ {
			if i < total-hits {
				out = append(out, lo)
			} else {
				out = append(out, hi)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		recent []float64
		want   bool
	}{
		{"10 of 12", series(10, 12), true},
		{"12 of 12", series(12, 12), true},
		{"9 of 12", series(9, 12), false},
		{"fewer than 12 samples (new session, restarted sampler)", series(11, 11), false},
		{"exactly the threshold counts", append(series(9, 11), 0.9), true},
		{"only the last 12 count", append(series(10, 12), lo, lo, lo), false},
		{"empty", nil, false},
	} {
		if got := p.Match(tc.recent); got != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got, tc.want)
		}
	}
	var off *AutoKillPolicy
	if off.Match(series(12, 12)) || off.String() != "off" {
		t.Error("a nil policy matched or did not read as off")
	}
}

// R70's knobs (docs/72 §4): the critical level, the sustained run and the
// rejoin window — defaults, env, flags, refusals and the startup log.
func TestTimelineKnobs(t *testing.T) {
	cfg, err := ParseFlags(nil, envFrom(minimal()))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.ScreeningCritical != 0.7 || cfg.ScreeningSustainedAfter != 3*time.Minute || cfg.ContentFlagRejoin != 10*time.Minute {
		t.Errorf("critical/sustained/rejoin = %v/%v/%v, want 0.7/3m/10m", cfg.ScreeningCritical, cfg.ScreeningSustainedAfter, cfg.ContentFlagRejoin)
	}
	attrs := strings.Join(asStrings(cfg.LogAttrs()), " ")
	for _, want := range []string{"screeningCritical 0.7", "screeningSustainedAfter 3m0s", "contentFlagRejoin 10m0s"} {
		if !strings.Contains(attrs, want) {
			t.Errorf("LogAttrs lacks %q: %s", want, attrs)
		}
	}

	env := minimal()
	env["GAWK_ADMIN_SCREENING_CRITICAL"] = "0.85"
	env["GAWK_ADMIN_SCREENING_SUSTAINED_AFTER"] = "90s"
	env["GAWK_ADMIN_CONTENT_FLAG_REJOIN"] = "off"
	cfg, err = ParseFlags(nil, envFrom(env))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.ScreeningCritical != 0.85 || cfg.ScreeningSustainedAfter != 90*time.Second || cfg.ContentFlagRejoin != 0 {
		t.Errorf("env did not reach the config: %v/%v/%v", cfg.ScreeningCritical, cfg.ScreeningSustainedAfter, cfg.ContentFlagRejoin)
	}
	if attrs := strings.Join(asStrings(cfg.LogAttrs()), " "); !strings.Contains(attrs, "contentFlagRejoin off") {
		t.Errorf("LogAttrs lacks contentFlagRejoin off: %s", attrs)
	}
	cfg, err = ParseFlags([]string{"-screening-critical", "1", "-content-flag-rejoin", "5m"}, envFrom(env))
	if err != nil || cfg.ScreeningCritical != 1 || cfg.ContentFlagRejoin != 5*time.Minute {
		t.Errorf("flags did not beat the env: %v, %v/%v", err, cfg.ScreeningCritical, cfg.ContentFlagRejoin)
	}

	for _, args := range [][]string{
		{"-screening-critical", "0"},
		{"-screening-critical", "1.2"},
		{"-screening-critical", "high"},
		{"-screening-sustained-after", "0s"},
		{"-screening-sustained-after", "-1m"},
		{"-content-flag-rejoin", "-5m"},
		{"-content-flag-rejoin", "later"},
	} {
		if _, err := ParseFlags(args, envFrom(minimal())); err == nil {
			t.Errorf("ParseFlags(%v) succeeded, want an error", args)
		}
	}
}
