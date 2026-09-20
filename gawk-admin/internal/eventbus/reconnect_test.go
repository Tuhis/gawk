package eventbus

import (
	"context"
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// runGatedNATS starts an embedded JetStream server that requires a token and
// returns its URL plus a function that drops the requirement — the portal's
// side of the same incident the relay's reconnect tests cover: a NATS whose
// grant for this workload arrives after the workload does.
func runGatedNATS(t *testing.T) (url string, open func()) {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	opts.Authorization = "let-me-in"
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats-server did not start")
	}
	return srv.ClientURL(), func() {
		t.Helper()
		open := opts
		open.Authorization = ""
		if err := srv.ReloadOptions(&open); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRecoversFromAnAuthFailureAtStartup: the portal that came up before its
// NATS user existed must find its way to the stream on its own. nats.go stops
// reconnecting after two identical auth errors, so without IgnoreAuthErrorAbort
// and a re-dial the consumer retried the stream forever against a connection
// that was already dead — the feed stayed empty until someone restarted the
// pod, and the only symptom was a quiet Events view.
func TestRecoversFromAnAuthFailureAtStartup(t *testing.T) {
	url, open := runGatedNATS(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ing := &fakeIngest{}
	c, err := New(ctx, Options{
		URL: url, Ingest: ing, ManageStream: true,
		RedialInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// The grant lands a moment later, as it did in production.
	open()
	waitFor(t, "the consumer to reach the stream after the credential was accepted",
		func() bool { return c.Health(ctx) != nil && c.Health(ctx).Error == "" })

	publish(t, url, "pod-a:1", events.TypeBroadcastStarted, "3f9a1c4e7b2d", startedData())
	waitFor(t, "an event to be ingested after the consumer recovered",
		func() bool { return ing.count() == 1 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v; losing leadership is not a failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run did not return when leadership ended")
	}
}

// TestResumesAfterTheConnectionDies covers the other half: a consumer that was
// working and whose connection then ended. Consume stops delivering and says
// nothing, so without supervision the leader holds the durable and ingests
// nothing — the worst shape this can take, because the portal looks healthy.
func TestResumesAfterTheConnectionDies(t *testing.T) {
	url := runNATS(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ing := &fakeIngest{}
	c, err := New(ctx, Options{
		URL: url, Ingest: ing, ManageStream: true,
		RedialInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go func() { _ = c.Run(ctx) }()

	publish(t, url, "pod-a:1", events.TypeBroadcastStarted, "3f9a1c4e7b2d", startedData())
	waitFor(t, "the first event to be ingested", func() bool { return ing.count() == 1 })

	// Kill it under the consumer. Nothing tells the portal this happened.
	c.conn().Close()

	publish(t, url, "pod-a:2", events.TypeBroadcastStarted, "3f9a1c4e7b2d", startedData())
	waitFor(t, "the consumer to resume after its connection died",
		func() bool { return ing.count() == 2 })
}

// startedData is the payload every event in these tests carries; the tests are
// about the connection, not the contract.
func startedData() events.BroadcastStartedData {
	return events.BroadcastStartedData{BroadcastID: "k7m2q9", BroadcastKey: "3f9a1c4e7b2d",
		Role: events.RoleOrigin}
}
