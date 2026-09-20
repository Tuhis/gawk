// Package eventbus publishes relay lifecycle events to an operator-provided
// NATS JetStream (R50, docs/51).
//
// Three rules shape everything here:
//
//   - The media path never waits for it. A hook does one non-blocking channel
//     send and returns; a full queue is a counted drop, not backpressure
//     (D1). At-least-once is the BUS's promise from the moment a publish is
//     acked — before that the relay owes nothing that would cost a frame.
//   - This is the only package in the relay that may import nats.go (D8), and
//     it imports nothing from transport, hub or roomsrv: it takes Event values
//     on a channel. A NATS client is a network client with reconnect logic and
//     TLS, and it belongs in one place where the media path cannot reach it.
//   - Every message is a CloudEvent under the R51 contract (docs/52): this
//     package adds no encoding of its own, it calls events.Marshal.
//
// The zero value — a nil *Publisher — is the "off" implementation: every
// method is nil-safe, so a deployment without NATS runs the same code paths
// with no goroutine, no connection and no allocation per hook.
package eventbus

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// Metrics counts what reaches the bus and what does not. internal/metrics
// implements it; a nil Metrics is fine, and so is a typed nil behind it —
// every method there is nil-safe.
type Metrics interface {
	Published()
	Dropped(reason string)
}

// Drop reasons. A new one is cheap; conflating two is not.
const (
	// DropQueueFull: the bounded channel was full when a hook fired. The
	// publisher is slower than the transitions, or NATS is unreachable and the
	// drain goroutine is blocked on backpressure.
	DropQueueFull = "queue_full"
	// DropPublish: the async publish itself failed, including "no response
	// from stream" — which is what a relay publishing before gawk-admin has
	// created GAWK_EVENTS looks like (docs/51 §6: order does not matter).
	DropPublish = "publish"
	// DropEncode: the event could not be marshalled. A bug, not an operational
	// condition; counted rather than panicking, because a malformed event must
	// not take the relay down.
	DropEncode = "encode"
	// DropNoConn: there is no usable connection to publish on. Distinct from
	// DropPublish on purpose — "publish" sends an operator to look at the
	// stream, and this one is about the connection: a bus that is down, or a
	// credential it does not accept. The supervisor is re-dialling while this
	// climbs.
	DropNoConn = "disconnected"
)

// Event is what a hook hands the bus: the fact, and nothing about identity.
// The publisher assigns the sequence, the pod, the subject and the timestamp
// formatting as it drains, so a hook costs one struct and one channel send.
type Event struct {
	// Type is an events.Type* constant.
	Type string
	// Key is the fleet's HMAC'd key for the broadcast or room. It is the
	// CloudEvents subject AND the third token of the NATS subject: a raw ID or
	// room code must never appear in either (docs/51 D2).
	Key string
	// Time is when the transition happened. Zero means "now".
	Time time.Time
	// Data is the events.*Data struct for Type.
	Data any
}

