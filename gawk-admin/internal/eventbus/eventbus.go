// Package eventbus consumes the relay event bus (R50, docs/51) and turns it
// into rows and live state.
//
// It owns three things and nothing else:
//
//   - The STREAM. gawk-admin creates and updates GAWK_EVENTS and its durable
//     consumer, because it owns the retention and the consumer's semantics;
//     the relays only publish, with a NATS user that can do nothing else
//     (docs/51 D2, D6).
//   - INGEST, exactly once. The bus promises at-least-once; the UNIQUE
//     `source` column turns that into exactly-once at the table, and a
//     duplicate is a no-op that still acks.
//   - The LIVE VIEW: the last viewer count and attachment state per key, in
//     memory, never stored. A write per five seconds per live thing, for data
//     nobody audits, is not an audit trail.
//
// This is the only gawk-admin package that may import nats.go (docs/51 D8); a
// source walk in eventbus_import_test.go fails on any other importer.
//
// It runs on the elected leader only. Leader-only keeps per-subject ordering
// and reuses the election the dispatcher already runs on; a work queue across
// replicas would need a second ordering story for no gain at this volume.
package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// Defaults for the stream gawk-admin owns.
const (
	DefaultStream   = "GAWK_EVENTS"
	DefaultSubjects = "gawk.>"
	// MaxAge is the catch-up window: a leader that was down for an hour loses
	// nothing, and an idle stream costs bounded disk.
	MaxAge = 24 * time.Hour
	// ConsumerName is the durable consumer. One, on the leader.
	ConsumerName = "gawk-admin"
	// RedialInterval is the default for Options.RedialInterval: how long a
	// configured-but-unusable bus stays unusable before the portal tries
	// again.
	RedialInterval = 60 * time.Second
	// AckWait bounds how long an unacked message waits before redelivery.
	// A redelivery is harmless — it collides on `source` and acks — so this
	// trades a little duplicate work for never losing a message to a crash
	// mid-ingest.
	AckWait = 30 * time.Second
)

// Options configures the consumer. An empty URL means off, and off is the
// difference between New returning a nil *Consumer and a live one.
type Options struct {
	URL       string
	CredsFile string
	// TLSCertFile / TLSKeyFile are a client certificate — the other way a NATS
	// deployment identifies a workload. With `verify_and_map` the subject DN
	// of this certificate IS the NATS username, so there is no secret to
	// distribute and a renewal changes nothing. CAFile verifies the server,
	// which a bus on its own private CA requires.
	TLSCertFile string
	TLSKeyFile  string
	CAFile      string
	Stream      string
	// ManageStream lets this portal create and update the stream and its
	// durable consumer, which is the default and what a self-hosted install
	// wants (docs/51 D2: the consumer owns the retention).
	//
	// Set it false where the JetStream objects are declared elsewhere — a
	// NACK Stream/Consumer CR reconciled from git, say. Such a deployment
	// grants workloads no `$JS.API.>` at all, so trying to manage them does
	// not merely duplicate the operator's intent, it fails forever against a
	// permission that will never be given. The portal then BINDS to what is
	// there and consumes it.
	ManageStream bool
	// ConsumerName is the durable to use; empty means ConsumerName.
	ConsumerName string
	// MaxBytes and Replicas are the stream's limits; zero means the server's
	// default for replicas and 256 MiB for bytes.
	MaxBytes int64
	Replicas int
	// RedialInterval is how often a consumer with no usable connection builds
	// a new one, and how often a running consumer checks that its connection
	// is still alive. Default RedialInterval const. Not a flag: no deployment
	// wants a configured bus abandoned.
	RedialInterval time.Duration
	// ReconnectWait is how long nats.go waits between its own connect
	// attempts. Zero leaves the client's default (2s), which is right for a
	// deployment; the tests shorten it so a rejection streak long enough to
	// trip the client's auth-abort fits in a test.
	ReconnectWait time.Duration
	Insecure      bool
	Log           *slog.Logger
	Ingest        Ingester
	Now           func() time.Time
}

// Ingester is the store, as this package needs it. An interface so the
// consumer can be tested without Postgres, and so the store keeps no knowledge
// of NATS.
type Ingester interface {
	// Ingest records one bus event. inserted is false for a duplicate, which
	// is not an error: the consumer acks either way.
	Ingest(ctx context.Context, ev Event) (inserted bool, err error)
}

// Event is one decoded CloudEvent off the bus, with the parts a consumer acts
// on lifted out of the envelope. Raw carries the whole event verbatim, which
// is what the row's payload stores.
type Event struct {
	ID      string
	Source  string
	Type    string
	Subject string
	Time    time.Time
	Data    map[string]any
	Raw     json.RawMessage
}

