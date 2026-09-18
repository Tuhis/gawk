package eventbus

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// runNATS starts an embedded JetStream server for the test and returns its URL.
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

// withStream starts a server and creates GAWK_EVENTS, the way gawk-admin does.
func withStream(t *testing.T) (url string, js jetstream.JetStream) {
	t.Helper()
	url = runNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err = jetstream.New(nc)
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
	return url, js
}

// countingMetrics is the test's Metrics. internal/metrics implements the same
// interface for production, but this package must not import it: metrics
// imports hub and roomsrv, which hand THIS package their events.
type countingMetrics struct {
	mu        sync.Mutex
	published int
	dropped   map[string]int
}

func newCountingMetrics() *countingMetrics {
	return &countingMetrics{dropped: map[string]int{}}
}

func (m *countingMetrics) Published() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published++
}

func (m *countingMetrics) Dropped(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropped[reason]++
}

func (m *countingMetrics) drops(reason string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dropped[reason]
}

func (m *countingMetrics) sent() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.published
}

func newTestPublisher(t *testing.T, url string, tweak func(*Options)) (*Publisher, *countingMetrics) {
	t.Helper()
	m := newCountingMetrics()
	opts := Options{URL: url, Pod: "pod-a", Metrics: m, ViewerInterval: 50 * time.Millisecond}
	if tweak != nil {
		tweak(&opts)
	}
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, m
}

// TestPublishesCloudEvents is the shape check a consumer depends on: the
// subject carries the HMAC'd key and nothing else, the headers say structured
// CloudEvents and repeat the body's id as the dedup key, and the body is
// exactly what events.Marshal produces — this package adds no encoding.
func TestPublishesCloudEvents(t *testing.T) {
	url, js := withStream(t)
	p, m := newTestPublisher(t, url, nil)

	p.Publish(Event{
		Type: events.TypeRoomParticipantJoined,
		Key:  "aa11bb22cc33",
		Time: time.Date(2026, 9, 18, 9, 30, 0, 0, time.UTC),
		Data: events.RoomParticipantJoinedData{RoomCode: "pf4tzn", RoomKey: "aa11bb22cc33",
			ParticipantID: 7, Nickname: "tuhis", ClientKind: events.ClientKindWebBroadcaster},
	})

	msg := firstMessage(t, js, "gawk.room.aa11bb22cc33.participant_joined")
	if got := msg.Headers().Get("Content-Type"); got != events.ContentType {
		t.Errorf("Content-Type %q, want %q", got, events.ContentType)
	}
	var ce map[string]any
	if err := json.Unmarshal(msg.Data(), &ce); err != nil {
		t.Fatal(err)
	}
	if got := msg.Headers().Get("Nats-Msg-Id"); got != ce["id"] {
		t.Errorf("Nats-Msg-Id %q != body id %v", got, ce["id"])
	}
	if ce["subject"] != "aa11bb22cc33" {
		t.Errorf("subject %v, want the HMAC'd key", ce["subject"])
	}
	if ce["source"] != "/gawk/relay/pod-a" {
		t.Errorf("source %v", ce["source"])
	}
	// The body is byte-identical to the contract package's encoding of the
	// same event, with the id the publisher assigned.
	want, err := events.Marshal(events.New(events.TypeRoomParticipantJoined,
		ce["id"].(string), events.SourceRelay("pod-a"), "aa11bb22cc33",
		time.Date(2026, 9, 18, 9, 30, 0, 0, time.UTC),
		events.RoomParticipantJoinedData{RoomCode: "pf4tzn", RoomKey: "aa11bb22cc33",
			ParticipantID: 7, Nickname: "tuhis", ClientKind: events.ClientKindWebBroadcaster}))
	if err != nil {
		t.Fatal(err)
	}
	if string(msg.Data()) != string(want) {
		t.Errorf("body\n got: %s\nwant: %s", msg.Data(), want)
	}
	if n := m.sent(); n != 1 {
		t.Errorf("published = %d, want 1", n)
	}
}

// TestSequenceIsMonotonicPerPod: (pod, seq) is the dedup key, so the ids a pod
// issues must be ordered and gap-free within its own run.
func TestSequenceIsMonotonicPerPod(t *testing.T) {
	url, js := withStream(t)
	p, _ := newTestPublisher(t, url, nil)
	for i := 0; i < 5; i++ {
		p.Publish(Event{Type: events.TypeBroadcastStarted, Key: "3f9a1c4e7b2d",
			Data: events.BroadcastStartedData{BroadcastID: "k7m2q9", BroadcastKey: "3f9a1c4e7b2d", Role: events.RoleOrigin}})
	}
	ids := collectIDs(t, js, "gawk.broadcast.>", 5)
	for i := 1; i < len(ids); i++ {
		if seqOf(t, ids[i]) != seqOf(t, ids[i-1])+1 {
			t.Fatalf("ids not consecutive: %v", ids)
		}
	}
}

