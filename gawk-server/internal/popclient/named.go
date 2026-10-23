package popclient

// named.go: the named tier on the PoP (docs/76 §6.11 items 38-40).
//
//   - Home's config names the host gawk-admin assigned
//     (p-<label>.<zone>). The PoP makes a P-256 key in its state directory
//     and sends a CSR for exactly that name on the link; the private key
//     never leaves the PoP. It renews at half-life with a fresh key, keeping
//     the serving pair until the new chain arrives.
//   - A certificate record is accepted only for the CSR the PoP is waiting
//     on (ForCSRSHA256) and only when its leaf carries that CSR's key and
//     names the host.
//   - With a certificate, the PoP serves the embedded web app over HTTPS on
//     TCP -ui-addr, with a generated config.js naming itself as the relay,
//     and the relay's QUIC listener answers that name with the same
//     certificate (cmd/gawk-relay wraps its GetCertificate with
//     Named.Certificate). One origin for the page and the relay.
//   - The DNS rebinding self-check resolves the name through the system
//     resolver and a public one. Guests are pointed at the name only when
//     the venue's resolver answers it with the LAN address; when only the
//     public one does, the venue filters private answers and the console
//     says so.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/relaystate"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// State-directory names of the named tier's files. The keys go through
// relaystate's secret store (DPAPI on Windows); the certificate and CSR
// are public but kept beside them so a pair is never split.
const (
	namedKeyFile     = "named-key.pem"
	namedCertFile    = "named-cert.pem"
	namedNextKeyFile = "named-next-key.pem"
	namedCSRFile     = "named-csr.pem"
)

// DNS self-check results (docs/76 item 40).
const (
	// DNSUnchecked: no check has finished yet (or there is no name).
	DNSUnchecked = "unchecked"
	// DNSOK: the venue's resolver answers the name with the LAN address.
	DNSOK = "ok"
	// DNSFiltered: only the public resolver does — the venue's resolver
	// drops public names that point at private addresses.
	DNSFiltered = "filtered"
	// DNSPending: neither does yet (the record is new, or the LAN address
	// changed and home has not republished it).
	DNSPending = "pending"
	// DNSNoLAN: there is no LAN address to compare with.
	DNSNoLAN = "no-lan"
)

// DNSFilteredNotice is the console's line for DNSFiltered (docs/76 item 40).
const DNSFilteredNotice = "This network's DNS hides the relay's name. Guests will use gawk.ioio.fi instead."

// publicResolverAddr is the public resolver the self-check asks over UDP.
const publicResolverAddr = "1.1.1.1:53"

// Resolver looks names up; *net.Resolver is one.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// PublicResolver is the self-check's public resolver: Go's resolver
// speaking plain DNS to 1.1.1.1 over UDP, whatever the system's is.
func PublicResolver() Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", publicResolverAddr)
	}}
}

// NamedOptions wire the named tier.
type NamedOptions struct {
	// StateDir holds the key, the CSR and the certificate.
	StateDir string
	// UIAddr is -ui-addr, the HTTPS listener (TCP); "" serves no page.
	UIAddr string
	// RelayPort is the relay's UDP port, for config.js (0: 4433).
	RelayPort int
	// App is the embedded web app, rooted at its index.html.
	App fs.FS
	// LAN is the advertised LAN address (default: the client's watcher).
	LAN func() netip.Addr
	// System and Public are the self-check's resolvers (default:
	// net.DefaultResolver and PublicResolver).
	System, Public Resolver
	// CheckEvery is the self-check period (default 5 min); RenewEvery how
	// often the renewal and resend timers are looked at (default 1 min);
	// ResendEvery how often an unanswered CSR is sent again (default 10 min).
	CheckEvery, RenewEvery, ResendEvery time.Duration
	Now                                 func() time.Time
	Log                                 *slog.Logger
	// Send replaces the client's Send (tests).
	Send func(rec any) error
}

// Named is the PoP's named tier.
type Named struct {
	c    *Client
	opts NamedOptions
	log  *slog.Logger
	kick chan struct{}
	have chan struct{} // closed once a certificate is held

	mu sync.Mutex
	// assigned is the host home's config names; until a config arrives,
	// the host of the certificate on disk.
	assigned string
	cert     *tls.Certificate
	certHost string
	haveOnce sync.Once
	// The request in flight: its key, PEM, parsed form, hash and host.
	nextKey  *ecdsa.PrivateKey
	csrPEM   string
	csr      *x509.CertificateRequest
	csrSHA   string
	csrHost  string
	lastSent time.Time
	dns      string
	health   []adminapi.HealthLine
}

