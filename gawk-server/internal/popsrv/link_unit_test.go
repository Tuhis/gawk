package popsrv

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrc"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// syncSource is a Pop source a test may edit while links read it.
type syncSource struct {
	mu   sync.Mutex
	pops map[string]*pops.Pop
	subs []func(string)
}

func (s *syncSource) Get(name string) (*pops.Pop, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pops[name]
	if !ok {
		return nil, false
	}
	return p.DeepCopy(), true
}

func (s *syncSource) List() []pops.Pop {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []pops.Pop
	for _, p := range s.pops {
		out = append(out, *p.DeepCopy())
	}
	return out
}

func (s *syncSource) Subscribe(fn func(string)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
	return func() {}
}

func (s *syncSource) Synced() bool { return true }

// edit changes a Pop and notifies the subscribers, as an informer would.
func (s *syncSource) edit(name string, fn func(p *pops.Pop)) {
	s.mu.Lock()
	if p, ok := s.pops[name]; ok {
		fn(p)
	}
	subs := slices.Clone(s.subs)
	s.mu.Unlock()
	for _, f := range subs {
		f(name)
	}
}

func (s *syncSource) remove(name string) {
	s.mu.Lock()
	delete(s.pops, name)
	subs := slices.Clone(s.subs)
	s.mu.Unlock()
	for _, f := range subs {
		f(name)
	}
}

// flakyStatus is a StatusWriter whose writes can be made to fail.
type flakyStatus struct {
	popsrc.StatusWriter
	mu                        sync.Mutex
	takeErr, linkErr, addrErr error
	renewErr                  error
	redeemErr                 error
}

func (f *flakyStatus) TakeLease(ctx context.Context, name, holder, addr string, now time.Time) (int64, error) {
	f.mu.Lock()
	err := f.takeErr
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return f.StatusWriter.TakeLease(ctx, name, holder, addr, now)
}

func (f *flakyStatus) PatchLease(ctx context.Context, name string, l pops.Lease, gen int64) error {
	f.mu.Lock()
	err := f.renewErr
	f.mu.Unlock()
	if err != nil && l.Holder != "" {
		return err
	}
	return f.StatusWriter.PatchLease(ctx, name, l, gen)
}

func (f *flakyStatus) PatchLink(ctx context.Context, name string, l pops.Link) error {
	f.mu.Lock()
	err := f.linkErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.StatusWriter.PatchLink(ctx, name, l)
}

func (f *flakyStatus) AddPublicAddr(ctx context.Context, name, addr string) error {
	if f.addrErr != nil {
		return f.addrErr
	}
	return f.StatusWriter.AddPublicAddr(ctx, name, addr)
}

func (f *flakyStatus) RedeemPairing(ctx context.Context, name, tokenSHA, credSHA string, now time.Time) error {
	if f.redeemErr != nil {
		return f.redeemErr
	}
	return f.StatusWriter.RedeemPairing(ctx, name, tokenSHA, credSHA, now)
}

type renewRecorder struct {
	mu  sync.Mutex
	n   int
	err error
}

func (r *renewRecorder) RenewAll(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return r.err
}