// Pod is the publishing pod's name, parsed out of the CloudEvents source.
func (e Event) Pod() string { return strings.TrimPrefix(e.Source, "/gawk/relay/") }

// Seq is the per-pod sequence from the event id, and whether it parsed. A gap
// in one pod's sequence is a gap in what this consumer saw (docs/51 D9).
func (e Event) Seq() (uint64, bool) {
	i := strings.LastIndex(e.ID, ":")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(e.ID[i+1:], 10, 64)
	return n, err == nil
}

// reconnectWait keeps nats.go's own default unless a caller (the tests) asked
// for a shorter one. Nothing may make it *longer* by accident: that would slow
// every legitimate reconnect to serve a test.
func reconnectWait(d time.Duration) time.Duration {
	if d <= 0 || d > nats.DefaultReconnectWait {
		return nats.DefaultReconnectWait
	}
	return d
}

// Consumer holds the connection, the stream and the live view.
type Consumer struct {
	opts     Options
	log      *slog.Logger
	connOpts []nats.Option

	// connMu guards the three that are replaced together on a re-dial. A
	// jetstream.Stream handle belongs to the connection it came from, so they
	// travel as a set and nothing keeps one across a dial.
	connMu sync.Mutex
	nc     *nats.Conn
	js     jetstream.JetStream
	stream jetstream.Stream

	// closing tells the ClosedHandler that the close it is seeing is Close's
	// own, not a failure worth an ERROR line at shutdown.
	closing atomic.Bool

	mu   sync.Mutex
	live map[string]LiveEntry
	pods map[string]PodState
}

// conn is the current connection, or nil if there has never been one.
func (c *Consumer) conn() *nats.Conn {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.nc
}

func (c *Consumer) currentStream() jetstream.Stream {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.stream
}

// LiveEntry is the latest delta for one key: the data nobody stores.
type LiveEntry struct {
	Type string
	Data map[string]any
	At   time.Time
}

// PodState is what /api/v1/relays reports per publishing pod, so an operator
// can see the bus is alive BEFORE anything depends on it.
type PodState struct {
	Pod      string    `json:"pod"`
	LastSeen time.Time `json:"lastSeen"`
	LastSeq  uint64    `json:"lastSeq"`
	// Gaps counts sequence jumps observed for this pod. Non-zero means events
	// were dropped by the relay (its counters say why) or expired unread.
	Gaps int `json:"gaps"`
}

// New connects. An empty URL returns (nil, nil): every method on a nil
// *Consumer is a no-op, which is what makes "off is byte-identical" true.
//
// It does NOT wait for NATS, and a bus that is down is not a startup error.
// The portal's job is moderation; the bus is an optional feed into it, and a
// portal that refused to serve because NATS was unreachable would take the ban
// pipe down with it. The client reconnects in the background, Run keeps trying
// to create the stream, and /relays' bus section is where an operator sees
// that it has not happened yet.
//
// The stream is still THIS side's to own (docs/51 D2) — the relays only
// publish — it is just created on the leader, once there is something to
// create it on.
func New(ctx context.Context, opts Options) (*Consumer, error) {
	if opts.URL == "" {
		return nil, nil
	}
	if opts.Stream == "" {
		opts.Stream = DefaultStream
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 256 << 20
	}

	if opts.RedialInterval <= 0 {
		opts.RedialInterval = RedialInterval
	}

	c := &Consumer{
		opts: opts,
		log:  opts.Log.With("component", "eventbus"),
		live: map[string]LiveEntry{},
		pods: map[string]PodState{},
	}
	c.connOpts = []nats.Option{
		nats.Name("gawk-admin"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(reconnectWait(opts.ReconnectWait)),
		// nats.go stops reconnecting once a server has returned the same auth
		// error twice, assuming a rejected credential stays rejected. Where
		// the bus's grants are reconciled from git that is wrong and costly:
		// a portal that started before its NATS user existed held a dead
		// connection, retried the stream against it forever, and showed an
		// empty Events view until someone restarted the pod.
		nats.IgnoreAuthErrorAbort(),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			c.log.Warn("event bus disconnected", "err", err)
		}),
		// Fires when a connect finally succeeds after RetryOnFailedConnect has
		// been retrying — "the bus was down when this pod started" and "the
		// grant arrived late", the two cases this whole mechanism exists for.
		// ReconnectHandler covers only a connection that was up once already,
		// so without this the recovery is logged by nobody.
		nats.ConnectHandler(func(nc *nats.Conn) {
			c.log.Info("event bus connected", "url", nc.ConnectedUrl())
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			c.log.Info("event bus reconnected", "url", nc.ConnectedUrl())
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			// Two closes are ours and are not news: Close on shutdown, and the
			// old connection a re-dial just replaced.
			if c.closing.Load() {
				return
			}
			if cur := c.conn(); cur != nil && cur != nc {
				return
			}
			c.log.Error("event bus connection closed, re-dialling",
				"err", nc.LastError(), "retryIn", opts.RedialInterval)
		}),
	}
	if opts.CredsFile != "" {
		c.connOpts = append(c.connOpts, nats.UserCredentials(opts.CredsFile))
	}
	if opts.TLSCertFile != "" && opts.TLSKeyFile != "" {
		c.connOpts = append(c.connOpts, nats.ClientCert(opts.TLSCertFile, opts.TLSKeyFile))
	}
	if opts.CAFile != "" {
		c.connOpts = append(c.connOpts, nats.RootCAs(opts.CAFile))
	}
	if opts.Insecure {
		opts.Log.Warn("event bus TLS certificate verification is DISABLED " +
			"(-eventbus-insecure): local development only, never a deployment")
		c.connOpts = append(c.connOpts, insecureSkipVerify())
	}
	if err := c.dial(); err != nil {
		// Not fatal, and not a reason to refuse to serve: moderation does not
		// depend on the feed. Run re-dials until the bus takes this portal.
		c.log.Error("event bus connect failed, retrying in the background",
			"url", opts.URL, "err", err, "retryIn", opts.RedialInterval)
	}
	return c, nil
}