// NewNamed builds the named tier on c and registers its link records. It
// loads any key and certificate from the state directory, so a restart
// serves its name at once, linked or not.
func NewNamed(c *Client, o NamedOptions) *Named {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.CheckEvery <= 0 {
		o.CheckEvery = 5 * time.Minute
	}
	if o.RenewEvery <= 0 {
		o.RenewEvery = time.Minute
	}
	if o.ResendEvery <= 0 {
		o.ResendEvery = 10 * time.Minute
	}
	if o.System == nil {
		o.System = net.DefaultResolver
	}
	if o.Public == nil {
		o.Public = PublicResolver()
	}
	if o.LAN == nil {
		o.LAN = func() netip.Addr {
			if c != nil && c.opts.LAN != nil {
				return c.opts.LAN.Current()
			}
			return netip.Addr{}
		}
	}
	if o.Send == nil && c != nil {
		o.Send = c.Send
	}
	n := &Named{c: c, opts: o, log: o.Log.With("component", "pop-named"), kick: make(chan struct{}, 1),
		have: make(chan struct{}), dns: DNSUnchecked}
	n.load()
	if c != nil {
		c.named.Store(n)
		c.OnRecord(poplink.TConfig, func(rec any) {
			if r, ok := rec.(*poplink.Config); ok {
				go n.onConfig(r.NamedHost)
			}
		})
		c.OnRecord(poplink.TCertificate, func(rec any) {
			if r, ok := rec.(*poplink.Certificate); ok {
				go n.onCertificate(*r)
			}
		})
		if st := c.State(); st.Header == HeaderLinked && st.Config != nil {
			go n.onConfig(st.Config.NamedHost)
		}
	}
	return n
}

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// Host is the name the PoP holds a valid certificate for and home still
// assigns; "" without one. It is what hello reports.
func (n *Named) Host() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.hostLocked()
}

func (n *Named) hostLocked() string {
	if n.cert == nil || n.certHost == "" || n.certHost != n.assigned {
		return ""
	}
	if !n.opts.Now().Before(n.cert.Leaf.NotAfter) {
		return ""
	}
	return n.certHost
}

// GuestHost is the name guests should be sent to: Host, when the venue's
// resolver answers it with the LAN address. It feeds the console's QR
// selection (relayconsole.State.NamedHost).
func (n *Named) GuestHost() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.dns != DNSOK {
		return ""
	}
	return n.hostLocked()
}

// DNSCheck is the self-check's last result (a DNS* constant).
func (n *Named) DNSCheck() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dns
}

// Notice is the console's notice line, "" when there is nothing to say.
func (n *Named) Notice() string {
	if n.DNSCheck() == DNSFiltered && n.Host() != "" {
		return DNSFilteredNotice
	}
	return ""
}

// Health is the named tier's self-check lines (docs/76 §7.1 Health).
func (n *Named) Health() []adminapi.HealthLine {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.healthLocked())
}

func (n *Named) healthLocked() []adminapi.HealthLine {
	if n.assigned == "" && n.cert == nil {
		return nil
	}
	var out []adminapi.HealthLine
	host := n.hostLocked()
	switch {
	case host != "":
		out = append(out, adminapi.HealthLine{Key: "named-certificate", OK: true,
			Detail: "Certificate: named (" + host + "), renews automatically"})
	case n.assigned != "":
		out = append(out, adminapi.HealthLine{Key: "named-certificate", OK: false,
			Detail: "Named certificate for " + n.assigned + ": waiting for the home fleet to issue it"})
	}
	if host == "" {
		return out
	}
	switch n.dns {
	case DNSOK:
		out = append(out, adminapi.HealthLine{Key: "dns", OK: true, Detail: "DNS: this network resolves " + host})
	case DNSFiltered:
		out = append(out, adminapi.HealthLine{Key: "dns", OK: false, Detail: DNSFilteredNotice})
	case DNSPending:
		out = append(out, adminapi.HealthLine{Key: "dns", OK: false,
			Detail: "DNS: " + host + " does not point at this machine yet; guests use the home link meanwhile"})
	case DNSNoLAN:
		out = append(out, adminapi.HealthLine{Key: "dns", OK: false, Detail: "DNS: no LAN address to check " + host + " against"})
	}
	return out
}