// Options configures the publisher. URL empty means off, and off is the
// difference between New returning a nil *Publisher and a live one.
type Options struct {
	// URL is the NATS server, e.g. tls://nats.example:4222. Empty = off.
	URL string
	// CredsFile is an NKey/JWT .creds file. The relay's NATS user needs
	// publish permission on <SubjectPrefix>.> and nothing else (docs/51 D6).
	CredsFile string
	// TLSCertFile / TLSKeyFile are a client certificate, the other way a NATS
	// deployment identifies a workload: with `verify_and_map`, the subject DN
	// of this certificate IS the NATS username, so the identity is the cert
	// rather than a secret to distribute. cert-manager writes exactly these
	// two files (plus the CA) into one Secret.
	TLSCertFile string
	TLSKeyFile  string
	// CAFile is the CA to verify the SERVER against. A bus on a private CA —
	// which is the sane way to run one, since the bus is internal — needs it;
	// without it the platform trust store is used.
	CAFile string
	// SubjectPrefix is the first subject token; default "gawk".
	SubjectPrefix string
	// Pod names this process in the CloudEvents source and id.
	Pod string
	// ViewerInterval coalesces the high-rate delta types (broadcast.viewers,
	// room.attachment_updated) to at most one per key per interval, and only
	// on change. Default 5s.
	ViewerInterval time.Duration
	// QueueSize bounds the channel between the hooks and the publisher.
	// Default queueSizeDefault.
	QueueSize int
	// RedialInterval is how often a publisher with no usable connection builds
	// a new one. Default redialIntervalDefault. Not a flag: there is no
	// deployment that wants a configured bus abandoned, and a minute is small
	// against the time it takes an operator to notice anything.
	RedialInterval time.Duration
	// ReconnectWait is how long nats.go waits between its own connect
	// attempts. Zero leaves the client's default (2s), which is right for a
	// deployment; the tests shorten it so a rejection streak long enough to
	// trip the client's auth-abort fits in a test.
	ReconnectWait time.Duration
	// Insecure skips NATS TLS verification. The docs/41 compose lane only: it
	// is a flag with no chart value and it warns at every start.
	Insecure bool
	Logger   *slog.Logger
	Metrics  Metrics
	// Now is injectable for tests.
	Now func() time.Time
}

const (
	queueSizeDefault     = 1024
	viewerIntervalDefaut = 5 * time.Second
	// redialIntervalDefault is how long a configured-but-unusable bus stays
	// unusable before the publisher tries again.
	redialIntervalDefault = 60 * time.Second
	// warnInterval rate-limits the drop log. A bus that is down drops at the
	// rate the relay has transitions, and a log line per drop would be the
	// second thing going wrong.
	warnInterval = 30 * time.Second
)

// reconnectWait keeps nats.go's own default unless a caller (the tests) asked
// for a shorter one. Nothing may make it *longer* by accident: that would slow
// every legitimate reconnect to serve a test.
func reconnectWait(d time.Duration) time.Duration {
	if d <= 0 || d > nats.DefaultReconnectWait {
		return nats.DefaultReconnectWait
	}
	return d
}

// busConn is the pair the drain goroutine publishes on. It is swapped as a
// unit: a JetStream context belongs to the connection it was built from, and
// carrying one past its connection is how you get publishes that go nowhere.
type busConn struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// Publisher drains the hook channel onto JetStream. A nil *Publisher is the
// off switch and every method tolerates it.
type Publisher struct {
	ch   chan Event
	opts Options
	// conn is nil whenever there is nothing usable to publish on: before the
	// first successful dial, and after a connection died. The supervisor
	// replaces it; send only reads it.
	conn     atomic.Pointer[busConn]
	connOpts []nats.Option
	log      *slog.Logger
	metrics  Metrics
	now      func() time.Time

	seq uint64 // only touched by the drain goroutine

	done chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	lastWarn time.Time
}

