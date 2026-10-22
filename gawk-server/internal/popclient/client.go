package popclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/lanaddr"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Link headers: the console's header line (docs/76 §7.1), the same strings
// as relayconsole.State.Header.
const (
	// HeaderStandalone: no credential; a plain LAN relay.
	HeaderStandalone = "standalone"
	// HeaderWaiting: a credential, the first link not up yet.
	HeaderWaiting = "waiting"
	// HeaderLinked: the link is up and home sent its config.
	HeaderLinked = "linked"
	// HeaderOffline: the link is down and being retried.
	HeaderOffline = "offline"
	// HeaderRevoked: home revoked the PoP, or its end date passed.
	HeaderRevoked = "revoked"
	// HeaderRepair: the credential is not current (a Re-pair); paste the
	// new string.
	HeaderRepair = "repair"
)

// Session is a link or leg session.
type Session = poplink.Session

// DialFunc dials a /pop/* URL. network is "udp" for the link and "udp4" or
// "udp6" for the /pop/addr dial over a given family. A refusal returns the
// HTTP response (the status and poplink.ReasonHeader) with the error.
type DialFunc func(ctx context.Context, network, url string, h http.Header) (Session, *http.Response, error)

// Options wires a Client.
type Options struct {
	// StateDir holds the credential (relaystate secret "pop-credential").
	StateDir string
	Version  string
	// Pair is a pairing string to redeem at start (-pair / GAWK_PAIR). It
	// is skipped when the stored credential was paired with it already, so
	// a container that keeps GAWK_PAIR set restarts cleanly.
	Pair     string
	Registry *hub.Registry
	Server   *transport.Server
	Rolling  *tlsutil.Rolling
	LAN      *lanaddr.Watcher
	Relay    config.RelayConfig
	Cfg      config.Config
	Log      *slog.Logger
	// Dial replaces the default dialer (tests); nil dials with RootCAs.
	Dial DialFunc
	// RootCAs verifies home's certificate (nil: the system roots).
	RootCAs *x509.CertPool
	// Retry timers: RetryMin doubles per failure up to RetryMax; a refusal
	// that needs the operator (401, 403, 426, a Bye) retries at
	// TerminalRetry, so a mistaken revoke undone at home relinks by itself.
	RetryMin, RetryMax, TerminalRetry time.Duration
	// StatsEvery is the Stats record interval (default 10 s).
	StatsEvery time.Duration
	// Hostname names the relay when neither -server-name nor home's
	// display name does (default os.Hostname).
	Hostname func() (string, error)
}

// LinkState is the link as the console shows it.
type LinkState struct {
	Header string
	// Config is home's last config record (nil until linked once).
	Config *poplink.Config
	// ByeReason is the last Bye's reason, or the reason a refusal named.
	ByeReason string
	// LastStatus is the last refused dial's HTTP status (0: none).
	LastStatus int
	// Endpoint is the home endpoint the credential is for.
	Endpoint string
}

// PairError is a refused pairing: the status and reason the console shows
// ("used", "pairing-expired", "unknown", "revoked", ...).
type PairError struct {
	Status int
	Reason string
}

func (e *PairError) Error() string {
	return fmt.Sprintf("popclient: pairing refused: %d %s", e.Status, e.Reason)
}

// ErrNotLinked: Send with no link up.
var ErrNotLinked = errors.New("popclient: not linked")

// Client is the PoP side of the link.
type Client struct {
	opts Options
	log  *slog.Logger
	dial DialFunc

	kick chan struct{}

	mu      sync.Mutex
	state   LinkState
	stored  *storedCredential
	pending *pairRequest
	ctrl    Session
	stream  interface{ Write([]byte) (int, error) }
	subs    map[int]func(LinkState)
	nextSub int

	wmu sync.Mutex

	hmu      sync.RWMutex
	handlers map[string][]func(rec any)
}

