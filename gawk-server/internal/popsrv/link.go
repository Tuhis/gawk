package popsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrc"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// Close codes a home ends a link with, besides poplink.CloseCodeLinkEnd
// (4006, always after a Bye). A superseded link (another pod force-took
// the lease, or the PoP relinked to this pod) closes with 0: the PoP
// relinks on anything that is not 4006.
const closeSuperseded = 0

// writeTimeout bounds one control-stream record write.
const writeTimeout = 5 * time.Second

// link is one live PoP link on this pod.
type link struct {
	m    *Manager
	log  *slog.Logger
	name string
	uid  string
	// credSHA and tokenSHA are the credential this link authenticated with
	// and the pairing string it was paired under: a Re-pair changes the
	// spec's string, which ends the link (docs/76 item 9).
	credSHA, tokenSHA string
	gen               int64
	sess              LinkSession
	since             time.Time
	remote            netip.Addr

	// ctrl is the control stream, under wmu; ready turns true once the
	// first Config went out, so a recheck never writes before the Hello.
	ctrl  io.ReadWriteCloser
	wmu   sync.Mutex
	ready atomic.Bool

	mu          sync.Mutex
	hello       poplink.Hello
	stats       poplink.Stats
	health      []adminapi.HealthLine
	publicAddrs []string
	lastConfig  []byte
	displayName string
	autoSwitch  bool

	endOnce sync.Once
	ended   chan struct{}
}

// ServeLink runs one link on an upgraded /pop/link session until it ends.
// p is the authenticated Pop; issued is the credential a pairing dial just
// minted ("" on an ordinary link), sent once as the first record.
//
// The link force-takes status.lease on arrival (a fresh link is the ground
// truth, docs/76 item 12), renews it every LeaseRenew and closes itself
// when a renewal finds its generation superseded. A Re-pair, revoke, CR
// deletion or end date ends it with a Bye and 4006.
func (m *Manager) ServeLink(ctx context.Context, p *pops.Pop, sess LinkSession, issued string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	now := m.now()
	l := &link{
		m:           m,
		log:         m.log.With("pop", p.Name, "remote", sess.RemoteAddr().String()),
		name:        p.Name,
		uid:         string(p.UID),
		sess:        sess,
		since:       now,
		remote:      addrOf(sess.RemoteAddr()),
		ended:       make(chan struct{}),
		displayName: p.Spec.DisplayName,
		autoSwitch:  p.AutoSwitch(),
	}
	if p.Status.Credential != nil {
		l.credSHA = p.Status.Credential.SHA256
		l.tokenSHA = p.Status.Credential.PairingTokenSHA256
	}
	if l.remote.IsValid() {
		l.publicAddrs = []string{publicAddrKey(l.remote)}
	}

	gen, err := m.opts.Status.TakeLease(ctx, p.Name, m.opts.Pod, m.opts.PodAddr, now)
	if err != nil {
		_ = sess.CloseWithError(closeSuperseded, "link lease unavailable")
		return fmt.Errorf("popsrv: take lease for %s: %w", p.Name, err)
	}
	l.gen = gen
	if old := m.register(l); old != nil {
		old.close(closeSuperseded, "relinked")
	}
	defer m.unregister(l)
	defer l.release()
	l.log.Info("pop link up", "generation", gen)

	actx, acancel := context.WithTimeout(ctx, m.opts.HelloTimeout)
	ctrl, err := sess.AcceptStream(actx)
	acancel()
	if err != nil {
		l.close(closeSuperseded, "no control stream")
		return fmt.Errorf("popsrv: %s control stream: %w", p.Name, err)
	}
	l.wmu.Lock()
	l.ctrl = ctrl
	l.wmu.Unlock()
	if issued != "" {
		if err := l.write(poplink.Credential{Credential: issued}); err != nil {
			l.close(closeSuperseded, "credential write failed")
			return err
		}
	}

	// The first record must be the Hello; a PoP that never sends one is
	// closed at HelloTimeout.
	helloTimer := time.AfterFunc(m.opts.HelloTimeout, func() { l.close(closeSuperseded, "no hello") })
	rec, err := poplink.Read(ctrl)
	helloTimer.Stop()
	if err != nil {
		l.close(closeSuperseded, "no hello")
		return fmt.Errorf("popsrv: %s hello: %w", p.Name, err)
	}
	hello, ok := rec.(*poplink.Hello)
	if !ok {
		l.close(closeSuperseded, "first record is not a hello")
		return fmt.Errorf("popsrv: %s first record %T, want hello", p.Name, rec)
	}
	l.setHello(*hello)
	if err := m.opts.Status.PatchLink(ctx, p.Name, l.statusLink()); err != nil {
		l.log.Warn("pop link status write failed", "err", err)
	}
	// Re-read: the pop may have changed between the auth and the lease.
	if fresh, ok := m.pop(p.Name); ok {
		p = fresh
	}
	if reason, bye := l.verdict(p, true); bye {
		l.bye(reason)
		<-l.ended
		return nil
	}
	if err := l.sendConfig(p); err != nil {
		l.close(closeSuperseded, "config write failed")
		return err
	}
	l.ready.Store(true)

	go l.renewLoop(ctx)
	m.dispatch(p.Name, poplink.THello, hello)
	for {
		rec, err := poplink.Read(ctrl)
		if err != nil {
			l.close(closeSuperseded, "control stream ended")
			l.log.Info("pop link down", "generation", l.gen, "reason", linkEndReason(err))
			return nil
		}
		if rec == nil {
			continue // a newer minor's record type
		}
		l.handle(rec)
	}
}