// dial builds a connection and its JetStream context and installs them,
// dropping the stream handle that belonged to the connection being replaced.
func (c *Consumer) dial() error {
	nc, err := nats.Connect(c.opts.URL, c.connOpts...)
	if err != nil {
		return fmt.Errorf("eventbus: connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return fmt.Errorf("eventbus: jetstream: %w", err)
	}
	c.connMu.Lock()
	old := c.nc
	c.nc, c.js, c.stream = nc, js, nil
	c.connMu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

// ensureConn re-dials if there is nothing usable, and is a no-op otherwise —
// including while nats.go is reconnecting on its own, which is not this
// layer's business.
func (c *Consumer) ensureConn() error {
	// After Close there is nothing to ensure. Without this a Run whose
	// leadership context is still live dials a connection that Close had just
	// released and that nothing will ever close again: measurably, a fresh
	// CONNECTED client appears within one re-dial tick of Close. main cancels
	// the context first today, so it never bit — but the type should not
	// permit it.
	if c.closing.Load() {
		return errors.New("eventbus: consumer is closed")
	}
	if nc := c.conn(); nc != nil && !nc.IsClosed() {
		return nil
	}
	if err := c.dial(); err != nil {
		return err
	}
	// "re-dialled", not "connected": nats.Connect with RetryOnFailedConnect
	// hands back a client that is still trying, so this line means a new
	// client exists and the ConnectHandler above is the one that means the
	// feed can flow.
	c.log.Info("event bus re-dialled", "url", c.opts.URL)
	return nil
}

// awaitStream keeps trying to create the stream until it exists or the leader
// stops being one.
//
// NATS may be unreachable, still starting, or refusing this credential; none
// of those is worth giving up over, and none of them should have stopped the
// portal from serving in the first place. Each failure is logged once per
// retry so an operator watching the log sees why the feed is quiet.
func (c *Consumer) awaitStream(ctx context.Context) error {
	// 5s in any deployment, but never slower than the re-dial interval that
	// governs every other retry here — one knob, so a caller that wants a
	// brisk loop (the tests) does not have to reach past this one.
	retry := 5 * time.Second
	if c.opts.RedialInterval > 0 && c.opts.RedialInterval < retry {
		retry = c.opts.RedialInterval
	}
	for {
		// A connection first: a stream cannot be reached over one that is
		// gone, and "gone" is what an auth failure at startup leaves behind.
		err := c.ensureConn()
		if err == nil {
			err = c.ensureStream(ctx)
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			// Leadership moved or the process is stopping: not an error worth
			// reporting, the next leader will do this.
			return nil
		}
		c.log.Warn("event bus stream not ready, retrying", "stream", c.opts.Stream, "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
}

// ensureStream creates or updates GAWK_EVENTS with the documented limits.
func (c *Consumer) ensureStream(ctx context.Context) error {
	cfg := jetstream.StreamConfig{
		Name:      c.opts.Stream,
		Subjects:  []string{DefaultSubjects},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    MaxAge,
		MaxBytes:  c.opts.MaxBytes,
		Discard:   jetstream.DiscardOld,
		Replicas:  c.opts.Replicas,
	}
	c.connMu.Lock()
	js := c.js
	c.connMu.Unlock()
	if js == nil {
		return fmt.Errorf("eventbus: no connection to %s", c.opts.URL)
	}
	var (
		stream jetstream.Stream
		err    error
	)
	if c.opts.ManageStream {
		stream, err = js.CreateOrUpdateStream(ctx, cfg)
	} else {
		// Somebody else declares it. Bind, and wait for them if it is not
		// there yet — the retry loop above is the same either way.
		stream, err = js.Stream(ctx, c.opts.Stream)
	}
	if err != nil {
		return fmt.Errorf("eventbus: ensure stream %s: %w", c.opts.Stream, err)
	}
	c.connMu.Lock()
	c.stream = stream
	c.connMu.Unlock()
	return nil
}

// Close releases the connection. Safe on a nil *Consumer, and on one that
// never had a connection to begin with.
func (c *Consumer) Close() {
	if c == nil {
		return
	}
	c.closing.Store(true)
	if nc := c.conn(); nc != nil {
		nc.Close()
	}
}

// Run consumes until ctx ends. The caller starts it on the elected leader and
// cancels ctx when leadership is lost.
//
// DeliverPolicy all on first creation: a fresh durable re-reads the retained
// window and every message it has seen before collides on `source`, so a
// rebuilt consumer costs work and loses nothing.
// It returns only when ctx ends. Everything else — a bus that is down, a
// credential it has not been granted yet, a connection that died mid-consume —
// is retried every RedialInterval, because the alternative is a leader that
// holds the durable consumer and quietly ingests nothing.
func (c *Consumer) Run(ctx context.Context) error {
	if c == nil {
		return nil
	}
	for {
		if err := c.runOnce(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("event bus consumer stopped, retrying",
				"err", err, "retryIn", c.opts.RedialInterval)
		}
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(c.opts.RedialInterval):
		}
	}
}

// runOnce binds the consumer and delivers until ctx ends or the connection
// under it dies. Returning is not failure — Run's loop is what turns a
// returned error into another attempt.
func (c *Consumer) runOnce(ctx context.Context) error {
	var err error
	if err = c.awaitStream(ctx); err != nil {
		return err
	}
	stream := c.currentStream()
	if stream == nil {
		// Leadership ended while the stream was still out of reach. Nothing
		// to consume from and nothing to report: the next leader picks this up.
		return nil
	}
	name := c.opts.ConsumerName
	if name == "" {
		name = ConsumerName
	}
	var cons jetstream.Consumer
	if c.opts.ManageStream {
		cons, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
			Durable:       name,
			AckPolicy:     jetstream.AckExplicitPolicy,
			AckWait:       AckWait,
			DeliverPolicy: jetstream.DeliverAllPolicy,
			MaxDeliver:    -1,
		})
	} else {
		// Declared elsewhere, with its own delivery rules. Binding rather than
		// asserting them is the point: the operator's CR is the truth, and a
		// portal that "corrected" it would fight its reconciler every loop.
		cons, err = stream.Consumer(ctx, name)
	}
	if err != nil {
		return fmt.Errorf("eventbus: ensure consumer %s: %w", name, err)
	}
	sub, err := cons.Consume(func(msg jetstream.Msg) {
		c.handle(ctx, msg)
	})
	if err != nil {
		return fmt.Errorf("eventbus: consume: %w", err)
	}
	defer sub.Stop()

	// Consume delivers on its own goroutine and reports nothing when the
	// connection under it ends, so watch for that here: a leader silently not
	// consuming is the failure mode worth the ticker.
	tick := time.NewTicker(c.opts.RedialInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if nc := c.conn(); nc == nil || nc.IsClosed() {
				return errors.New("eventbus: connection closed under the consumer")
			}
		}
	}
}

