package popclient

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// A mint request home never answers — the link dropped after it was
// written, or home failed silently — must not stop the pool asking: once
// the request expires, the next draw asks again (review of #563).
func TestAnUnansweredMintIsAskedAgain(t *testing.T) {
	c := New(Options{Cfg: config.Config{ServerName: "relay"}})
	c.setState(func(s *LinkState) {
		s.Header = HeaderLinked
		s.Config = &poplink.Config{Telemetry: &poplink.TelemetryConfig{ReportIntervalMs: 2000}}
	})
	var mu sync.Mutex
	var mints int
	c.tel.send = func(rec any) error {
		if _, ok := rec.(poplink.TelemetryMint); ok {
			mu.Lock()
			mints++
			mu.Unlock()
		}
		return nil // written, and never answered
	}
	ctx, cancel := context.WithTimeout(context.Background(), telemetryAskExpiry+2*telemetryRetry)
	defer cancel()
	if _, _, ok := c.TelemetryHello(ctx, "ABC234", wire.TelemetryRoleViewer); ok {
		t.Fatal("a hello with no tokens ever minted")
	}
	mu.Lock()
	defer mu.Unlock()
	if mints < 2 {
		t.Fatalf("mint requests = %d, want a second once the first went unanswered for %v", mints, telemetryAskExpiry)
	}
}

// A malformed answer still settles the request, so the pool asks again.
func TestAMalformedAnswerSettlesTheRequest(t *testing.T) {
	c := New(Options{Cfg: config.Config{ServerName: "relay"}})
	tel := c.tel
	tel.mu.Lock()
	p := &telemetryPool{asked: true, askedAt: time.Now(), ready: make(chan struct{}), used: time.Now()}
	tel.pools[poolKey("ABC234", wire.TelemetryRoleViewer)] = p
	tel.mu.Unlock()
	tel.tokens(&poplink.TelemetryTokens{ID: "ABC234", Role: "viewer", BroadcastKey: "not-hex"})
	tel.mu.Lock()
	defer tel.mu.Unlock()
	if p.asked {
		t.Fatal("a malformed TelemetryTokens left the request outstanding")
	}
}