func (r *renewRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// unitHome is a Manager over an editable source and in-memory status, with
// one file Pop "lan".
type unitHome struct {
	m      *Manager
	src    *syncSource
	status *flakyStatus
	pop    *pops.Pop
}

func newUnitHome(t *testing.T, mutate func(*Options)) *unitHome {
	t.Helper()
	cred, _ := pops.NewCredential()
	p := pops.FilePop{Name: "lan", UID: fileUID, DisplayName: "LAN", CredentialSHA256: pops.CredentialSHA256(cred)}.ToPop()
	src := &syncSource{pops: map[string]*pops.Pop{"lan": &p}}
	st := &flakyStatus{StatusWriter: popsrc.NewMemStatus(src)}
	o := Options{Pod: "home-0", PodAddr: "10.0.0.1:4433", Source: src, Status: st, Log: harnessLog(t),
		HelloTimeout: 2 * time.Second, ByeGrace: 50 * time.Millisecond, LeaseRenew: time.Hour}
	if mutate != nil {
		mutate(&o)
	}
	m := New(o)
	t.Cleanup(m.Close)
	return &unitHome{m: m, src: src, status: st, pop: p.DeepCopy()}
}

// serve runs ServeLink for h's Pop over an in-memory link and returns the
// PoP's end and ServeLink's result.
func (h *unitHome) serve(t *testing.T, issued string) (poplink.Session, <-chan error) {
	t.Helper()
	pop, home := poplink.Pipe(netip.MustParseAddrPort("203.0.113.7:5000"), netip.MustParseAddrPort("10.0.0.1:4433"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	p := h.pop.DeepCopy()
	fin := make(chan struct{})
	go func() { done <- h.m.ServeLink(ctx, p, home, issued); close(fin) }()
	t.Cleanup(func() {
		cancel()
		_ = pop.CloseWithError(0, "test over")
		select {
		case <-fin:
		case <-time.After(5 * time.Second):
			t.Error("ServeLink did not return")
		}
	})
	return pop, done
}

// ctrl opens the PoP's control stream, as popclient does.
func ctrlStream(t *testing.T, pop poplink.Session) io.ReadWriteCloser {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st, err := pop.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mustWrite(t *testing.T, w io.Writer, rec any) {
	t.Helper()
	if err := poplink.Write(w, rec); err != nil {
		t.Fatal(err)
	}
}

// next reads the next record, failing the test after 3s.
func next(t *testing.T, r io.Reader) any {
	t.Helper()
	type res struct {
		rec any
		err error
	}
	ch := make(chan res, 1)
	go func() { rec, err := poplink.Read(r); ch <- res{rec, err} }()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("read: %v", got.err)
		}
		return got.rec
	case <-time.After(3 * time.Second):
		t.Fatal("no record within 3s")
		return nil
	}
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("ServeLink did not return")
		return nil
	}
}

// link up: Hello in, Config out.
func (h *unitHome) up(t *testing.T, hello poplink.Hello) (poplink.Session, io.ReadWriteCloser, <-chan error) {
	t.Helper()
	pop, done := h.serve(t, "")
	c := ctrlStream(t, pop)
	mustWrite(t, c, hello)
	if cfg, ok := next(t, c).(*poplink.Config); !ok || cfg.PopName != "lan" || cfg.DisplayName != "LAN" {
		t.Fatalf("first record after the hello: %+v", cfg)
	}
	WaitFor(t, "link ready", func() bool { l := h.m.local("lan"); return l != nil && l.ready.Load() })
	return pop, c, done
}

func closeCodeOf(t *testing.T, s poplink.Session) uint32 {
	t.Helper()
	WaitFor(t, "session closed", func() bool { return poplink.SessionEndErr(s) != nil })
	code, ok := poplink.CloseCode(poplink.SessionEndErr(s))
	if !ok {
		t.Fatalf("no close code in %v", poplink.SessionEndErr(s))
	}
	return code
}

func TestServeLinkFailsWhenTheLeaseCannotBeTaken(t *testing.T) {
	h := newUnitHome(t, nil)
	h.status.takeErr = errors.New("api down")
	pop, done := h.serve(t, "")
	if err := waitDone(t, done); err == nil || !strings.Contains(err.Error(), "take lease") {
		t.Fatalf("ServeLink = %v, want the lease error", err)
	}
	if code := closeCodeOf(t, pop); code != closeSuperseded {
		t.Fatalf("closed %d, want %d (the PoP relinks)", code, closeSuperseded)
	}
	if _, ok := h.m.HoldsLink("lan"); ok {
		t.Fatal("a link that never took its lease is registered")
	}
}

func TestServeLinkClosesAPoPThatOpensNoControlStream(t *testing.T) {
	h := newUnitHome(t, func(o *Options) { o.HelloTimeout = 50 * time.Millisecond })
	pop, done := h.serve(t, "")
	if err := waitDone(t, done); err == nil || !strings.Contains(err.Error(), "control stream") {
		t.Fatalf("ServeLink = %v", err)
	}
	if code := closeCodeOf(t, pop); code != closeSuperseded {
		t.Fatalf("closed %d", code)
	}
	if p, _ := h.status.Get(context.Background(), "lan"); p.Status.Lease == nil || p.Status.Lease.Holder != "" {
		t.Fatalf("lease after the failed link: %+v; want released", p.Status.Lease)
	}
}

func TestServeLinkWantsAHelloFirst(t *testing.T) {
	h := newUnitHome(t, nil)
	pop, done := h.serve(t, "")
	mustWrite(t, ctrlStream(t, pop), poplink.Stats{UplinkMbps: 3})
	if err := waitDone(t, done); err == nil || !strings.Contains(err.Error(), "want hello") {
		t.Fatalf("ServeLink = %v", err)
	}
}

func TestServeLinkEndsOnAControlStreamClosedBeforeTheHello(t *testing.T) {
	h := newUnitHome(t, nil)
	pop, done := h.serve(t, "")
	c := ctrlStream(t, pop)
	_ = c.Close()
	if err := waitDone(t, done); err == nil || !strings.Contains(err.Error(), "hello") {
		t.Fatalf("ServeLink = %v", err)
	}
}

// A pairing dial's credential is the first record, before the PoP's hello.
func TestServeLinkSendsTheIssuedCredentialFirst(t *testing.T) {
	h := newUnitHome(t, nil)
	pop, _ := h.serve(t, "the-credential")
	c := ctrlStream(t, pop)
	if cr, ok := next(t, c).(*poplink.Credential); !ok || cr.Credential != "the-credential" {
		t.Fatalf("first record %+v, want the credential", cr)
	}
	mustWrite(t, c, poplink.Hello{Version: "v1"})
	if _, ok := next(t, c).(*poplink.Config); !ok {
		t.Fatal("no config after the hello")
	}
}

// A Pop revoked between the auth and the hello gets a Bye and 4006, not a
// Config.
func TestServeLinkByesAPopRevokedBeforeItsHello(t *testing.T) {
	h := newUnitHome(t, nil)
	h.src.edit("lan", func(p *pops.Pop) { p.Spec.Revoked = true })
	pop, done := h.serve(t, "")
	c := ctrlStream(t, pop)
	mustWrite(t, c, poplink.Hello{})
	if bye, ok := next(t, c).(*poplink.Bye); !ok || bye.Reason != poplink.ByeRevoked {
		t.Fatalf("record %+v, want a revoked bye", bye)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatalf("ServeLink = %v; a bye is a clean end", err)
	}
	if code := closeCodeOf(t, pop); code != poplink.CloseCodeLinkEnd {
		t.Fatalf("closed %d, want 4006", code)
	}
}

// The link's records land where the console reads them: stats and health
// in the admin row, a re-sent hello in status.link; an unknown record is
// skipped; the link ending releases the lease.
func TestLinkRecordsFeedTheAdminRowAndStatus(t *testing.T) {
	h := newUnitHome(t, nil)
	h.status.linkErr = errors.New("status write refused") // the first write only warns
	pop, c, done := h.up(t, poplink.Hello{Version: "v1", Proto: 1})
	h.status.mu.Lock()
	h.status.linkErr = nil
	h.status.mu.Unlock()

	// An unknown record type keeps the stream in sync.
	future := `{"t":"future"}`
	if _, err := c.Write(append([]byte{0, 0, 0, byte(len(future))}, future...)); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, c, poplink.Stats{UplinkMbps: 12, GuestsConnected: 3, Broadcasts: []adminapi.PopBroadcast{
		{ID: "K7XQ2M", Forwarded: true, OutsideViewers: 4}, {ID: "ABC234"},
	}})
	mustWrite(t, c, poplink.Health{Lines: []adminapi.HealthLine{{Key: "uplink", OK: true}}})
	mustWrite(t, c, poplink.Hello{Version: "v2", LanAddr: "192.168.1.5:4433", PinnedHashes: []string{"aa"}})

	WaitFor(t, "admin row", func() bool {
		rows := h.m.AdminPops()
		return len(rows) == 1 && rows[0].Version == "v2" && rows[0].UplinkMbps == 12 && len(rows[0].Health) == 1
	})
	row := h.m.AdminPops()[0]
	if row.Live != 2 || row.Forwarded != 1 || row.OutsideViewers != 4 || row.GuestsConnected != 3 || row.LanAddr != "192.168.1.5:4433" {
		t.Fatalf("admin row %+v", row)
	}
	if len(row.PublicAddrs) != 1 || row.PublicAddrs[0] != "203.0.113.7" {
		t.Fatalf("public addrs %v, want the link's remote", row.PublicAddrs)
	}
	WaitFor(t, "status.link follows the re-sent hello", func() bool {
		p, _ := h.status.Get(context.Background(), "lan")
		return p.Status.Link != nil && p.Status.Link.Version == "v2" && p.Status.Link.LanAddr == "192.168.1.5:4433"
	})
	lp := h.m.Linked()
	if len(lp) != 1 || lp[0].Name != "lan" || len(lp[0].PinnedHashes) != 1 || lp[0].DisplayName != "LAN" {
		t.Fatalf("Linked = %+v", lp)
	}
	if addr, gen, ok := h.m.LinkHolder("lan"); !ok || addr != "10.0.0.1:4433" || gen != 1 {
		t.Fatalf("LinkHolder = %q %d %v", addr, gen, ok)
	}

	_ = c.Close()
	if err := waitDone(t, done); err != nil {
		t.Fatalf("ServeLink = %v", err)
	}
	if code := closeCodeOf(t, pop); code != closeSuperseded {
		t.Fatalf("closed %d", code)
	}
	p, _ := h.status.Get(context.Background(), "lan")
	if p.Status.Lease == nil || p.Status.Lease.Holder != "" || p.Status.Link.Connected {
		t.Fatalf("after the link: lease %+v link %+v; want released and disconnected", p.Status.Lease, p.Status.Link)
	}
	if rows := h.m.AdminPops(); len(rows) != 0 {
		t.Fatalf("admin rows after the link ended: %+v", rows)
	}
	if err := h.m.SendToLink("lan", poplink.Want{ID: "X"}); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("SendToLink after the link = %v", err)
	}
}

