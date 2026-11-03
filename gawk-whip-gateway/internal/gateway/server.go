// Package gateway is gawk-whip-gateway's WHIP endpoint (docs/83 WG5–WG7):
// the HTTP API, one pion PeerConnection per OBS session, the media adapter
// feeding the shared publisher engine, the status records and D10's peer
// lookup.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/Tuhis/gawk/gawk-server/publisher"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/clientip"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/media"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/ratelimit"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
)

// Engine is the slice of *publisher.Session a WHIP session drives.
type Engine interface {
	Start(ctx context.Context) error
	Stop() error
	StopBroadcast() error
}

// EngineFactory builds an engine; the default is publisher.New.
type EngineFactory func(publisher.Config, publisher.Callbacks, publisher.Options) Engine

// DefaultEngineFactory is the production engine.
func DefaultEngineFactory(c publisher.Config, cb publisher.Callbacks, o publisher.Options) Engine {
	return publisher.New(c, cb, o)
}

// RelayOptions say where and how the gateway publishes.
type RelayOptions struct {
	// URL is the relay's https:// origin.
	URL string
	// PublishSecret is the relay's -publish-secret (?secret=).
	PublishSecret string
	// PublishProxyToken is the relay's -publish-proxy-token (D4): sent as
	// Authorization: Bearer on every publish dial, with Gawk-Publisher-Ip.
	PublishProxyToken string
	// Origin is the Origin sent on the dial; empty is publisher.DefaultOrigin.
	Origin string
	// Insecure skips TLS verification (dev certs only).
	Insecure bool
	// DialTimeout bounds the publish dial a POST waits on.
	DialTimeout time.Duration
}

// Options configure a Server. config.Config.Options builds them.
type Options struct {
	Relay RelayOptions

	// PublicAddress is the address put in every host candidate (D10: the
	// shared LoadBalancer address, mapped 1:1). Empty advertises the local
	// interface addresses as they are.
	PublicAddress string
	// MediaBind is the host the UDP mux and the ICE-TCP listener bind
	// (empty: all); MediaPort their port (basePort + ordinal). Zero picks a
	// free port (tests).
	MediaBind string
	MediaPort int
	// ICE timeouts (pion's SetICETimeouts): how long without traffic until
	// the session counts as disconnected, then failed (D8's lost).
	ICEDisconnectedTimeout time.Duration
	ICEFailedTimeout       time.Duration
	ICEKeepalive           time.Duration

	// AllowedOrigins may read and clear /whip/status cross-origin (D9).
	// Empty allows any origin (development); the bearer key is the
	// credential, never a cookie.
	AllowedOrigins []string
	// TrustedProxyCIDRs: D4.
	TrustedProxyCIDRs []string
	// POSTRate and POSTBurst: the per-client-IP POST /whip limit; POSTRate
	// <= 0 disables it.
	POSTRate  float64
	POSTBurst int
	// StatusRate and StatusBurst: the per-client-IP GET/DELETE /whip/status
	// limit; StatusRate <= 0 disables it.
	StatusRate  float64
	StatusBurst int

	// MaxSessions and MaxSessionsHard: D10's per-pod caps.
	MaxSessions     int
	MaxSessionsHard int
	// FullRetryAfter is the Retry-After of a 503 (D8: 30 s); RelayBusyRetryAfter
	// that of a relay 429 or an unreachable relay.
	FullRetryAfter      time.Duration
	RelayBusyRetryAfter time.Duration

	Reorder media.ReorderConfig
	GOP     media.GOPConfig
	Backoff status.BackoffPolicy
	// RecordTTL is how long a refusal or an ended session is reported (D9:
	// 10 min); MaxRecords bounds the table (4096).
	RecordTTL  time.Duration
	MaxRecords int

	// Peers are the other gateway pods' internal base URLs (D10), e.g.
	// http://gawk-whip-gateway-1.gawk-whip-gateway-peers:8081. Empty: one
	// replica, no lookup.
	Peers []string
	// InternalToken authenticates the internal listener (D10).
	InternalToken string
	// PeerTimeout is how long a peer may take to answer (200 ms).
	PeerTimeout time.Duration

	Version string
	Log     *slog.Logger
	Metrics *Metrics

	// Seams for tests; nil in production.
	Dial      publisher.DialFunc
	NewEngine EngineFactory
	Now       func() time.Time
	// IncludeLoopback gathers loopback candidates (in-process tests).
	IncludeLoopback bool
	// Hooks observe events tests assert on.
	Hooks Hooks
}

