package eventbus

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Tuhis/gawk/gawk-server/events"
)

func runNATS(t *testing.T) string {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats-server did not start")
	}
	return srv.ClientURL()
}

// fakeIngest records what was ingested and can fail on demand, which is how
// the "do not ack what was not written" path is tested.
type fakeIngest struct {
	mu   sync.Mutex
	seen []Event
	err  error
}

func (f *fakeIngest) Ingest(_ context.Context, ev Event) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	f.seen = append(f.seen, ev)
	return true, nil
}

func (f *fakeIngest) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

func (f *fakeIngest) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.seen))
	for i, ev := range f.seen {
		out[i] = ev.Type
	}
	return out
}

// leading is newConsumer plus what the leader does first: create the stream.
// Publishing before it exists is a counted drop by design (docs/51 §6), which
// is realistic but not what these tests are about.
func leading(t *testing.T, url string, ing Ingester) *Consumer {
	t.Helper()
	c := newConsumer(t, url, ing)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.ensureStream(ctx); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	return c
}

func newConsumer(t *testing.T, url string, ing Ingester) *Consumer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := New(ctx, Options{URL: url, Ingest: ing})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// publish puts one CloudEvent on the bus the way the relay does, id and all.
func publish(t *testing.T, url, id, typ, subject string, data any) {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	body, err := events.Marshal(events.New(typ, id, events.SourceRelay("pod-a"), subject, time.Now(), data))
	if err != nil {
		t.Fatal(err)
	}
	scope := "room"
	if typ == events.TypeBroadcastStarted || typ == events.TypeBroadcastViewers {
		scope = "broadcast"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := js.PublishMsg(ctx, &nats.Msg{
		Subject: "gawk." + scope + "." + subject + "." + events.Name(typ),
		Data:    body,
		Header: nats.Header{
			"Content-Type": []string{events.ContentType},
			"Nats-Msg-Id":  []string{id},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestEnsureStreamIsIdempotent: two leaders may overlap across a handover, and
// a restart after a limit change must apply it. Creating the stream is
// therefore an update, never a failure.
//
// The stream is created by the LEADER (awaitStream, from Run), not at
// construction: a portal must start whether or not NATS is up.
func TestEnsureStreamIsIdempotent(t *testing.T) {
	url := runNATS(t)
	c := newConsumer(t, url, nil)
	ctx := context.Background()

	if c.stream != nil {
		t.Error("the stream was created before anything was elected leader")
	}
	if err := c.ensureStream(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := c.stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.MaxAge != MaxAge || info.Config.Retention != jetstream.LimitsPolicy ||
		info.Config.Discard != jetstream.DiscardOld {
		t.Errorf("stream config = %+v, want the documented limits", info.Config)
	}

	second := newConsumer(t, url, nil)
	if err := second.ensureStream(ctx); err != nil {
		t.Fatalf("a second ensure failed where it should have updated: %v", err)
	}
}

// TestConsumesAndIngestsOnce is the milestone's core claim: what the relay
// publishes reaches the store, in order, exactly once per message.
func TestConsumesAndIngestsOnce(t *testing.T) {
	url := runNATS(t)
	ing := &fakeIngest{}
	c := leading(t, url, ing)

	publish(t, url, "pod-a:1", events.TypeRoomOpened, "aa11bb22cc33",
		events.RoomOpenedData{RoomCode: "pf4tzn", RoomKey: "aa11bb22cc33", Kind: events.RoomKindDynamic})
	publish(t, url, "pod-a:2", events.TypeRoomParticipantJoined, "aa11bb22cc33",
		events.RoomParticipantJoinedData{RoomCode: "pf4tzn", RoomKey: "aa11bb22cc33", ParticipantID: 7})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := c.Run(ctx); err != nil {
			t.Error(err)
		}
	}()

	waitFor(t, "two ingested events", func() bool { return ing.count() == 2 })
	want := []string{events.TypeRoomOpened, events.TypeRoomParticipantJoined}
	got := ing.types()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ingested %v, want %v", got, want)
		}
	}

	// Per-pod liveness, the half of the bus an operator can see on /relays.
	health := c.Health(ctx)
	if health == nil || len(health.Pods) != 1 || health.Pods[0].Pod != "pod-a" {
		t.Fatalf("health = %+v", health)
	}
	if health.Pods[0].LastSeq != 2 {
		t.Errorf("lastSeq = %d, want 2", health.Pods[0].LastSeq)
	}
}

// TestDeltasAreNotStored: a viewer count is live state, not an audit row
// (docs/51 D5). It must reach the live view and never the Ingester.
func TestDeltasAreNotStored(t *testing.T) {
	url := runNATS(t)
	ing := &fakeIngest{}
	c := leading(t, url, ing)

	publish(t, url, "pod-a:1", events.TypeBroadcastViewers, "3f9a1c4e7b2d",
		events.BroadcastViewersData{BroadcastID: "k7m2q9", BroadcastKey: "3f9a1c4e7b2d",
			Role: events.RoleOrigin, ViewersLocal: 42, ViewersGlobal: 317})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	waitFor(t, "the live view to see the delta", func() bool { return len(c.Live()) == 1 })
	if ing.count() != 0 {
		t.Errorf("a delta was ingested as a row: %v", ing.types())
	}
	entry := c.Live()[events.TypeBroadcastViewers+"|3f9a1c4e7b2d"]
	if entry.Data["viewersLocal"] != float64(42) {
		t.Errorf("live entry = %+v", entry)
	}
}

// TestGapsAreCountedNotRepaired: a gap in one pod's sequence means events were
// dropped or expired. The consumer says so and moves on — reconciling is the
// consumer's job, not the bus's (docs/51 D9).
func TestGapsAreCounted(t *testing.T) {
	url := runNATS(t)
	ing := &fakeIngest{}
	c := leading(t, url, ing)

	publish(t, url, "pod-a:1", events.TypeRoomOpened, "aa11bb22cc33",
		events.RoomOpenedData{RoomCode: "pf4tzn", RoomKey: "aa11bb22cc33", Kind: events.RoomKindDynamic})
	publish(t, url, "pod-a:9", events.TypeRoomDetached, "aa11bb22cc33",
		events.RoomDetachedData{RoomCode: "pf4tzn", RoomKey: "aa11bb22cc33", BroadcastID: "k7m2q9"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	waitFor(t, "both events", func() bool { return ing.count() == 2 })
	h := c.Health(ctx)
	if h.Pods[0].Gaps != 1 {
		t.Errorf("gaps = %d, want 1", h.Pods[0].Gaps)
	}
}

// TestOffIsInert: no URL, no client, no stream, no panic.
func TestOffIsInert(t *testing.T) {
	c, err := New(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Fatal("an empty URL must yield a nil consumer")
	}
	if err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Health(context.Background()) != nil || c.Live() != nil {
		t.Error("a nil consumer reported state")
	}
	c.Close()
}

// TestDecodeRejectsNonCloudEvents: the consumer acks what it cannot parse, but
// it must not silently treat it as an event.
func TestDecodeRejectsNonCloudEvents(t *testing.T) {
	if _, err := decode([]byte(`{"hello":"world"}`)); err == nil {
		t.Error("a body with no id or type decoded as a CloudEvent")
	}
	body, err := json.Marshal(map[string]any{"id": "pod-a:1", "type": events.TypeRoomOpened, "data": 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode(body); err == nil {
		t.Error("a non-object data decoded")
	}
}

// TestAnUnreachableBusDoesNotStopThePortal: the portal's job is moderation,
// and the bus is an optional feed into it. A NATS that is down, still starting
// or refusing the credential must not keep the portal from serving — that
// would take the ban pipe down with the feed, for a subsystem that is off by
// default.
//
// The relay's publisher takes the same view for the same reason, and CI found
// this the hard way: making it fatal put the dev stack's portal in a restart
// loop the moment the bus was switched on.
func TestAnUnreachableBusDoesNotStopThePortal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := New(ctx, Options{URL: "nats://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("New against a port with no NATS on it: %v", err)
	}
	if c == nil {
		t.Fatal("no consumer")
	}
	defer c.Close()

	// It says so rather than pretending: an operator looking at /relays sees
	// that the stream is not there.
	h := c.Health(ctx)
	if h == nil || h.Error == "" {
		t.Errorf("health = %+v, want an error explaining the silence", h)
	}

	// And Run keeps trying rather than returning: it gives up only when this
	// replica stops being the leader.
	runCtx, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v; losing leadership is not a failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run did not return when leadership ended")
	}
}

// TestInsecureSkipsVerification pins what -eventbus-insecure actually does:
// it is the docs/41 compose lane's switch, it has no chart value, and it warns
// at every start — so the one thing it must not do is quietly become the
// default.
func TestInsecureSkipsVerification(t *testing.T) {
	if !insecureTLS().InsecureSkipVerify {
		t.Error("the insecure switch does not skip verification")
	}
	if (Options{}).Insecure {
		t.Error("Insecure defaults to true")
	}
}

// TestInsecureDoesNotForceTLSOnAPlainURL: the same regression as the relay's.
// nats.Secure REQUIRES TLS, so on a plain nats:// server it broke every
// handshake — and because the client retries silently, the portal's only
// symptom was a stream that never appeared and a feed that stayed empty.
func TestInsecureDoesNotForceTLSOnAPlainURL(t *testing.T) {
	url := runNATS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := New(ctx, Options{URL: url, Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	if err := c.ensureStream(ctx); err != nil {
		t.Fatalf("the leader could not create its stream against a plain server: %v", err)
	}
}

// TestWantsTLS pins which URLs the insecure switch may touch at all.
func TestWantsTLS(t *testing.T) {
	for url, want := range map[string]bool{
		"nats://nats:4222": false,
		"tls://nats:4222":  true,
		"wss://nats:443":   true,
	} {
		if got := wantsTLS(url); got != want {
			t.Errorf("wantsTLS(%q) = %v, want %v", url, got, want)
		}
	}
}