// handle ingests one message and acks it.
//
// Every path acks, including the failures that would never succeed on a
// retry: a message this build cannot parse, or a type it does not know, would
// otherwise be redelivered forever and stall the consumer behind it. An
// unknown type is expected — a newer relay may publish one — and the rule from
// the contract is to treat it as unknown, not as an error (docs/52 D6).
func (c *Consumer) handle(ctx context.Context, msg jetstream.Msg) {
	ev, err := decode(msg.Data())
	if err != nil {
		c.log.Warn("undecodable bus message, acking", "err", err)
		ack(c.log, msg)
		return
	}
	c.note(ev)

	if isDelta(ev.Type) {
		// Deltas are the live view and nothing else (docs/51 D5).
		c.remember(ev)
		ack(c.log, msg)
		return
	}
	if c.opts.Ingest == nil {
		ack(c.log, msg)
		return
	}
	switch _, err := c.opts.Ingest.Ingest(ctx, ev); {
	case err == nil:
		ack(c.log, msg)
	case errors.Is(err, context.Canceled):
		// Leadership moved or the process is stopping: do NOT ack. The next
		// leader redelivers and the row is written there.
	default:
		c.log.Warn("ingest failed, leaving the message for redelivery",
			"type", ev.Type, "id", ev.ID, "err", err)
	}
}