// New builds a Client from the state directory's stored credential.
func New(o Options) *Client {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.RetryMin <= 0 {
		o.RetryMin = 500 * time.Millisecond
	}
	if o.RetryMax <= 0 {
		o.RetryMax = 15 * time.Second
	}
	if o.TerminalRetry <= 0 {
		o.TerminalRetry = 30 * time.Second
	}
	if o.StatsEvery <= 0 {
		o.StatsEvery = 10 * time.Second
	}
	c := &Client{
		opts:     o,
		log:      o.Log.With("component", "pop-link"),
		kick:     make(chan struct{}, 1),
		subs:     map[int]func(LinkState){},
		handlers: map[string][]func(any){},
	}
	c.dial = o.Dial
	if c.dial == nil {
		c.dial = defaultDialer(o.RootCAs)
	}
	c.state.Header = HeaderStandalone
	if st, err := loadCredential(o.StateDir); err != nil {
		c.log.Warn("stored PoP credential unreadable; running standalone", "err", err)
	} else if st != nil {
		c.stored = st
		c.state.Header = HeaderWaiting
		c.state.Endpoint = st.Endpoint
	}
	return c
}

// State is the link's current state.
func (c *Client) State() LinkState {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state
	if s.Config != nil {
		cfg := *s.Config
		s.Config = &cfg
	}
	return s
}