// Certificate is the named certificate for a handshake naming its host
// (nil otherwise): the relay's QUIC listener tries it before its pinned
// one, so https://<named>:4433 is one origin for the page and the relay.
func (n *Named) Certificate(hello *tls.ClientHelloInfo) *tls.Certificate {
	if hello == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	host := n.hostLocked()
	if host == "" || normalizeHost(hello.ServerName) != host {
		return nil
	}
	return n.cert
}

// wake asks Run's loop for a self-check now.
func (n *Named) wake() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

// onConfig follows home's assigned host and asks for a certificate when
// one is due. A config also follows every link-up, so an unanswered CSR is
// sent again then.
func (n *Named) onConfig(host string) {
	host = normalizeHost(host)
	n.mu.Lock()
	changed := n.assigned != host
	n.assigned = host
	n.mu.Unlock()
	n.request(true)
	if changed {
		n.wake()
		n.reportHealth()
	}
}

// request sends a CSR for the assigned host when there is no usable
// certificate for it or the one held is past half-life. force resends a
// pending one at once; otherwise every ResendEvery.
func (n *Named) request(force bool) {
	n.mu.Lock()
	host := n.assigned
	if host == "" || !n.dueLocked() {
		n.mu.Unlock()
		return
	}
	if n.csr == nil || n.csrHost != host {
		if err := n.newRequestLocked(host); err != nil {
			n.mu.Unlock()
			n.log.Error("named certificate request not made", "host", host, "err", err)
			return
		}
		force = true
	}
	if !force && n.opts.Now().Sub(n.lastSent) < n.opts.ResendEvery {
		n.mu.Unlock()
		return
	}
	pemStr, sha := n.csrPEM, n.csrSHA
	n.lastSent = n.opts.Now()
	n.mu.Unlock()
	if n.opts.Send == nil {
		return
	}
	if err := n.opts.Send(poplink.CSR{PEM: pemStr}); err != nil {
		if !errors.Is(err, ErrNotLinked) {
			n.log.Debug("named csr not sent", "err", err)
		}
		n.mu.Lock()
		n.lastSent = time.Time{}
		n.mu.Unlock()
		return
	}
	n.log.Info("named certificate requested", "host", host, "csr_sha256", sha)
}

// dueLocked: the certificate is missing, for another host, or past its
// half-life.
func (n *Named) dueLocked() bool {
	if n.cert == nil || n.certHost != n.assigned {
		return true
	}
	leaf := n.cert.Leaf
	half := leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) / 2)
	return !n.opts.Now().Before(half)
}

// newRequestLocked makes a fresh key and a CSR naming exactly host, and
// persists both before anything is sent.
func (n *Named) newRequestLocked(host string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{host}}, key)
	if err != nil {
		return err
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	csr, err := pops.ValidateCSR(csrPEM, host)
	if err != nil {
		return err // our own CSR; cannot happen
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return err
	}
	if err := relaystate.SaveSecret(n.opts.StateDir, namedNextKeyFile, keyPEM); err != nil {
		return err
	}
	if err := relaystate.SaveSecret(n.opts.StateDir, namedCSRFile, []byte(csrPEM)); err != nil {
		return err
	}
	n.nextKey, n.csrPEM, n.csr, n.csrSHA, n.csrHost = key, csrPEM, csr, pops.CSRSHA256(csrPEM), host
	n.lastSent = time.Time{}
	return nil
}

