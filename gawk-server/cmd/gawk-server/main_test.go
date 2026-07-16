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
	cfg := config.Config{
		MaxSubscribers:      7,
		BroadcastGrace:      42 * time.Second,
		MaxBroadcasts:       9,
		MaxTotalSubscribers: 33,
		MaxBandwidthBytes:   1250000,
	}
	want := hub.Options{
		MaxSubscribers:      7,
		BroadcastGrace:      42 * time.Second,
		MaxBroadcasts:       9,
		MaxTotalSubscribers: 33,
		MaxBandwidthBytes:   1250000,
	}
	// DeepEqual, not ==: Options grew func-typed cluster hooks in R17 W3
	// (nil here on both sides — registryOptions never sets them; main wires
	// them separately when -cluster-mode is on).
	if got := registryOptions(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("registryOptions(cfg) = %+v, want %+v", got, want)
	}
}
