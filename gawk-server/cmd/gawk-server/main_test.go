package main

import (
	"reflect"
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