// A Pop edit re-sends Config only when it changed; a revoke byes the link.
func TestLinkRecheckFollowsTheSource(t *testing.T) {
	h := newUnitHome(t, func(o *Options) { o.HomeOrigin = "https://gawk.example" })
	pop, c, done := h.up(t, poplink.Hello{})
	h.src.edit("lan", func(p *pops.Pop) { p.Spec.DisplayName = "Renamed"; p.Spec.Limits.MaxBroadcasts = 3 })
	cfg, ok := next(t, c).(*poplink.Config)
	if !ok || cfg.DisplayName != "Renamed" || cfg.MaxBroadcasts != 3 || cfg.HomeOrigin != "https://gawk.example" {
		t.Fatalf("config after the edit: %+v", cfg)
	}
	// An edit that changes nothing the PoP sees sends nothing; the revoke
	// that follows is the next record.
	h.src.edit("lan", func(*pops.Pop) {})
	end := metav1.NewTime(time.Now().Add(-time.Minute))
	h.src.edit("lan", func(p *pops.Pop) { p.Spec.NotAfter = &end })
	if bye, ok := next(t, c).(*poplink.Bye); !ok || bye.Reason != poplink.ByeExpired {
		t.Fatalf("record %+v, want an expired bye", bye)
	}
	if code := closeCodeOf(t, pop); code != poplink.CloseCodeLinkEnd {
		t.Fatalf("closed %d, want 4006", code)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestLinkByesWhenThePopIsDeleted(t *testing.T) {
	h := newUnitHome(t, nil)
	_, c, _ := h.up(t, poplink.Hello{})
	h.src.remove("lan")
	if bye, ok := next(t, c).(*poplink.Bye); !ok || bye.Reason != poplink.ByeRevoked {
		t.Fatalf("record %+v, want a revoked bye", bye)
	}
}

// A newer lease generation in the source closes this link as superseded
// (0, so the PoP relinks), not with a Bye.
func TestLinkRecheckClosesASupersededLink(t *testing.T) {
	h := newUnitHome(t, nil)
	pop, _, done := h.up(t, poplink.Hello{})
	h.src.edit("lan", func(p *pops.Pop) { p.Status.Lease = &pops.Lease{Holder: "home-1", Generation: 7} })
	if code := closeCodeOf(t, pop); code != closeSuperseded {
		t.Fatalf("closed %d, want %d", code, closeSuperseded)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
}

// Each renewal tick renews the lease and the PoP's reservations; a failed
// write only warns, and a lease another pod took ends the link.
func TestRenewLoopRenewsAndYieldsToANewerLease(t *testing.T) {
	rr := &renewRecorder{err: errors.New("janitor down")}
	h := newUnitHome(t, func(o *Options) { o.LeaseRenew = 20 * time.Millisecond; o.Reservations = rr })
	h.status.renewErr = errors.New("api slow")
	pop, _, done := h.up(t, poplink.Hello{})
	WaitFor(t, "reservation renewals", func() bool { return rr.count() >= 2 })
	if code := poplink.SessionEndErr(pop); code != nil {
		t.Fatalf("a failed renewal ended the link: %v", code)
	}
	h.status.mu.Lock()
	h.status.renewErr = nil
	h.status.mu.Unlock()
	// Another pod force-takes the lease.
	if _, err := h.status.StatusWriter.TakeLease(context.Background(), "lan", "home-1", "10.0.0.2:4433", time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := closeCodeOf(t, pop); code != closeSuperseded {
		t.Fatalf("closed %d", code)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	if p, _ := h.status.Get(context.Background(), "lan"); p.Status.Lease.Holder != "home-1" {
		t.Fatalf("the stale link's release overwrote the newer lease: %+v", p.Status.Lease)
	}
}

// A PoP relinking to the same pod replaces its old link, which closes.
func TestARelinkToTheSamePodClosesTheOldLink(t *testing.T) {
	h := newUnitHome(t, nil)
	old, _, oldDone := h.up(t, poplink.Hello{Version: "old"})
	_, _, _ = h.up(t, poplink.Hello{Version: "new"})
	if code := closeCodeOf(t, old); code != closeSuperseded {
		t.Fatalf("old link closed %d", code)
	}
	if err := waitDone(t, oldDone); err != nil {
		t.Fatal(err)
	}
	if gen, ok := h.m.HoldsLink("lan"); !ok || gen != 2 {
		t.Fatalf("HoldsLink = %d %v; want the new link's generation 2", gen, ok)
	}
	if rows := h.m.AdminPops(); len(rows) != 1 || rows[0].Version != "new" {
		t.Fatalf("admin rows %+v", rows)
	}
}

// RecordAddr: an invalid address is ignored, a v6 one is kept as its /64 in
// memory and status, and a failed status write only warns.
func TestRecordAddr(t *testing.T) {
	h := newUnitHome(t, nil)
	_, _, _ = h.up(t, poplink.Hello{})
	h.m.RecordAddr("lan", netip.Addr{})
	h.m.RecordAddr("lan", netip.MustParseAddr("2001:db8:1:2:aaaa::1"))
	h.m.RecordAddr("lan", netip.MustParseAddr("2001:db8:1:2:bbbb::9")) // same /64
	rows := h.m.AdminPops()
	if len(rows) != 1 || len(rows[0].PublicAddrs) != 2 || rows[0].PublicAddrs[1] != "2001:db8:1:2::/64" {
		t.Fatalf("public addrs %v", rows[0].PublicAddrs)
	}
	p, _ := h.status.Get(context.Background(), "lan")
	if len(p.Status.Link.PublicAddrs) != 2 {
		t.Fatalf("status public addrs %v", p.Status.Link.PublicAddrs)
	}
	h.status.addrErr = errors.New("api down")
	h.m.RecordAddr("other", netip.MustParseAddr("198.51.100.1")) // not linked here: status only, which fails
}

// The uplink subsystem over a live link: reserve answers per ID, release
// frees, and a want is forwarded once per timeout however many ask, its
// refusal reaching every waiter.
func TestUplinkOverALink(t *testing.T) {
	r := hub.NewRegistry(harnessLog(t), hub.Options{})
	h := newUnitHome(t, func(o *Options) { o.Registry = r })
	res := NewMemReservations()
	u := h.m.StartUplink(UplinkOptions{Reservations: res, DemandTimeout: 300 * time.Millisecond})
	if u.timeout != 300*time.Millisecond {
		t.Fatal("timeout not taken")
	}
	_, c, _ := h.up(t, poplink.Hello{})

	mustWrite(t, c, poplink.Reserve{IDs: []string{"K7XQ2M", "ABC234"}})
	for _, want := range []string{"K7XQ2M", "ABC234"} {
		got, ok := next(t, c).(*poplink.Reserved)
		if !ok || got.ID != want || got.Result != poplink.ReservedOK {
			t.Fatalf("reserved %+v, want %s ok", got, want)
		}
	}
	if owner, ok := u.ReservedFor("ABC234"); !ok || owner != "lan" {
		t.Fatalf("ReservedFor = %q %v", owner, ok)
	}
	mustWrite(t, c, poplink.Release{IDs: []string{"ABC234", "NEVER1"}})
	WaitFor(t, "release", func() bool { _, ok := res.Owner("ABC234"); return !ok })
	if addr, _, ok := u.LinkHolder("lan"); !ok || addr != "10.0.0.1:4433" {
		t.Fatalf("LinkHolder = %q %v", addr, ok)
	}
	if _, ok := u.HoldsLink("lan"); !ok {
		t.Fatal("HoldsLink false on the link-home pod")
	}

	// Two pods ask; one want goes out; the refusal answers both.
	type ans struct {
		reason string
		err    error
	}
	answers := make(chan ans, 2)
	for range 2 {
		go func() {
			reason, err := u.Want(context.Background(), "lan", "K7XQ2M")
			answers <- ans{reason, err}
		}()
	}
	w, ok := next(t, c).(*poplink.Want)
	if !ok || w.ID != "K7XQ2M" {
		t.Fatalf("record %+v, want the want", w)
	}
	WaitFor(t, "both waiters", func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.waits["lan/K7XQ2M"] != nil
	})
	time.Sleep(20 * time.Millisecond) // let the second asker join the wait
	mustWrite(t, c, poplink.Refuse{ID: "K7XQ2M", Reason: "uplink-full"})
	for range 2 {
		select {
		case a := <-answers:
			if a.err != nil || a.reason != "uplink-full" {
				t.Fatalf("Want = %q %v", a.reason, a.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a waiter never got the refusal")
		}
	}
	// A refusal nobody waits on is dropped.
	mustWrite(t, c, poplink.Refuse{ID: "NOBODY", Reason: "x"})

	// An unanswered want times out as "" (the leg is coming), and a
	// cancelled asker returns at once.
	// net.Pipe is synchronous, so the PoP reads each want while Want waits.
	start := time.Now()
	unanswered := make(chan ans, 1)
	go func() {
		reason, err := u.Want(context.Background(), "lan", "ZZZ999")
		unanswered <- ans{reason, err}
	}()
	if w, ok := next(t, c).(*poplink.Want); !ok || w.ID != "ZZZ999" {
		t.Fatalf("record %+v, want the unanswered want", w)
	}
	if a := <-unanswered; a.reason != "" || a.err != nil {
		t.Fatalf("unanswered Want = %q %v", a.reason, a.err)
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("unanswered Want returned after %v, before the timeout", el)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan ans, 1)
	go func() {
		reason, err := u.Want(ctx, "lan", "YYY888")
		cancelled <- ans{reason, err}
	}()
	if w, ok := next(t, c).(*poplink.Want); !ok || w.ID != "YYY888" {
		t.Fatalf("record %+v, want the second want", w)
	}
	cancel()
	select {
	case a := <-cancelled:
		if a.reason != "" || a.err != nil {
			t.Fatalf("cancelled Want = %q %v", a.reason, a.err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("a cancelled Want waited for the timeout")
	}
	if got := u.key("K7XQ2M"); got == "" || got == "K7XQ2M" {
		t.Fatalf("key = %q, want the registry's obfuscation", got)
	}
}

func TestUplinkWantNeedsTheLink(t *testing.T) {
	h := newUnitHome(t, nil)
	u := h.m.StartUplink(UplinkOptions{})
	if u.timeout != 5*time.Second {
		t.Fatalf("default demand timeout %v", u.timeout)
	}
	if _, err := u.Want(context.Background(), "lan", "K7XQ2M"); !errors.Is(err, transport.ErrPopNotHolder) {
		t.Fatalf("Want without the link = %v", err)
	}
	// No reservation set: nothing is reserved, every reserve is a conflict,
	// release is a no-op, and there is no registry to key with.
	if _, ok := u.ReservedFor("K7XQ2M"); ok {
		t.Fatal("ReservedFor without reservations")
	}
	if got := u.reserveOne("lan", "K7XQ2M"); got != poplink.ReservedConflict {
		t.Fatalf("reserveOne without reservations = %q", got)
	}
	u.release("lan", []string{"K7XQ2M"})
	if u.key("K7XQ2M") != "" {
		t.Fatal("key without a registry")
	}
	if src := h.m.Source(); src != h.src {
		t.Fatal("Source is not the configured source")
	}
}

// A want whose write fails returns the error and leaves no pending wait.
func TestUplinkWantWriteFailure(t *testing.T) {
	h := newUnitHome(t, nil)
	u := h.m.StartUplink(UplinkOptions{Reservations: NewMemReservations(), DemandTimeout: time.Second})
	pop, c, _ := h.up(t, poplink.Hello{})
	_ = c.Close()
	_ = pop
	WaitFor(t, "link gone or write failing", func() bool {
		_, err := u.Want(context.Background(), "lan", "K7XQ2M")
		return err != nil
	})
	u.mu.Lock()
	n := len(u.waits)
	u.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d pending wants after a failed write", n)
	}
}

type failingReservations struct{ Reservations }

func (failingReservations) Owner(string) (string, bool) { return "", false }
func (failingReservations) Reserve(context.Context, string, string, int) (string, error) {
	return "", errors.New("api down")
}
func (failingReservations) Release(context.Context, string, string) error {
	return errors.New("api down")
}

// A reservation that could not be decided is a conflict (the broadcast
// stays party-only); a failed release only logs.
func TestUplinkReserveFailureIsAConflict(t *testing.T) {
	h := newUnitHome(t, nil)
	u := h.m.StartUplink(UplinkOptions{Reservations: failingReservations{}})
	if got := u.reserveOne("lan", "K7XQ2M"); got != poplink.ReservedConflict {
		t.Fatalf("reserveOne on a failing store = %q", got)
	}
	u.release("lan", []string{"K7XQ2M"})
	// Without a link the reply cannot be sent; reserve still decides.
	u.reserve("lan", []string{"K7XQ2M"})
}

func TestLinkHolderFromTheSourceLease(t *testing.T) {
	h := newUnitHome(t, nil)
	if _, _, ok := h.m.LinkHolder("lan"); ok {
		t.Fatal("LinkHolder without any lease")
	}
	h.src.edit("lan", func(p *pops.Pop) {
		p.Status.Lease = &pops.Lease{Holder: "home-1", Addr: "10.0.0.2:4433", Generation: 4}
	})
	if addr, gen, ok := h.m.LinkHolder("lan"); !ok || addr != "10.0.0.2:4433" || gen != 4 {
		t.Fatalf("LinkHolder = %q %d %v", addr, gen, ok)
	}
	if _, _, ok := h.m.LinkHolder("missing"); ok {
		t.Fatal("LinkHolder for a missing Pop")
	}
	bare := New(Options{Log: harnessLog(t)})
	if _, _, ok := bare.LinkHolder("lan"); ok {
		t.Fatal("LinkHolder with no source")
	}
	if _, ok := bare.pop("lan"); ok {
		t.Fatal("pop with no source")
	}
	if bare.findByCredential("x") != nil || bare.findByPairing("abc") != nil {
		t.Fatal("lookups with no source found a Pop")
	}
}

// Linked lists other pods' links from status, skipping disconnected,
// unleased and revoked Pops.
func TestLinkedFromStatus(t *testing.T) {
	h := newUnitHome(t, nil)
	mk := func(name string, connected bool, holder string, revoked bool) {
		h.src.mu.Lock()
		p := pops.FilePop{Name: name, UID: fileUID}.ToPop()
		p.Spec.Revoked = revoked
		p.Status.Link = &pops.Link{Connected: connected, PublicAddrs: []string{"198.51.100.4", "2001:db8::/64", "junk"}}
		p.Status.Lease = &pops.Lease{Holder: holder}
		h.src.pops[name] = &p
		h.src.mu.Unlock()
	}
	mk("b-live", true, "home-1", false)
	mk("a-live", true, "home-2", false)
	mk("down", false, "home-1", false)
	mk("free", true, "", false)
	mk("gone", true, "home-1", true)
	got := h.m.Linked()
	if len(got) != 2 || got[0].Name != "a-live" || got[1].Name != "b-live" {
		t.Fatalf("Linked = %+v", got)
	}
	if len(got[0].PublicAddrs) != 2 || got[0].PublicAddrs[0].String() != "198.51.100.4/32" {
		t.Fatalf("prefixes %v; the junk entry is skipped", got[0].PublicAddrs)
	}
}