// onCertificate installs a chain that answers the pending CSR. Anything
// else — a previous chain, another host, another key — is ignored.
func (n *Named) onCertificate(rec poplink.Certificate) {
	n.mu.Lock()
	if n.csr == nil || rec.ForCSRSHA256 == "" || rec.ForCSRSHA256 != n.csrSHA {
		n.mu.Unlock()
		n.log.Debug("named certificate ignored: not for the pending request", "for", rec.ForCSRSHA256)
		return
	}
	chain, err := pops.CheckChain(rec.ChainPEM, n.csr, n.csrHost)
	if err != nil {
		n.mu.Unlock()
		n.log.Warn("named certificate refused", "err", err)
		return
	}
	cert := &tls.Certificate{PrivateKey: n.nextKey, Leaf: chain[0]}
	for _, c := range chain {
		cert.Certificate = append(cert.Certificate, c.Raw)
	}
	keyPEM, err := encodeKey(n.nextKey)
	if err == nil {
		err = relaystate.SaveSecret(n.opts.StateDir, namedKeyFile, keyPEM)
	}
	if err == nil {
		err = relaystate.SaveSecret(n.opts.StateDir, namedCertFile, []byte(rec.ChainPEM))
	}
	if err != nil {
		n.mu.Unlock()
		n.log.Error("named certificate not stored", "err", err)
		return
	}
	before := n.hostLocked()
	n.cert, n.certHost = cert, n.csrHost
	n.nextKey, n.csrPEM, n.csr, n.csrSHA, n.csrHost = nil, "", nil, "", ""
	_ = relaystate.RemoveSecret(n.opts.StateDir, namedNextKeyFile)
	_ = relaystate.RemoveSecret(n.opts.StateDir, namedCSRFile)
	after := n.hostLocked()
	n.mu.Unlock()
	n.haveOnce.Do(func() { close(n.have) })
	n.log.Info("named certificate installed", "host", after, "not_after", chain[0].NotAfter)
	if before != after && n.c != nil && n.opts.Send != nil {
		// Home's offers name the PoP's host only once it serves it.
		if err := n.opts.Send(n.c.hello()); err != nil && !errors.Is(err, ErrNotLinked) {
			n.log.Debug("hello with the named host not sent", "err", err)
		}
	}
	n.wake()
	n.reportHealth()
}

// load restores the serving pair and any pending request from disk. A
// pair that does not match, or has expired, is dropped.
func (n *Named) load() {
	if n.opts.StateDir == "" {
		return
	}
	keyPEM, err1 := relaystate.LoadSecret(n.opts.StateDir, namedKeyFile)
	chainPEM, err2 := relaystate.LoadSecret(n.opts.StateDir, namedCertFile)
	if err1 == nil && err2 == nil {
		if cert, err := tls.X509KeyPair(chainPEM, keyPEM); err == nil && cert.Leaf != nil &&
			len(cert.Leaf.DNSNames) == 1 && n.opts.Now().Before(cert.Leaf.NotAfter) {
			n.cert, n.certHost, n.assigned = &cert, normalizeHost(cert.Leaf.DNSNames[0]), normalizeHost(cert.Leaf.DNSNames[0])
			n.haveOnce.Do(func() { close(n.have) })
		}
	}
	nextPEM, err1 := relaystate.LoadSecret(n.opts.StateDir, namedNextKeyFile)
	csrPEM, err2 := relaystate.LoadSecret(n.opts.StateDir, namedCSRFile)
	if err1 != nil || err2 != nil {
		return
	}
	key, err := decodeKey(nextPEM)
	if err != nil {
		return
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || len(csr.DNSNames) != 1 {
		return
	}
	if pub, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return
	}
	n.nextKey, n.csrPEM, n.csr, n.csrSHA, n.csrHost = key, string(csrPEM), csr, pops.CSRSHA256(string(csrPEM)), normalizeHost(csr.DNSNames[0])
	if n.assigned == "" {
		// The last host home assigned, until its config says otherwise.
		n.assigned = n.csrHost
	}
}

func encodeKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func decodeKey(b []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("popclient: no PEM key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// reportHealth sends the named lines home (they show on the PoP's admin
// detail); offline it is a no-op until the next change.
func (n *Named) reportHealth() {
	n.mu.Lock()
	lines := slices.Clone(n.healthLocked())
	same := slices.Equal(lines, n.health)
	n.health = lines
	n.mu.Unlock()
	if same || n.opts.Send == nil {
		return
	}
	if err := n.opts.Send(poplink.Health{Lines: lines}); err != nil && !errors.Is(err, ErrNotLinked) {
		n.log.Debug("named health not sent", "err", err)
	}
}

// CheckDNS runs the rebinding self-check once (docs/76 item 40) and
// returns the result.
func (n *Named) CheckDNS(ctx context.Context) string {
	host := n.Host()
	lan := n.opts.LAN().Unmap()
	var res string
	switch {
	case host == "":
		res = DNSUnchecked
	case !lan.IsValid():
		res = DNSNoLAN
	default:
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		var local, public bool
		wg.Add(2)
		go func() { defer wg.Done(); local = resolves(ctx, n.opts.System, host, lan) }()
		go func() { defer wg.Done(); public = resolves(ctx, n.opts.Public, host, lan) }()
		wg.Wait()
		switch {
		case local:
			res = DNSOK
		case public:
			res = DNSFiltered
		default:
			res = DNSPending
		}
	}
	n.mu.Lock()
	changed := n.dns != res
	n.dns = res
	n.mu.Unlock()
	if changed {
		switch res {
		case DNSFiltered:
			n.log.Warn("the venue's DNS hides the named host; guests are sent to the home link", "host", host)
		case DNSOK:
			n.log.Info("named host resolves on this network", "host", host)
		}
		n.reportHealth()
	}
	return res
}

func resolves(ctx context.Context, r Resolver, host string, want netip.Addr) bool {
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if a.Unmap() == want {
			return true
		}
	}
	return false
}

// Run keeps the tier going until ctx ends: the renewal and resend timers,
// the self-check, and — once a certificate is held — the HTTPS listener.
func (n *Named) Run(ctx context.Context) error {
	if n.c != nil && n.c.opts.LAN != nil {
		cancel := n.c.opts.LAN.Subscribe(func(netip.Addr) { n.wake() })
		defer cancel()
	}
	go n.serveUI(ctx)
	renew := time.NewTicker(n.opts.RenewEvery)
	defer renew.Stop()
	check := time.NewTicker(n.opts.CheckEvery)
	defer check.Stop()
	n.CheckDNS(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-renew.C:
			n.request(false)
		case <-check.C:
			n.CheckDNS(ctx)
		case <-n.kick:
			n.CheckDNS(ctx)
		}
	}
}

// serveUI waits for a certificate, then serves the page on UIAddr. The
// listener opens only on the named tier, so a pinned-only PoP never asks
// a firewall about TCP.
func (n *Named) serveUI(ctx context.Context) {
	if n.opts.UIAddr == "" {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-n.have:
	}
	for {
		err := n.listenAndServe(ctx)
		if ctx.Err() != nil {
			return
		}
		n.log.Warn("named UI listener failed; retrying", "addr", n.opts.UIAddr, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

func (n *Named) listenAndServe(ctx context.Context) error {
	l, err := net.Listen("tcp", n.opts.UIAddr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           n.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		TLSConfig:         n.TLSConfig(),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	n.log.Info("named UI listening", "addr", l.Addr().String(), "host", n.Host())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ServeTLS(l, "", "") }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	}
}

// TLSConfig is the HTTPS listener's: the named certificate whatever the
// SNI (a browser on the LAN reaching the page by its name sends it).
func (n *Named) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			n.mu.Lock()
			defer n.mu.Unlock()
			if n.hostLocked() == "" {
				return nil, errors.New("popclient: no named certificate")
			}
			return n.cert, nil
		},
	}
}

// Handler serves the web app and its generated config.js. GET and HEAD
// only: this page is public content, it drives nothing.
func (n *Named) Handler() http.Handler {
	var files http.Handler = http.NotFoundHandler()
	if n.opts.App != nil {
		files = http.FileServerFS(n.opts.App)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path == "/config.js" {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(NamedConfigJS(n.Host(), n.opts.RelayPort))
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// NamedConfigJS is the named page's config.js (docs/76 item 39): the PoP
// by its name as the relay — a publicly trusted certificate, so no pinned
// hashes — and requirePublishSecret false, as on the console's /app/. The
// home app origin reaches the page through 0x18, not here.
func NamedConfigJS(host string, relayPort int) []byte {
	if relayPort <= 0 || relayPort > 65535 {
		relayPort = 4433
	}
	cfg := struct {
		RelayURL             string `json:"relayUrl,omitempty"`
		RequirePublishSecret bool   `json:"requirePublishSecret"`
	}{RequirePublishSecret: false}
	if host != "" {
		cfg.RelayURL = "https://" + net.JoinHostPort(host, strconv.Itoa(relayPort))
	}
	b, _ := json.Marshal(cfg)
	out := []byte("// Generated by gawk-relay for its named address (docs/76 §6.11 item 39).\nwindow.__GAWK_CONFIG__ = ")
	out = append(out, b...)
	return append(out, ";\n"...)
}

// String is for logs.
func (n *Named) String() string {
	return fmt.Sprintf("named{host=%q dns=%s}", n.Host(), n.DNSCheck())
}