// Subscribe calls fn after every state change; the returned func stops it.
func (c *Client) Subscribe(fn func(LinkState)) (cancel func()) {
	c.mu.Lock()
	id := c.nextSub
	c.nextSub++
	c.subs[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

// setState applies fn to the state and notifies subscribers.
func (c *Client) setState(fn func(s *LinkState)) {
	c.mu.Lock()
	fn(&c.state)
	subs := make([]func(LinkState), 0, len(c.subs))
	for _, s := range c.subs {
		subs = append(subs, s)
	}
	c.mu.Unlock()
	st := c.State()
	for _, s := range subs {
		s(st)
	}
}

// OnRecord registers fn for every record of type t (a poplink.T* name)
// arriving from home. Handlers run on the link's reader goroutine, in
// order, and must not block. The extension point every Wave 3 subsystem
// uses on the PoP.
func (c *Client) OnRecord(t string, fn func(rec any)) {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	c.handlers[t] = append(c.handlers[t], fn)
}

func (c *Client) dispatch(t string, rec any) {
	c.hmu.RLock()
	hs := c.handlers[t]
	c.hmu.RUnlock()
	for _, fn := range hs {
		fn(rec)
	}
}

// Send writes rec on the link's control stream.
func (c *Client) Send(rec any) error {
	c.mu.Lock()
	st := c.stream
	c.mu.Unlock()
	if st == nil {
		return ErrNotLinked
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if d, ok := st.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(time.Now().Add(5 * time.Second))
		defer d.SetWriteDeadline(time.Time{})
	}
	return poplink.Write(st, rec)
}

func (c *Client) wake() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Run holds the link until ctx ends: it dials, relinks at once after a
// drain (4002), backs off on failures and reads Bye, 401, 403 and 426 into
// the state. With no credential it idles until Pair. It returns nil on
// cancellation.
func (c *Client) Run(ctx context.Context) error {
	if c.opts.Pair != "" {
		go c.pairAtStart(ctx, c.opts.Pair)
	}
	failures := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		c.mu.Lock()
		req := c.pending
		c.pending = nil
		st := c.stored
		c.mu.Unlock()

		var res linkResult
		switch {
		case req != nil:
			res = c.linkOnce(ctx, req.endpoint, req, nil)
		case st != nil:
			res = c.linkOnce(ctx, st.Endpoint, nil, st)
		default:
			select {
			case <-ctx.Done():
				return nil
			case <-c.kick:
			}
			continue
		}
		var delay time.Duration
		switch {
		case res.relinkNow:
			failures = 0
		case res.terminal:
			delay = c.opts.TerminalRetry
		case res.linked:
			failures = 0
			delay = c.opts.RetryMin
		default:
			delay = min(c.opts.RetryMin<<min(failures, 10), c.opts.RetryMax)
			failures++
		}
		if delay == 0 {
			continue
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-c.kick:
			t.Stop()
		case <-t.C:
		}
	}
}

// linkResult says how a link attempt ended.
type linkResult struct {
	// linked: the link came up (home sent a config).
	linked bool
	// relinkNow: home drained (4002) or the link was superseded; dial again
	// at once.
	relinkNow bool
	// terminal: a refusal that needs the operator; retry slowly.
	terminal bool
}

// linkOnce makes one link attempt: a pairing dial when req is set,
// otherwise an ordinary one with st's credential.
func (c *Client) linkOnce(ctx context.Context, endpoint string, req *pairRequest, st *storedCredential) linkResult {
	q := url.Values{"proto": {poplink.ProtoQuery()}}
	h := http.Header{"Origin": {poplink.Origin}}
	if req != nil {
		q.Set("pair", req.token)
	} else {
		h.Set("Authorization", "Bearer "+st.Credential)
		if st.Pop != "" {
			q.Set("pop", st.Pop)
		}
	}
	target := "https://" + endpoint + "/pop/link?" + q.Encode()
	sess, rsp, err := c.dial(ctx, "udp", target, h)
	if err != nil {
		return c.refused(ctx, req, rsp, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = sess.CloseWithError(0, "pop shutting down")
	}()

	ctrl, err := sess.OpenStream(ctx)
	if err == nil {
		err = poplink.Write(ctrl, c.hello())
	}
	if err != nil {
		_ = sess.CloseWithError(0, "control stream failed")
		c.failPairing(req, err)
		c.setState(func(s *LinkState) { s.Header = offlineHeader(s.Header) })
		return linkResult{}
	}
	c.mu.Lock()
	c.ctrl, c.stream = sess, ctrl
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.ctrl == sess {
			c.ctrl, c.stream = nil, nil
		}
		c.mu.Unlock()
	}()

	var res linkResult
	var bye string
	statsStop := make(chan struct{})
	defer close(statsStop)
	for {
		rec, err := poplink.Read(ctrl)
		if err != nil {
			_ = sess.CloseWithError(0, "link ended")
			return c.linkEnded(req, sess, err, bye, res)
		}
		switch r := rec.(type) {
		case *poplink.Credential:
			if req == nil {
				continue // only ever the first record of a pairing dial
			}
			nst := &storedCredential{Endpoint: endpoint, Credential: r.Credential, PairTokenSHA: pops.TokenSHA256(req.token)}
			if err := saveCredential(c.opts.StateDir, nst); err != nil {
				c.log.Error("pairing succeeded but the credential could not be stored", "err", err)
			}
			c.mu.Lock()
			c.stored = nst
			c.mu.Unlock()
			c.setState(func(s *LinkState) {
				s.Endpoint, s.ByeReason, s.LastStatus = endpoint, "", 0
				if s.Header == HeaderStandalone || s.Header == HeaderRepair || s.Header == HeaderRevoked {
					s.Header = HeaderWaiting
				}
			})
			req.finish(nil)
			req = nil
			c.log.Info("paired with home", "endpoint", endpoint)
		case *poplink.Config:
			cfg := *r
			first := !res.linked
			res.linked = true
			c.rememberPop(cfg.PopName)
			c.setState(func(s *LinkState) {
				s.Header, s.Config, s.ByeReason, s.LastStatus = HeaderLinked, &cfg, "", 0
			})
			if first {
				c.log.Info("linked to home", "endpoint", endpoint, "pop", cfg.PopName)
				go c.statsLoop(statsStop)
				go c.recordOtherFamily(ctx, endpoint, sess)
			}
		case *poplink.Bye:
			bye = r.Reason
			c.log.Info("home ended the link", "reason", bye)
			_ = sess.CloseWithError(0, "bye")
		}
		if rec != nil {
			c.dispatch(poplink.TypeOf(rec), rec)
		}
	}
}

// refused records a dial that failed, by its HTTP status.
func (c *Client) refused(ctx context.Context, req *pairRequest, rsp *http.Response, err error) linkResult {
	status, reason := 0, ""
	if rsp != nil {
		status, reason = rsp.StatusCode, rsp.Header.Get(poplink.ReasonHeader)
		if status < 300 {
			status = 0
		}
	}
	if ctx.Err() != nil {
		c.failPairing(req, ctx.Err())
		return linkResult{}
	}
	if req != nil {
		if status != 0 {
			req.finish(&PairError{Status: status, Reason: reason})
		} else {
			req.finish(err)
		}
		c.setState(func(s *LinkState) { s.LastStatus = status })
		// A refused pairing leaves the previous credential (if any) in use.
		return linkResult{relinkNow: c.hasCredential()}
	}
	c.log.Info("link dial refused", "status", status, "reason", reason, "err", err)
	switch status {
	case http.StatusUnauthorized:
		c.setState(func(s *LinkState) { s.Header, s.ByeReason, s.LastStatus = HeaderRepair, poplink.ByeRepair, status })
		return linkResult{terminal: true}
	case http.StatusForbidden:
		if reason == "" {
			reason = poplink.ByeRevoked
		}
		c.setState(func(s *LinkState) { s.Header, s.ByeReason, s.LastStatus = HeaderRevoked, reason, status })
		return linkResult{terminal: true}
	case http.StatusUpgradeRequired:
		c.setState(func(s *LinkState) { s.Header, s.LastStatus = offlineHeader(s.Header), status })
		return linkResult{terminal: true}
	}
	c.setState(func(s *LinkState) { s.Header, s.LastStatus = offlineHeader(s.Header), status })
	return linkResult{}
}

// linkEnded reads how an up link ended into the state.
func (c *Client) linkEnded(req *pairRequest, sess Session, err error, bye string, res linkResult) linkResult {
	code, coded := poplink.CloseCode(err)
	if !coded {
		code, coded = poplink.CloseCode(context.Cause(sess.Context()))
	}
	if req != nil {
		// Upgraded but the redeem lost a race: the session code is the
		// status (410 used).
		if coded && code >= 400 && code < 600 {
			req.finish(&PairError{Status: int(code), Reason: closeReason(err)})
		} else {
			req.finish(fmt.Errorf("popclient: pairing link ended before a credential: %w", err))
		}
		return linkResult{relinkNow: c.hasCredential()}
	}
	switch {
	case bye != "":
		header := HeaderRevoked
		if bye == poplink.ByeRepair {
			header = HeaderRepair
		}
		c.setState(func(s *LinkState) { s.Header, s.ByeReason = header, bye })
		res.terminal = true
		return res
	case coded && code == wire.CloseCodeServerDraining:
		// A draining home pod: relink at once, wherever the load balancer
		// sends us (docs/76 item 12).
		res.relinkNow = true
	case coded && code == 0 && res.linked:
		// Superseded (this PoP relinked elsewhere) or a clean end.
		res.relinkNow = true
	}
	c.setState(func(s *LinkState) { s.Header = offlineHeader(s.Header) })
	return res
}

func closeReason(err error) string {
	var se *webtransport.SessionError
	if errors.As(err, &se) {
		return se.Message
	}
	return ""
}

// offlineHeader keeps a header that needs the operator (repair, revoked)
// and otherwise says offline.
func offlineHeader(cur string) string {
	switch cur {
	case HeaderRepair, HeaderRevoked, HeaderStandalone:
		return cur
	}
	return HeaderOffline
}

func (c *Client) hasCredential() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stored != nil
}