// Hooks are test observation points. All optional.
type Hooks struct {
	// PLI fires for every PLI sent, with its trigger ("start", "resume").
	PLI func(sessionID, trigger string)
	// SkipTakeoverAck makes D10's takeover skip its acknowledgement, as if
	// the ack never reached the other pod.
	SkipTakeoverAck bool
	// EngineConfig sees each session's engine configuration.
	EngineConfig func(publisher.Config)

	// capChecked runs right after a POST passed the session caps.
	capChecked func()
	// beforeLiveRecord runs in goLive between its check and its record write.
	beforeLiveRecord func(*session)
}

// Server is one gateway pod.
type Server struct {
	opts    Options
	log     *slog.Logger
	m       *Metrics
	now     func() time.Time
	table   *status.Table
	ips     *clientip.Resolver
	limiter *ratelimit.Limiter
	// statusLimiter is the per-client-IP GET/DELETE /whip/status limit: each
	// request for a key this pod doesn't hold asks every peer (D10).
	statusLimiter *ratelimit.Limiter
	peers         *peerClient

	udp      net.PacketConn
	tcp      net.Listener
	settings webrtc.SettingEngine

	mu       sync.Mutex
	sessions map[string]*session      // by session ID
	byKey    map[status.Hash]*session // the key's newest session on this pod
	pending  int                      // POSTs past the cap check, not yet sessions
	keyLocks map[status.Hash]*keyLock // serialises one key's POSTs
	closed   bool

	xffWarnMu   sync.Mutex
	xffWarnLast time.Time
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

// New builds a Server and binds its media ports.
func New(o Options) (*Server, error) {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Metrics == nil {
		o.Metrics = NewMetrics(o.Version)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewEngine == nil {
		o.NewEngine = DefaultEngineFactory
	}
	if o.Relay.DialTimeout <= 0 {
		o.Relay.DialTimeout = 10 * time.Second
	}
	if o.PeerTimeout <= 0 {
		o.PeerTimeout = 200 * time.Millisecond
	}
	if o.MaxSessions <= 0 {
		return nil, errors.New("gateway: MaxSessions must be positive")
	}
	if o.MaxSessionsHard < o.MaxSessions {
		o.MaxSessionsHard = o.MaxSessions
	}
	if o.RecordTTL <= 0 {
		o.RecordTTL = 10 * time.Minute
	}
	if o.Backoff.Initial <= 0 || o.Backoff.Max < o.Backoff.Initial {
		return nil, errors.New("gateway: invalid backoff policy")
	}
	if len(o.Peers) > 0 && o.InternalToken == "" {
		return nil, errors.New("gateway: peers need an internal token")
	}
	ips, err := clientip.New(o.TrustedProxyCIDRs)
	if err != nil {
		return nil, fmt.Errorf("gateway: trusted proxy CIDRs: %w", err)
	}
	s := &Server{
		opts:    o,
		log:     o.Log,
		m:       o.Metrics,
		now:     o.Now,
		table:   status.NewTable(o.MaxRecords, o.RecordTTL),
		ips:     ips,
		limiter: ratelimit.New(o.POSTRate, o.POSTBurst, 65536),

		statusLimiter: ratelimit.New(o.StatusRate, o.StatusBurst, 65536),
		sessions:      make(map[string]*session),
		byKey:         make(map[status.Hash]*session),
		keyLocks:      make(map[status.Hash]*keyLock),
	}
	s.peers = newPeerClient(o.Peers, o.InternalToken, o.PeerTimeout, s.m)
	if err := s.listenMedia(); err != nil {
		return nil, err
	}
	return s, nil
}

// listenMedia binds the UDP mux and the ICE-TCP passive listener on one
// port number (D10: media.basePort + ordinal) and builds the shared
// SettingEngine: ICE-lite, host candidates rewritten to PublicAddress.
func (s *Server) listenMedia() error {
	bind := s.opts.MediaBind
	var udp net.PacketConn
	var tcp net.Listener
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		udp, err = net.ListenPacket("udp", net.JoinHostPort(bind, strconv.Itoa(s.opts.MediaPort)))
		if err != nil {
			return fmt.Errorf("gateway: media UDP: %w", err)
		}
		port := udp.LocalAddr().(*net.UDPAddr).Port
		tcp, err = net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
		if err == nil {
			break
		}
		_ = udp.Close()
		if s.opts.MediaPort != 0 {
			return fmt.Errorf("gateway: media ICE-TCP: %w", err)
		}
	}
	if err != nil {
		return fmt.Errorf("gateway: media ports: %w", err)
	}
	s.udp, s.tcp = udp, tcp

	se := webrtc.SettingEngine{LoggerFactory: pionLogs{log: s.log}}
	se.SetLite(true)
	pl := pionLogs{log: s.log}
	se.SetICEUDPMux(webrtc.NewICEUDPMux(pl.NewLogger("udpmux"), udp))
	se.SetICETCPMux(webrtc.NewICETCPMux(pl.NewLogger("tcpmux"), tcp, 8))
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	disc, failed, keep := s.opts.ICEDisconnectedTimeout, s.opts.ICEFailedTimeout, s.opts.ICEKeepalive
	if disc <= 0 {
		disc = 5 * time.Second
	}
	if failed <= 0 {
		failed = 15 * time.Second
	}
	if keep <= 0 {
		keep = 2 * time.Second
	}
	se.SetICETimeouts(disc, failed, keep)
	if s.opts.IncludeLoopback {
		se.SetIncludeLoopbackCandidate(true)
	}
	if s.opts.PublicAddress != "" {
		// D10: each pod maps the shared LB address 1:1 and answers with a
		// host candidate carrying its own port. (SetNAT1To1IPs with
		// ICECandidateTypeHost, in pion v4's non-deprecated spelling.)
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{s.opts.PublicAddress},
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		}); err != nil {
			_ = udp.Close()
			_ = tcp.Close()
			return fmt.Errorf("gateway: public address: %w", err)
		}
	}
	s.settings = se
	return nil
}