// TestPublishNeverBlocks is D1 made a test: with the queue full, a hook still
// returns immediately and the overflow is counted. The media path's rule —
// drop rather than stall — applies to the relay's own telemetry.
func TestPublishNeverBlocks(t *testing.T) {
	// No stream, queue of one, and a publisher whose drain goroutine is busy:
	// what matters is that Publish returns, not that anything arrives.
	url := runNATS(t)
	p, m := newTestPublisher(t, url, func(o *Options) { o.QueueSize = 1 })

	start := time.Now()
	for i := 0; i < 5000; i++ {
		p.Publish(Event{Type: events.TypeBroadcastViewers, Key: "3f9a1c4e7b2d",
			Data: events.BroadcastViewersData{BroadcastID: "k7m2q9", BroadcastKey: "3f9a1c4e7b2d",
				Role: events.RoleOrigin, ViewersLocal: i}})
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("5000 hooks took %v — a hook must not wait on the bus", elapsed)
	}
	if m.drops(DropQueueFull) == 0 {
		t.Error("overflow was not counted")
	}
}

// TestMissingStreamCountsAsDrop: publishing before gawk-admin has created
// GAWK_EVENTS is an ordinary startup race (docs/51 §6), and the relay's answer
// is a counter, not a retry queue.
func TestMissingStreamCountsAsDrop(t *testing.T) {
	url := runNATS(t)
	p, m := newTestPublisher(t, url, nil)
	p.Publish(Event{Type: events.TypeBroadcastStarted, Key: "3f9a1c4e7b2d",
		Data: events.BroadcastStartedData{BroadcastID: "k7m2q9", BroadcastKey: "3f9a1c4e7b2d", Role: events.RoleOrigin}})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.drops(DropPublish) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("a publish with no stream was never counted as a drop")
}

// TestOffIsInert: an unset URL must cost nothing — no goroutine, no
// connection, no panic. This is what "off is byte-identical" rests on.
func TestOffIsInert(t *testing.T) {
	p, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Fatal("an empty URL must yield a nil publisher")
	}
	p.Publish(Event{Type: events.TypeBroadcastStarted})
	p.Close()
}

func firstMessage(t *testing.T, js jetstream.JetStream, subject string) jetstream.Msg {
	t.Helper()
	msgs := collect(t, js, subject, 1)
	return msgs[0]
}

func collect(t *testing.T, js jetstream.JetStream, subject string, n int) []jetstream.Msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := js.Stream(ctx, "GAWK_EVENTS")
	if err != nil {
		t.Fatal(err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := cons.Fetch(n, jetstream.FetchMaxWait(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var out []jetstream.Msg
	for msg := range batch.Messages() {
		if err := msg.Ack(); err != nil {
			t.Fatal(err)
		}
		out = append(out, msg)
	}
	if len(out) < n {
		t.Fatalf("got %d messages on %s, want %d", len(out), subject, n)
	}
	return out
}

func collectIDs(t *testing.T, js jetstream.JetStream, subject string, n int) []string {
	t.Helper()
	var ids []string
	for _, msg := range collect(t, js, subject, n) {
		var ce map[string]any
		if err := json.Unmarshal(msg.Data(), &ce); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ce["id"].(string))
	}
	return ids
}

func seqOf(t *testing.T, id string) int {
	t.Helper()
	i := strings.LastIndex(id, ":")
	if i < 0 {
		t.Fatalf("id %q is not <pod>:<seq>", id)
	}
	seq, err := strconv.Atoi(id[i+1:])
	if err != nil {
		t.Fatalf("id %q: %v", id, err)
	}
	return seq
}

// TestPublishDropsWhenTheDrainIsStuck is the D1 guarantee tested structurally,
// with no server in it at all: a publisher whose drain goroutine is not
// running (a NATS that accepts and never acks looks exactly like this from the
// hook's side) still returns immediately, and counts what it threw away.
//
// The server-backed TestPublishNeverBlocks exercises the same rule against a
// bus that fails fast. This one holds it when the bus fails SLOWLY, which is
// the case a future change to the drain path could quietly break.
func TestPublishDropsWhenTheDrainIsStuck(t *testing.T) {
	m := newCountingMetrics()
	// Built by hand rather than through New: there is no drain goroutine, so
	// the queue fills and stays full.
	p := &Publisher{
		ch:      make(chan Event, 2),
		metrics: m,
		now:     time.Now,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	start := time.Now()
	for i := 0; i < 10_000; i++ {
		p.Publish(Event{Type: events.TypeBroadcastViewers, Key: "3f9a1c4e7b2d",
			Data: events.BroadcastViewersData{BroadcastKey: "3f9a1c4e7b2d", ViewersLocal: i}})
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("10k hooks against a stuck drain took %v — a hook must never wait on the bus", elapsed)
	}
	if len(p.ch) != cap(p.ch) {
		t.Errorf("queue holds %d of %d: the test did not actually fill it", len(p.ch), cap(p.ch))
	}
	if got := m.drops(DropQueueFull); got != 10_000-cap(p.ch) {
		t.Errorf("counted %d overflow drops, want %d", got, 10_000-cap(p.ch))
	}
}