func ack(log *slog.Logger, msg jetstream.Msg) {
	if err := msg.Ack(); err != nil {
		log.Warn("ack failed", "err", err)
	}
}

// note records per-pod liveness and gaps. A gap is logged, not repaired: a
// consumer that needs the truth after a drop reconciles against the read API
// (docs/51 D1, D9).
func (c *Consumer) note(ev Event) {
	pod := ev.Pod()
	if pod == "" {
		return
	}
	seq, ok := ev.Seq()
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pods[pod]
	if ok && st.LastSeq != 0 && seq > st.LastSeq+1 {
		st.Gaps++
		c.log.Info("event bus sequence gap", "pod", pod, "missing", seq-st.LastSeq-1)
	}
	if ok {
		st.LastSeq = seq
	}
	st.Pod, st.LastSeen = pod, c.opts.Now().UTC()
	c.pods[pod] = st
}

func (c *Consumer) remember(ev Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live[ev.Type+"|"+ev.Subject] = LiveEntry{Type: ev.Type, Data: ev.Data, At: ev.Time}
}

// Live returns a copy of the live view: the last delta per key.
func (c *Consumer) Live() map[string]LiveEntry {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]LiveEntry, len(c.live))
	for k, v := range c.live {
		out[k] = v
	}
	return out
}

// Health is what GET /api/v1/relays reports under `bus`. Nil when the bus is
// off, which is how the route says "not configured" rather than "quiet".
func (c *Consumer) Health(ctx context.Context) *Health {
	if c == nil {
		return nil
	}
	// Through the accessors: this runs on an HTTP handler's goroutine while
	// the leader's Run may be re-dialling, and the connection and its stream
	// handle are replaced together.
	nc := c.conn()
	stream := c.currentStream()

	h := &Health{Stream: c.opts.Stream, Connected: nc != nil && nc.IsConnected()}
	c.mu.Lock()
	for _, st := range c.pods {
		h.Pods = append(h.Pods, st)
	}
	c.mu.Unlock()
	if stream == nil {
		// Configured, not yet established: the leader is still trying, or
		// this replica is not the leader. Either way the feed is not flowing
		// and the operator should see that rather than a blank.
		h.Error = "stream not created yet"
		return h
	}
	if info, err := stream.Info(ctx); err == nil {
		h.Messages = info.State.Msgs
		h.Bytes = info.State.Bytes
	} else {
		h.Error = err.Error()
	}
	return h
}

// Health is the bus's answer to "is it alive?" — per-pod last message and the
// stream's own state, so an operator can tell a quiet fleet from a broken bus
// before R49's webhooks depend on it.
type Health struct {
	Stream    string     `json:"stream"`
	Connected bool       `json:"connected"`
	Messages  uint64     `json:"messages"`
	Bytes     uint64     `json:"bytes"`
	Pods      []PodState `json:"pods,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// decode parses a structured-mode CloudEvent.
func decode(body []byte) (Event, error) {
	var env struct {
		ID      string          `json:"id"`
		Source  string          `json:"source"`
		Type    string          `json:"type"`
		Subject string          `json:"subject"`
		Time    time.Time       `json:"time"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Event{}, err
	}
	if env.ID == "" || env.Type == "" {
		return Event{}, errors.New("not a CloudEvent: no id or type")
	}
	ev := Event{ID: env.ID, Source: env.Source, Type: env.Type,
		Subject: env.Subject, Time: env.Time.UTC(), Raw: json.RawMessage(body)}
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &ev.Data); err != nil {
			return Event{}, fmt.Errorf("data is not an object: %w", err)
		}
	}
	return ev, nil
}

// isDelta names the two coalesced, high-rate types the relay publishes. They
// update the live view and are never stored: a write every five seconds per
// live thing, for data nobody audits, is not an audit trail (docs/51 D5).
func isDelta(typ string) bool {
	return typ == events.TypeBroadcastViewers || typ == events.TypeRoomAttachmentUpdated
}