func (l *link) handle(rec any) {
	t := poplink.TypeOf(rec)
	switch r := rec.(type) {
	case *poplink.Hello:
		l.setHello(*r)
	case *poplink.Stats:
		l.mu.Lock()
		l.stats = *r
		l.mu.Unlock()
	case *poplink.Health:
		l.mu.Lock()
		l.health = slices.Clone(r.Lines)
		l.mu.Unlock()
	}
	l.m.dispatch(l.name, t, rec)
}

func (l *link) setHello(h poplink.Hello) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hello = h
	if h.Health != nil {
		l.health = slices.Clone(h.Health)
	}
}

// statusLink is status.link for this link: connected, since, the PoP's
// Hello and the address the link came from. /pop/addr adds the other
// family afterwards.
func (l *link) statusLink() pops.Link {
	l.mu.Lock()
	defer l.mu.Unlock()
	since := metav1.NewTime(l.since)
	return pops.Link{
		Connected:    true,
		Since:        &since,
		Version:      l.hello.Version,
		Proto:        l.hello.Proto,
		PublicAddrs:  slices.Clone(l.publicAddrs),
		LanAddr:      l.hello.LanAddr,
		PinnedHashes: slices.Clone(l.hello.PinnedHashes),
	}
}

// write sends one record on the control stream.
func (l *link) write(rec any) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	if l.ctrl == nil {
		return ErrNotHolder
	}
	if d, ok := l.ctrl.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(time.Now().Add(writeTimeout))
		defer d.SetWriteDeadline(time.Time{})
	}
	return poplink.Write(l.ctrl, rec)
}

// sendConfig sends the Config record when it differs from the last one.
func (l *link) sendConfig(p *pops.Pop) error {
	c := l.m.config(p)
	b, _ := json.Marshal(c)
	l.mu.Lock()
	same := string(b) == string(l.lastConfig)
	l.lastConfig = b
	l.displayName = p.Spec.DisplayName
	l.autoSwitch = p.AutoSwitch()
	l.mu.Unlock()
	if same {
		return nil
	}
	return l.write(c)
}

