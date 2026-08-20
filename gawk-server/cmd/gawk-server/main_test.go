package main

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
)

// R2 review finding F1: the hardening limits parsed by config.ParseFlags must
// actually reach hub.Options in production. The original R2 change wired them
// only into the transport test helper, so -max-bandwidth was a no-op and
// -max-broadcasts / -max-total-subscribers overrides were silently ignored.
func TestRegistryOptionsCarryAllLimits(t *testing.T) {
	// Every value here is deliberately non-zero. A bool left at its zero value
	// proves nothing about plumbing — it matches whether the assignment exists
	// or not — which is the same blind spot F1 was, one type down.
	cfg := config.Config{
		MaxSubscribers:                7,
		BroadcastGrace:                42 * time.Second,
		MaxBroadcasts:                 9,
		MaxTotalSubscribers:           33,
		MaxBandwidthBytes:             1250000,
		DVRAudio:                      true,
		LiveEdgeAudioOnReliableStream: true,
	}
	want := hub.Options{
		MaxSubscribers:                7,
		BroadcastGrace:                42 * time.Second,
		MaxBroadcasts:                 9,
		MaxTotalSubscribers:           33,
		MaxBandwidthBytes:             1250000,
		DVRAudio:                      true,
		LiveEdgeAudioOnReliableStream: true,
	}
	// DeepEqual, not ==: Options grew func-typed cluster hooks in R17 W3
	// (nil here on both sides — registryOptions never sets them; main wires
	// them separately when -cluster-mode is on).
	if got := registryOptions(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("registryOptions(cfg) = %+v, want %+v", got, want)
	}
}

// R39 AP2 (docs/42 §9): the startup log must state which ban source this pod
// is enforcing from — the operator's only confirmation surface for a knob
// that otherwise shows up nowhere.
func TestStartupLogStatesTheModerationSource(t *testing.T) {
	for _, source := range []string{"off", "k8s", "file:/etc/gawk/bans.json"} {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		logStartup(log, config.Config{ModerationSource: source}, "test")
		if got := buf.String(); !strings.Contains(got, "moderation_source="+source) {
			t.Errorf("startup log for source %q does not state it:\n%s", source, got)
		}
	}
}
