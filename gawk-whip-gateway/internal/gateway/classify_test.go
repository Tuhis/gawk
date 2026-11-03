package gateway

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/publisher"
	"github.com/Tuhis/gawk/gawk-server/wire"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
)

// The publisher engine reports a terminal relay close as a *CloseError
// (resume.go closeCodeError). This pins classifyEngineEnd against the real
// engine, so a change there fails here instead of silently turning a 4006
// into relay_busy or a 4004 into an end the gateway records.
func TestEngineCloseCodesClassify(t *testing.T) {
	cases := []struct {
		code       uint32
		reason     string
		superseded bool
	}{
		{wire.CloseCodePublisherSuperseded, "", true},
		{wire.CloseCodeTerminatedByOperator, status.ReasonKilled, false},
		{wire.CloseCodeBroadcastEnded, "", false},
	}
	for _, c := range cases {
		relay := newFakeRelay()
		errc := make(chan error, 1)
		ended := make(chan struct{})
		src := newSource(false)
		eng := publisher.New(
			publisher.Config{RelayURL: "https://relay.test", BroadcastID: "K7Q2XM", ResumeToken: "00"},
			publisher.Callbacks{OnError: func(err error) { errc <- err }, OnEnded: func() { close(ended) }},
			publisher.Options{Dial: relay.dial, MediaFactory: src.factory},
		)
		if err := eng.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		relay.cur("K7Q2XM").remoteClose(c.code)
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatalf("code %d: the engine did not end", c.code)
		}
		var err error
		select {
		case err = <-errc:
		default:
		}
		reason, superseded := classifyEngineEnd(err)
		if reason != c.reason || superseded != c.superseded {
			t.Errorf("code %d (%v): classified %q superseded=%v, want %q %v", c.code, err, reason, superseded, c.reason, c.superseded)
		}
		_ = eng.Stop()
	}

	// A resume that the relay refuses for good carries its status.
	if r, _ := classifyEngineEnd(&publisher.StartError{Phase: publisher.PhaseConnect, Status: 451}); r != status.ReasonBanned {
		t.Fatalf("451 resume: %q", r)
	}
}

func TestEngineLogsCarryNoBroadcastID(t *testing.T) {
	var buf strings.Builder
	log := slog.New(scrubIDs{slog.NewTextHandler(&buf, nil)})
	log.Info("broadcast resumed", "broadcast_id", "K7Q2XM", "attempt", 1)
	log.With("broadcast_id", "K7Q2XM").Info("x")
	if strings.Contains(buf.String(), "K7Q2XM") || !strings.Contains(buf.String(), "attempt=1") {
		t.Fatalf("log: %s", buf.String())
	}
}
