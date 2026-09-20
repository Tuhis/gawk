package eventbus

import (
	"context"
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// runGatedNATS starts an embedded JetStream server that requires a token, and
// returns its URL plus a function that removes the requirement — the test's
// stand-in for an operator adding the grant this relay was missing.
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

// createStream makes GAWK_EVENTS the way gawk-admin does, with whatever
// credential the server currently wants.
func createStream(t *testing.T, url string, connOpts ...nats.Option) jetstream.JetStream {
	t.Helper()
	nc, err := nats.Connect(url, connOpts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "GAWK_EVENTS",
		Subjects: []string{"gawk.>"},
	}); err != nil {
		t.Fatal(err)
	}
	return js
}

func aStartedEvent() Event {
	return Event{Type: events.TypeBroadcastStarted, Key: "3f9a1c4e7b2d",
		Data: events.BroadcastStartedData{BroadcastID: "k7m2q9", BroadcastKey: "3f9a1c4e7b2d",
			Role: events.RoleOrigin}}
}

// TestRecoversFromAnAuthFailureAtStartup is the production incident, in a
// test. A relay whose NATS user did not exist yet — the pods rolled 40 seconds
// before the bus reloaded its grants — connected, was told "authorization
// violation", and never published again. nats.go aborts reconnecting after two
// identical auth errors, so the relay sat on a closed connection for as long
// as the process lived, logging a rejected publish every warn interval. Only a
// restart cleared it, and nothing but the drop counter said why.
//
// The grant arriving late is the normal case, not the exotic one: it is one
// commit and two controllers, and their order is not guaranteed.
func TestRecoversFromAnAuthFailureAtStartup(t *testing.T) {
	url, open := runGatedNATS(t)
	// No token: every connect attempt is rejected, exactly as an unknown user
	// or an unlisted certificate subject is.
	p, m := newTestPublisher(t, url, func(o *Options) {
		o.RedialInterval = 100 * time.Millisecond
	})

	p.Publish(aStartedEvent())
	// Nothing can reach a bus that refuses the credential, and that is fine:
	// the drop is counted and the media path never noticed.
	waitFor(t, func() bool { return m.drops(DropNoConn)+m.drops(DropPublish) > 0 },
		"a publish against a refused connection was never counted as a drop")

	open()
	js := createStream(t, url)

	// The relay must find its way back on its own, with no restart.
	waitFor(t, func() bool {
		p.Publish(aStartedEvent())
		return m.sent() > 0
	}, "the publisher never reconnected after the credential was accepted")

	msgs := collect(t, js, "gawk.broadcast.3f9a1c4e7b2d.started", 1)
	if len(msgs) == 0 {
		t.Fatal("no event arrived after the reconnect")
	}
}

// TestSurvivesARejectionStreakWithoutTheSupervisor is the regression guard for
// nats.IgnoreAuthErrorAbort() specifically, and it exists because the obvious
// test is not one.
//
// TestRecoversFromAnAuthFailureAtStartup passes with the option deleted: the
// gate there opens within milliseconds, which is one ReconnectWait before
// nats.go has had the SECOND identical auth error its abort needs — and even
// past that point the supervisor would re-dial and rescue it. Two safety nets
// mean neither is proven by an end-to-end recovery test.
//
// So this one removes the supervisor from the picture (an hour's re-dial
// interval), shortens the client's own retry, and does not open the gate until
// the client has been rejected several times over. What is left is exactly one
// mechanism: a client that keeps trying through repeated authorization
// failures. Without the option it gives up, the connection reaches CLOSED, and
// nothing arrives.
func TestSurvivesARejectionStreakWithoutTheSupervisor(t *testing.T) {
	url, open := runGatedNATS(t)
	p, m := newTestPublisher(t, url, func(o *Options) {
		o.RedialInterval = time.Hour
		o.ReconnectWait = 20 * time.Millisecond
	})

	// Well past two rejections — the client's abort threshold — at 20ms a try.
	time.Sleep(500 * time.Millisecond)
	if conn := p.conn.Load(); conn != nil && conn.nc.IsClosed() {
		t.Fatal("the client gave up on a bus that was only refusing it for now; " +
			"nats.IgnoreAuthErrorAbort is what keeps it trying")
	}

	open()
	js := createStream(t, url)

	waitFor(t, func() bool {
		p.Publish(aStartedEvent())
		return m.sent() > 0
	}, "the client never got in after the credential was accepted, with no supervisor to rescue it")

	if msgs := collect(t, js, "gawk.broadcast.3f9a1c4e7b2d.started", 1); len(msgs) == 0 {
		t.Fatal("no event arrived after the rejection streak ended")
	}
}

// TestRedialsAClosedConnection covers the same promise one layer down: however
// a connection ends up unusable — an auth abort, a server that closed it, a
// client that gave up — a configured bus is re-dialled rather than left dead.
// The supervisor is what makes "silently failing forever" impossible, so it is
// tested directly and not only through the auth path above.
func TestRedialsAClosedConnection(t *testing.T) {
	url, js := withStream(t)
	p, m := newTestPublisher(t, url, func(o *Options) {
		o.RedialInterval = 100 * time.Millisecond
	})

	// Kill the connection under the publisher, the way a terminal client-side
	// close does. Nothing else in the process knows this happened.
	conn := p.conn.Load()
	if conn == nil {
		t.Fatal("publisher came up with no connection")
	}
	conn.nc.Close()

	waitFor(t, func() bool {
		p.Publish(aStartedEvent())
		return m.sent() > 0
	}, "a closed connection was never re-dialled")

	if msgs := collect(t, js, "gawk.broadcast.3f9a1c4e7b2d.started", 1); len(msgs) == 0 {
		t.Fatal("no event arrived after the re-dial")
	}
}

// TestPublishWithNoConnectionIsACountedDrop: the hook still must not block or
// fail, and the reason must say what is actually wrong. "publish" would send
// an operator looking at the stream; "disconnected" points at the connection.
func TestPublishWithNoConnectionIsACountedDrop(t *testing.T) {
	url, _ := runGatedNATS(t)
	p, m := newTestPublisher(t, url, func(o *Options) {
		// Long enough that no redial can rescue this within the test.
		o.RedialInterval = time.Hour
	})

	start := time.Now()
	p.Publish(aStartedEvent())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a hook waited %v on a bus with no connection", elapsed)
	}
	waitFor(t, func() bool { return m.drops(DropNoConn) > 0 },
		"a publish with no connection was not counted as disconnected")
	if got := m.sent(); got != 0 {
		t.Errorf("counted %d publishes against a bus that has no connection", got)
	}
}

// TestOffStartsNoSupervisor: the off switch stays free. An unset URL must not
// leave a goroutine ticking every minute against nothing.
func TestOffStartsNoSupervisor(t *testing.T) {
	p, err := New(Options{RedialInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Fatal("an empty URL must yield a nil publisher")
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}