// hello is the first record on every link.
func (c *Client) hello() poplink.Hello {
	h := poplink.Hello{Version: c.opts.Version, Proto: poplink.ProtoMajor}
	if c.opts.LAN != nil {
		if a := c.opts.LAN.Current(); a.IsValid() {
			h.LanAddr = a.String()
		}
		for _, cand := range c.opts.LAN.Candidates() {
			h.LanCandidates = append(h.LanCandidates, cand.Addr.String())
		}
	}
	if c.opts.Rolling != nil {
		cur, next := c.opts.Rolling.Hashes()
		for _, x := range []string{cur, next} {
			if x != "" && !slices.Contains(h.PinnedHashes, x) {
				h.PinnedHashes = append(h.PinnedHashes, x)
			}
		}
	}
	return h
}

// statsLoop reports the PoP's use every StatsEvery while linked. PP8/PP9
// fill in the uplink and downlink numbers.
func (c *Client) statsLoop(stop <-chan struct{}) {
	t := time.NewTicker(c.opts.StatsEvery)
	defer t.Stop()
	for {
		if err := c.Send(c.stats()); err != nil && !errors.Is(err, ErrNotLinked) {
			c.log.Debug("stats write failed", "err", err)
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

func (c *Client) stats() poplink.Stats {
	rc := c.opts.Relay
	return poplink.Stats{
		Uplink:   pops.Budget{MaxMbps: rc.UplinkBudget.Mbps, MaxStreams: rc.UplinkBudget.Streams},
		Downlink: pops.Budget{MaxMbps: rc.DownlinkBudget.Mbps, MaxStreams: rc.DownlinkBudget.Streams},
		// The home broadcasts pulled over the downlink now (downlink.go).
		DownlinkStreams: c.DownlinkUse().Streams,
		// Health is reported on its own record; nothing to add yet.
	}
}

// SendHealth reports the PoP's self-check (sent when it changes).
func (c *Client) SendHealth(lines []adminapi.HealthLine) error {
	return c.Send(poplink.Health{Lines: lines})
}

// recordOtherFamily dials /pop/addr over the address family the link did
// not use, when home's endpoint has one (docs/76 item 13).
func (c *Client) recordOtherFamily(ctx context.Context, endpoint string, link Session) {
	network := otherFamily(addrOf(link.RemoteAddr()))
	if network == "" {
		return
	}
	c.mu.Lock()
	st := c.stored
	c.mu.Unlock()
	if st == nil {
		return
	}
	q := url.Values{"proto": {poplink.ProtoQuery()}}
	if st.Pop != "" {
		q.Set("pop", st.Pop)
	}
	h := http.Header{"Origin": {poplink.Origin}, "Authorization": {"Bearer " + st.Credential}}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sess, _, err := c.dial(dctx, network, "https://"+endpoint+"/pop/addr?"+q.Encode(), h)
	if err != nil {
		c.log.Debug("no second address family to report", "network", network, "err", err)
		return
	}
	select {
	case <-sess.Context().Done():
	case <-dctx.Done():
	}
	_ = sess.CloseWithError(0, "")
}

// otherFamily is the dial network for the family a link over remote did
// not use ("" when remote is unknown).
func otherFamily(remote netip.Addr) string {
	switch {
	case !remote.IsValid():
		return ""
	case remote.Unmap().Is4():
		return "udp6"
	}
	return "udp4"
}

func addrOf(a net.Addr) netip.Addr {
	if u, ok := a.(*net.UDPAddr); ok && u != nil {
		return u.AddrPort().Addr().Unmap()
	}
	return netip.Addr{}
}

// Dialer is the default DialFunc with roots as the trust anchors (nil:
// the system roots); harnesses wrap it to stand in for a load balancer.
func Dialer(roots *x509.CertPool) DialFunc { return defaultDialer(roots) }

// defaultDialer dials home over QUIC with the link timers (docs/76 item
// 10: 15 s idle, 5 s keepalive; home's listener cannot set per-route
// timers, so the PoP's end sets them).
func defaultDialer(roots *x509.CertPool) DialFunc {
	return func(ctx context.Context, network, target string, h http.Header) (Session, *http.Response, error) {
		u, err := url.Parse(target)
		if err != nil {
			return nil, nil, err
		}
		host := u.Hostname()
		d := &webtransport.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host},
			QUICConfig: &quic.Config{
				EnableDatagrams:                  true,
				EnableStreamResetPartialDelivery: true,
				MaxIdleTimeout:                   poplink.LinkIdleTimeout,
				KeepAlivePeriod:                  poplink.LinkKeepAlive,
			},
		}
		if network == "udp4" || network == "udp6" {
			d.DialAddr = func(ctx context.Context, addr string, tlsCfg *tls.Config, qc *quic.Config) (*quic.Conn, error) {
				h, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupNetIP(ctx, strings.Replace(network, "udp", "ip", 1), h)
				if err != nil || len(ips) == 0 {
					return nil, fmt.Errorf("popclient: %s has no %s address: %v", h, network, err)
				}
				return quic.DialAddrEarly(ctx, net.JoinHostPort(ips[0].Unmap().String(), port), tlsCfg, qc)
			}
		}
		rsp, sess, err := d.Dial(ctx, target, h)
		if err != nil {
			_ = d.Close()
			return nil, rsp, err
		}
		return poplink.WrapSession(sess), rsp, nil
	}
}