// New connects and starts the drain goroutine. An empty URL returns (nil, nil):
// the caller stores the nil *Publisher and every hook becomes a no-op, which is
// what makes "off is byte-identical" true rather than aspirational.
//
// Connection failure is NOT an error and never will be: a configured bus that
// cannot be reached — down, not yet installed, refusing this credential — is
// an operational condition the relay rides out. It comes up, counts its drops,
// and keeps trying every RedialInterval until the bus accepts it. The error
// return is kept for the caller's shape and is nil in practice.
func New(opts Options) (*Publisher, error) {
	if opts.URL == "" {
		return nil, nil
	}
	if opts.SubjectPrefix == "" {
		opts.SubjectPrefix = "gawk"
	}
	if opts.ViewerInterval <= 0 {
		opts.ViewerInterval = viewerIntervalDefaut
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = queueSizeDefault
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RedialInterval <= 0 {
		opts.RedialInterval = redialIntervalDefault
	}

	p := &Publisher{
		ch:      make(chan Event, opts.QueueSize),
		opts:    opts,
		log:     opts.Logger.With("component", "eventbus"),
		metrics: opts.Metrics,
		now:     opts.Now,
		seq:     randomSeqStart(),
		done:    make(chan struct{}),
	}

	p.connOpts = []nats.Option{
		nats.Name("gawk-server/" + opts.Pod),
		// Retry forever, and come up even if the server is not there yet: the
		// relay must never fail to start because its telemetry sink is down.
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(reconnectWait(opts.ReconnectWait)),
		// THE ONE THAT COST A PRODUCTION AFTERNOON. By default nats.go stops
		// reconnecting once a server has returned the same authorization error
		// twice, on the theory that a rejected credential will stay rejected.
		// On a bus whose grants are reconciled from git that theory is wrong:
		// the relay pods rolled 40 seconds before the NATS config reload that
		// created their user, two of the three were rejected, and those two
		// held a dead connection until someone restarted them — publishing
		// nothing, with only a drop counter to say so.
		nats.IgnoreAuthErrorAbort(),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			p.log.Warn("event bus disconnected", "err", err)
		}),
		// Fires when a connect finally succeeds after RetryOnFailedConnect has
		// been retrying — which is exactly the shape of "the bus was down when
		// this pod started" and "the grant arrived late". Without it that
		// recovery is logged by nobody: ReconnectHandler is only for a
		// connection that was established once already.
		nats.ConnectHandler(func(nc *nats.Conn) {
			p.log.Info("event bus connected", "url", nc.ConnectedUrl())
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			p.log.Info("event bus reconnected", "url", nc.ConnectedUrl())
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			// Two closes are ours and are not news: shutdown, and the old
			// connection a re-dial just replaced. Anything else is terminal
			// for a connection that was supposed to be working, and the
			// supervisor is about to build another — this line is so the log
			// says that happened rather than going quiet.
			select {
			case <-p.done:
				return
			default:
			}
			if c := p.conn.Load(); c != nil && c.nc != nc {
				return
			}
			p.log.Error("event bus connection closed, re-dialling",
				"err", nc.LastError(), "retryIn", opts.RedialInterval)
		}),
	}
	if opts.CredsFile != "" {
		p.connOpts = append(p.connOpts, nats.UserCredentials(opts.CredsFile))
	}
	if opts.TLSCertFile != "" && opts.TLSKeyFile != "" {
		p.connOpts = append(p.connOpts, nats.ClientCert(opts.TLSCertFile, opts.TLSKeyFile))
	}
	if opts.CAFile != "" {
		p.connOpts = append(p.connOpts, nats.RootCAs(opts.CAFile))
	}
	if opts.Insecure {
		p.log.Warn("event bus TLS certificate verification is DISABLED " +
			"(-eventbus-insecure): local development only, never a deployment")
		p.connOpts = append(p.connOpts, insecureSkipVerify())
	}

	if err := p.dial(); err != nil {
		p.log.Error("event bus connect failed, retrying in the background",
			"url", opts.URL, "err", err, "retryIn", opts.RedialInterval)
	}

	p.wg.Add(2)
	go p.run()
	go p.supervise()
	return p, nil
}

// dial builds a connection and its JetStream context and installs them as the
// current pair, closing whatever they replace.
func (p *Publisher) dial() error {
	nc, err := nats.Connect(p.opts.URL, p.connOpts...)
	if err != nil {
		return err
	}
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncErrHandler(
		func(_ jetstream.JetStream, msg *nats.Msg, err error) {
			// Includes "no response from stream": a relay publishing before
			// gawk-admin has created GAWK_EVENTS. Counted, not retried.
			p.drop(DropPublish)
			p.warn("event bus publish failed", "subject", msg.Subject, "err", err)
		}))
	if err != nil {
		nc.Close()
		return err
	}
	if old := p.conn.Swap(&busConn{nc: nc, js: js}); old != nil {
		old.nc.Close()
	}
	return nil
}