// MediaPort is the bound media port (UDP and TCP).
func (s *Server) MediaPort() int { return s.udp.LocalAddr().(*net.UDPAddr).Port }

// Metrics exposes the server's metrics.
func (s *Server) Metrics() *Metrics { return s.m }

// Close ends every session (code 0: the broadcasts wait in grace and OBS
// reconnects, D10's restart) and releases the media ports.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	all := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		all = append(all, sess)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, sess := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.end(endCause{kind: endShutdown})
		}()
	}
	wg.Wait()
	_ = s.udp.Close()
	_ = s.tcp.Close()
}

// lockKey serialises one key's POSTs on this pod.
func (s *Server) lockKey(h status.Hash) func() {
	s.mu.Lock()
	kl := s.keyLocks[h]
	if kl == nil {
		kl = &keyLock{}
		s.keyLocks[h] = kl
	}
	kl.refs++
	s.mu.Unlock()
	kl.mu.Lock()
	return func() {
		kl.mu.Unlock()
		s.mu.Lock()
		if kl.refs--; kl.refs == 0 {
			delete(s.keyLocks, h)
		}
		s.mu.Unlock()
	}
}

// liveSession is the key's session on this pod while it is live or still
// being torn down (D10: both exempt a reconnect from the cap).
func (s *Server) liveSession(h status.Hash) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byKey[h]
}

// admit checks D10's caps and reserves a slot in one critical section, so
// two POSTs racing at maxSessions-1 can't both pass the check before either
// reserves. The load is sessions plus admitted POSTs. A same-pod replacement
// (exempt) doesn't grow the count; a reconnect whose session is on a peer
// (peerExempt) is held to maxSessionsHard; anything else to maxSessions.
// release undoes the reservation.
func (s *Server) admit(exempt, peerExempt bool) (release func(), ok bool) {
	s.mu.Lock()
	n := len(s.sessions) + s.pending
	switch {
	case exempt:
	case peerExempt:
		if n >= s.opts.MaxSessionsHard {
			s.mu.Unlock()
			return nil, false
		}
	default:
		if n >= s.opts.MaxSessions {
			s.mu.Unlock()
			return nil, false
		}
	}
	s.pending++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.pending--
			s.mu.Unlock()
		})
	}, true
}

// recordRefusal records a POST refused before a session existed (D9).
func (s *Server) recordRefusal(h status.Hash, reason, codec string) {
	now := s.now()
	s.table.Update(h, now, func(r *status.Record) {
		r.LastError = &status.ErrorInfo{Reason: reason, Codec: codec}
		r.LastErrorAt = now
		if r.State == status.StateLive && s.liveSession(h) == nil {
			r.State = status.StateEnded
		}
	})
}

// warnXFF logs D4's silent misconfiguration at most once a minute.
func (s *Server) warnXFF() {
	s.xffWarnMu.Lock()
	defer s.xffWarnMu.Unlock()
	now := s.now()
	if now.Sub(s.xffWarnLast) < time.Minute {
		return
	}
	s.xffWarnLast = now
	s.log.Warn("X-Forwarded-For from a peer outside trustedProxyCidrs while the list is set: the header is ignored and every user behind that proxy shares one rate-limit bucket; add the ingress controller's addresses (both families on dual-stack)")
}