// verdict decides whether the Pop as it now reads still admits this link:
// bye true with the reason when it does not. A Pop with a newer lease
// generation means another link took over; that is a supersede, not a
// Bye, and handled by the caller via superseded.
func (l *link) verdict(p *pops.Pop, found bool) (reason string, bye bool) {
	if !found || p == nil || string(p.UID) != l.uid {
		return poplink.ByeRevoked, true
	}
	if p.Spec.Revoked {
		return poplink.ByeRevoked, true
	}
	if p.Spec.NotAfter != nil && l.m.now().After(p.Spec.NotAfter.Time) {
		return poplink.ByeExpired, true
	}
	// A Re-pair writes a new spec.pairing; the credential stays the one
	// paired under the old string, so it is not current any more. The
	// spec is compared rather than status.credential, which an informer
	// can still show as it was before this link's own redeem.
	if p.Spec.Pairing.TokenSHA256 != l.tokenSHA {
		return poplink.ByeRepair, true
	}
	// -pops-file: the credential hash is the credential; editing it ends
	// the link. (On the k8s path a credential for the same string cannot
	// differ: a string pairs once.)
	if c := p.Status.Credential; c != nil && c.SHA256 != "" && c.PairingTokenSHA256 == l.tokenSHA &&
		!hexEqual(c.SHA256, l.credSHA) {
		return poplink.ByeRepair, true
	}
	return "", false
}

func (l *link) superseded(p *pops.Pop) bool {
	return p != nil && p.Status.Lease != nil && p.Status.Lease.Generation > l.gen
}

// recheck re-reads the Pop and ends the link, or sends a new Config.
func (l *link) recheck() {
	select {
	case <-l.ended:
		return
	default:
	}
	p, found := l.m.pop(l.name)
	if reason, bye := l.verdict(p, found); bye {
		l.bye(reason)
		return
	}
	if l.superseded(p) {
		l.close(closeSuperseded, "link superseded")
		return
	}
	if l.ready.Load() {
		if err := l.sendConfig(p); err != nil {
			l.log.Debug("pop config write failed", "err", err)
		}
	}
}

// bye sends the Bye, gives it ByeGrace to arrive (the PoP closes first
// once it has read it), then closes with 4006.
func (l *link) bye(reason string) {
	go func() {
		if err := l.write(poplink.Bye{Reason: reason}); err != nil {
			l.log.Debug("pop bye write failed", "err", err)
		}
		l.log.Info("pop link ended by home", "reason", reason)
		select {
		case <-l.sess.Context().Done():
		case <-time.After(l.m.opts.ByeGrace):
		}
		l.close(poplink.CloseCodeLinkEnd, "pop "+reason)
	}()
}

// close ends the session once.
func (l *link) close(code uint32, msg string) {
	l.endOnce.Do(func() {
		_ = l.sess.CloseWithError(code, msg)
		close(l.ended)
	})
}

// release gives the lease back (fenced on the generation, so a stale pod's
// release can never mark a newer link disconnected).
func (l *link) release() {
	l.close(closeSuperseded, "link ended")
	ctx, cancel := background()
	defer cancel()
	err := l.m.opts.Status.PatchLease(ctx, l.name, pops.Lease{}, l.gen)
	if err != nil && !errors.Is(err, popsrc.ErrLeaseLost) {
		l.log.Warn("pop lease release failed", "err", err)
	}
}

// linkedPop is the offer matcher's view of this link.
func (l *link) linkedPop() (LinkedPop, bool) {
	select {
	case <-l.ended:
		return LinkedPop{}, false
	default:
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return LinkedPop{
		Name: l.name, UID: l.uid, DisplayName: l.displayName,
		PublicAddrs:  parsePrefixes(l.publicAddrs),
		LanAddr:      l.hello.LanAddr,
		PinnedHashes: slices.Clone(l.hello.PinnedHashes),
		NamedHost:    l.hello.NamedHost,
		AutoSwitch:   l.autoSwitch,
	}, true
}

// linkEndReason is a log-friendly reason for a control stream's end.
func linkEndReason(err error) string {
	if code, ok := poplink.CloseCode(err); ok {
		return fmt.Sprintf("closed (%d)", code)
	}
	if errors.Is(err, io.EOF) {
		return "stream closed"
	}
	return err.Error()
}

func addrOf(a net.Addr) netip.Addr {
	if a == nil {
		return netip.Addr{}
	}
	if u, ok := a.(*net.UDPAddr); ok {
		return u.AddrPort().Addr().Unmap()
	}
	return sourceAddr(a.String())
}