// supervise is the promise that a configured bus is never abandoned.
//
// nats.go reconnects on its own and IgnoreAuthErrorAbort keeps it doing so
// through a credential the server has not learned about yet. This loop is the
// layer under that: whatever ends a connection for good — an abort a future
// client version decides on, a server that closed it, a dial that failed
// before there was a connection at all — a relay with eventbus.url set tries
// again every RedialInterval, forever, and says so in the log.
func (p *Publisher) supervise() {
	defer p.wg.Done()
	tick := time.NewTicker(p.opts.RedialInterval)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-tick.C:
			if c := p.conn.Load(); c != nil && !c.nc.IsClosed() {
				continue
			}
			if err := p.dial(); err != nil {
				p.log.Warn("event bus redial failed",
					"url", p.opts.URL, "err", err, "retryIn", p.opts.RedialInterval)
				continue
			}
			// "re-dialled", not "connected": nats.Connect with
			// RetryOnFailedConnect hands back a client that is still trying,
			// so this line means a new client exists, and the ConnectHandler
			// above is the one that means events can flow.
			p.log.Info("event bus re-dialled", "url", p.opts.URL)
		}
	}
}

// Publish hands an event to the bus. It never blocks and never returns an
// error: a full queue is a counted drop (D1). This is what the hooks at the
// relay's fan-out points call, on the goroutine that just did something real.
func (p *Publisher) Publish(ev Event) {
	if p == nil {
		return
	}
	select {
	case p.ch <- ev:
	default:
		p.drop(DropQueueFull)
		p.warn("event bus queue full, dropping event", "type", ev.Type)
	}
}

// Close stops the drain goroutine, flushes what JetStream has accepted and
// closes the connection.
func (p *Publisher) Close() {
	if p == nil {
		return
	}
	close(p.done)
	p.wg.Wait()
	c := p.conn.Load()
	if c == nil {
		// Configured, never usable. There is nothing to flush and nothing to
		// close, and shutdown is not the time to start caring.
		return
	}
	select {
	case <-c.js.PublishAsyncComplete():
	case <-time.After(2 * time.Second):
		// Shutdown is not the place to wait on a sick bus.
	}
	c.nc.Close()
}

// run is the one goroutine that touches NATS.
func (p *Publisher) run() {
	defer p.wg.Done()
	c := newCoalescer(p.opts.ViewerInterval)
	tick := time.NewTicker(p.opts.ViewerInterval)
	defer tick.Stop()
	for {
		select {
		case ev := <-p.ch:
			if c.isDelta(ev.Type) {
				if out, ok := c.offer(ev, p.now()); ok {
					p.send(out)
				}
				continue
			}
			p.send(ev)
		case <-tick.C:
			for _, ev := range c.flush(p.now()) {
				p.send(ev)
			}
		case <-p.done:
			return
		}
	}
}

// send builds the CloudEvent, wraps it in a NATS message and publishes.
//
// With no usable connection the event is a counted drop and the sequence still
// advances: (pod, seq) is a dedup key, not a delivery guarantee, and a gap in
// it is exactly the signal gawk-admin reports per pod (docs/51 D9). Reusing a
// number for a different event would be the harmful thing.
func (p *Publisher) send(ev Event) {
	p.seq++
	at := ev.Time
	if at.IsZero() {
		at = p.now()
	}
	id := events.BusID(p.opts.Pod, p.seq)
	ce := events.New(ev.Type, id, events.SourceRelay(p.opts.Pod), ev.Key, at, ev.Data)
	body, err := events.Marshal(ce)
	if err != nil {
		p.drop(DropEncode)
		p.warn("event bus encode failed", "type", ev.Type, "err", err)
		return
	}
	msg := &nats.Msg{
		Subject: p.subject(ev),
		Data:    body,
		Header: nats.Header{
			// The structured-mode signal of the CloudEvents NATS binding, and
			// JetStream's deduplication key — equal to the body's id, which is
			// also what gawk-admin deduplicates on forever (docs/51 D2, D3).
			"Content-Type": []string{events.ContentType},
			"Nats-Msg-Id":  []string{id},
		},
	}
	// IsConnected, not IsClosed: a client that is merely reconnecting accepts
	// an async publish into a pending buffer and reports the failure only when
	// the ack times out, tens of seconds later. That is a queue, and the rule
	// here is drop rather than queue — so a bus that is not connected right
	// now is a drop right now, counted with the reason an operator can act on.
	c := p.conn.Load()
	if c == nil || !c.nc.IsConnected() {
		p.drop(DropNoConn)
		p.warn("event bus has no connection, dropping event",
			"type", ev.Type, "retryIn", p.opts.RedialInterval)
		return
	}
	if _, err := c.js.PublishMsgAsync(msg); err != nil {
		p.drop(DropPublish)
		p.warn("event bus publish rejected", "subject", msg.Subject, "err", err)
		return
	}
	// Counted here, not on the ack: the ack arrives on the error handler's
	// goroutine and only failures are distinguishable there. published minus
	// dropped{publish} is what an operator compares against the stream's
	// message count.
	p.publishedInc()
}

// subject is <prefix>.<scope>.<key>.<event> — the shape that shows up in NATS
// monitoring, in server logs and in an operator's `nats sub '>'`, which is why
// the key token is the HMAC'd one (docs/51 D2).
func (p *Publisher) subject(ev Event) string {
	scope, event := splitType(ev.Type)
	key := ev.Key
	if key == "" {
		key = "_"
	}
	return p.opts.SubjectPrefix + "." + scope + "." + key + "." + event
}

// splitType turns fi.ioio.gawk.room.participant_joined into (room,
// participant_joined). An unrecognised type yields ("unknown", the type),
// because a subject is not worth dropping an event over.
func splitType(typ string) (scope, event string) {
	rest := strings.TrimPrefix(typ, events.TypePrefix)
	if i := strings.Index(rest, "."); i > 0 {
		return rest[:i], rest[i+1:]
	}
	return "unknown", rest
}

// warn logs at most one line per warnInterval: a bus that is down drops at the
// rate the relay has transitions, and a line per drop would be the second
// thing going wrong.
func (p *Publisher) warn(msg string, args ...any) {
	now := p.now()
	p.mu.Lock()
	quiet := now.Sub(p.lastWarn) < warnInterval
	if !quiet {
		p.lastWarn = now
	}
	p.mu.Unlock()
	if !quiet {
		p.log.Warn(msg, args...)
	}
}

// randomSeqStart keeps a restarted pod from re-issuing the ids it used a
// minute ago: (pod, seq) is the dedup key, and JetStream's own dedup window
// plus gawk-admin's UNIQUE source column both treat a repeat as a duplicate
// (docs/51 D3).
func randomSeqStart() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	// Leave room to count without wrapping; the value only has to be unlikely
	// to collide with the previous process's range.
	return binary.BigEndian.Uint64(b[:]) >> 16
}

// drop and publishedInc tolerate a nil Metrics, so a caller that wants no
// counters passes none rather than a stub.
func (p *Publisher) drop(reason string) {
	if p.metrics != nil {
		p.metrics.Dropped(reason)
	}
}

func (p *Publisher) publishedInc() {
	if p.metrics != nil {
		p.metrics.Published()
	}
}

// insecureSkipVerify relaxes certificate verification WITHOUT requiring TLS.
//
// nats.Secure does both, and the difference is not cosmetic: applied to a
// plain nats:// server it makes every handshake fail, and the client buries
// that in its reconnect loop — connections pile up unnamed, nothing is ever
// published, and the only symptom is silence. It also matters the other way
// round, because a NATS that requires TLS may still be dialled as nats://:
// the client upgrades from the server's INFO, TLS is not implied by the
// scheme. So the switch must say only what it means — do not verify — and
// leave whether TLS happens to the server.
func insecureSkipVerify() nats.Option {
	return func(o *nats.Options) error {
		if o.TLSConfig == nil {
			o.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		o.TLSConfig.InsecureSkipVerify = true //nolint:gosec // the flag's whole purpose, warned about at every start
		return nil
	}
}
