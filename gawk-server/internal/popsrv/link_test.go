package popsrv

// The PP7 acceptance rows (docs/76 §11) in-process: three home pods on a
// fake clientset with the docs/22 finding-11 reactors, and one PoP.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/relaystate"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const popName = "assembly-2026"

func pairCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitHeader(t *testing.T, c *popclient.Client, header string) popclient.LinkState {
	t.Helper()
	WaitFor(t, "link header "+header, func() bool { return c.State().Header == header })
	return c.State()
}

// pairLinked pairs the harness PoP and waits for the link.
func pairLinked(t *testing.T, f *HomeFleet, pop *PoP) {
	t.Helper()
	s := f.CreatePop(t, popName, "Assembly 2026", time.Hour)
	if err := pop.Client.Pair(pairCtx(t), s); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	waitHeader(t, pop.Client, popclient.HeaderLinked)
}

// rawDial dials a home pod's /pop/* route as a PoP would, returning the
// refusal's status and reason.
func rawDial(t *testing.T, f *HomeFleet, pod *HomePod, path string, h http.Header) (*webtransport.Session, int, string) {
	t.Helper()
	d := &webtransport.Transport{
		TLSClientConfig: f.ClientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { d.Close() })
	if h == nil {
		h = http.Header{}
	}
	h.Set("Origin", poplink.Origin)
	rsp, sess, err := d.Dial(pairCtx(t), "https://"+pod.Addr()+path, h)
	if err != nil {
		if rsp == nil {
			t.Fatalf("dial %s: %v", path, err)
		}
		return nil, rsp.StatusCode, rsp.Header.Get(poplink.ReasonHeader)
	}
	t.Cleanup(func() { sess.CloseWithError(0, "") })
	return sess, http.StatusOK, ""
}

func TestPairLinksAndADrainedRelinkForceTakesOnAnotherPod(t *testing.T) {
	f, pop := StartHomeAndPop(t, 3)
	f.Route(0)
	pairLinked(t, f, pop)

	p := f.GetPop(t, popName)
	if p.Status.Credential == nil || p.Status.Credential.PairingTokenSHA256 != p.Spec.Pairing.TokenSHA256 ||
		p.Status.Credential.PairedAt == nil {
		t.Fatalf("status.credential = %+v", p.Status.Credential)
	}
	if l := p.Status.Lease; l == nil || l.Holder != "home-0" || l.Generation != 1 || l.Addr != f.Pods[0].Addr() {
		t.Fatalf("status.lease = %+v", p.Status.Lease)
	}
	if l := p.Status.Link; l == nil || !l.Connected || l.Version != "test" || l.Proto != poplink.ProtoMajor ||
		len(l.PublicAddrs) != 1 || l.PublicAddrs[0] != "127.0.0.1" {
		t.Fatalf("status.link = %+v", p.Status.Link)
	}
	// The credential never lands in status, only its hash; the PoP keeps
	// it in its state directory.
	raw, err := relaystate.LoadSecret(pop.StateDir, popclient.CredentialSecret)
	if err != nil {
		t.Fatalf("stored credential: %v", err)
	}
	var stored struct{ Endpoint, Credential, Pop string }
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Endpoint != HarnessEndpoint || pops.CredentialSHA256(stored.Credential) != p.Status.Credential.SHA256 {
		t.Fatalf("stored = %+v", stored)
	}
	WaitFor(t, "pop name stored", func() bool {
		raw, _ := relaystate.LoadSecret(pop.StateDir, popclient.CredentialSecret)
		return strings.Contains(string(raw), `"pop":"`+popName+`"`)
	})
	cfg := pop.Client.State().Config
	if cfg == nil || cfg.PopName != popName || cfg.DisplayName != "Assembly 2026" || cfg.HomeOrigin != HarnessHomeOrigin ||
		cfg.UID != string(p.UID) || !cfg.HomeRooms || cfg.MaxBroadcasts != pops.DefaultMaxBroadcasts ||
		cfg.Uplink.MaxMbps != pops.DefaultUplinkMbps {
		t.Fatalf("config = %+v", cfg)
	}

	// Drain the link-home pod; the load balancer now sends the PoP to
	// home-1, which force-takes the lease.
	f.Route(1)
	f.Pods[0].Drain(t)
	WaitFor(t, "relink on home-1", func() bool {
		return f.LinkHolder(popName) == f.Pods[1] && pop.Client.State().Header == popclient.HeaderLinked
	})
	WaitFor(t, "lease force-taken by home-1", func() bool {
		l := f.GetPop(t, popName).Status.Lease
		return l != nil && l.Holder == "home-1" && l.Generation == 2
	})
	// home-0's release came first or was fenced: the link stays connected.
	if l := f.GetPop(t, popName).Status.Link; l == nil || !l.Connected {
		t.Fatalf("after the relink: status.link = %+v", l)
	}
	if got := pop.Dialed(); len(got) < 2 || got[0] != "home-0" || got[len(got)-1] != "home-1" {
		t.Errorf("link dials = %v", got)
	}
}

// A pod that finds its lease generation superseded closes its stale link,
// so only one pod ever serves a PoP.
func TestASupersededLinkCloses(t *testing.T) {
	f, pop := StartHomeAndPop(t, 2)
	f.Route(0)
	pairLinked(t, f, pop)
	// Another link (the same PoP's credential, from a second process) lands
	// on home-1 and force-takes.
	raw, _ := relaystate.LoadSecret(pop.StateDir, popclient.CredentialSecret)
	var stored struct{ Credential string }
	_ = json.Unmarshal(raw, &stored)
	f.Route(1)
	other := startClient(t, f, t.TempDir(), func(o *popclient.Options) {})
	if err := other.Pair(pairCtx(t), popclient.CredentialPrefix+HarnessEndpoint+"/"+stored.Credential); err != nil {
		t.Fatal(err)
	}
	waitHeader(t, other, popclient.HeaderLinked)
	WaitFor(t, "home-0 drops its stale link", func() bool { return f.Pods[0].Manager.local(popName) == nil })
	if l := f.GetPop(t, popName).Status.Lease; l.Holder != "home-1" || l.Generation < 2 {
		t.Fatalf("lease = %+v", l)
	}
}

func TestPairingRefusalsAreLegible(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1)
	ctx := pairCtx(t)

	// Unknown string.
	tok, _ := pops.NewPairingToken()
	var pe *popclient.PairError
	if err := pop.Client.Pair(ctx, pops.FormatPairing(HarnessEndpoint, tok)); !errors.As(err, &pe) ||
		pe.Status != http.StatusNotFound || pe.Reason != poplink.ReasonUnknown {
		t.Fatalf("unknown string: %v", err)
	}
	// Expired string.
	expired := f.CreatePop(t, "expired-pop", "Expired", -time.Minute)
	if err := pop.Client.Pair(ctx, expired); !errors.As(err, &pe) ||
		pe.Status != http.StatusGone || pe.Reason != poplink.ReasonPairingExpired {
		t.Fatalf("expired string: %v", err)
	}
	if h := pop.Client.State().Header; h != popclient.HeaderStandalone {
		t.Errorf("after refusals the relay is %q, want standalone", h)
	}
	// A good string pairs; the same string again is used.
	s := f.CreatePop(t, popName, "Assembly 2026", time.Hour)
	if err := pop.Client.Pair(ctx, s); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	waitHeader(t, pop.Client, popclient.HeaderLinked)
	if err := pop.Client.Pair(ctx, s); !errors.As(err, &pe) || pe.Status != http.StatusGone || pe.Reason != poplink.ReasonUsed {
		t.Fatalf("used string: %v", err)
	}
	// The refused re-pairing leaves the paired credential in use.
	waitHeader(t, pop.Client, popclient.HeaderLinked)

	// A wrong credential is 401 with the repair reason.
	_, status, reason := rawDial(t, f, f.Pods[0], "/pop/link?proto=1&pop="+popName,
		http.Header{"Authorization": {"Bearer not-the-credential"}})
	if status != http.StatusUnauthorized || reason != poplink.ByeRepair {
		t.Fatalf("wrong credential: %d %q", status, reason)
	}
	// No credential at all, too.
	if _, status, _ := rawDial(t, f, f.Pods[0], "/pop/link?proto=1", nil); status != http.StatusUnauthorized {
		t.Fatalf("no credential: %d", status)
	}
}

// spec.pairing.expiresAt only limits how long an unused string can be
// redeemed: a credential paired in time keeps linking after it.
func TestACredentialPairedInTimeOutlivesExpiresAt(t *testing.T) {
	f, pop := StartHomeAndPop(t, 2)
	f.Route(0)
	pairLinked(t, f, pop)
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	f.UpdateSpec(t, popName, func(s *pops.PopSpec) { s.Pairing.ExpiresAt = &past })
	f.Route(1)
	f.Pods[0].Drain(t)
	WaitFor(t, "relink after expiresAt", func() bool {
		return f.LinkHolder(popName) == f.Pods[1] && pop.Client.State().Header == popclient.HeaderLinked
	})
}

// Re-pair refuses the old credential at once: the live link gets
// bye{repair} and 4006, and a dial with it is 401. Two Re-pairs in the same
// second differ: only the last string pairs.
func TestRePairRefusesTheOldCredentialAtOnce(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1)
	pairLinked(t, f, pop)
	raw, _ := relaystate.LoadSecret(pop.StateDir, popclient.CredentialSecret)
	var stored struct{ Credential string }
	_ = json.Unmarshal(raw, &stored)

	first := f.RePair(t, popName, time.Hour)
	second := f.RePair(t, popName, time.Hour)
	if first == second {
		t.Fatal("two Re-pairs in the same second produced the same string")
	}
	st := waitHeader(t, pop.Client, popclient.HeaderRepair)
	if st.ByeReason != poplink.ByeRepair {
		t.Errorf("bye reason = %q, want repair", st.ByeReason)
	}
	_, status, reason := rawDial(t, f, f.Pods[0], "/pop/link?proto=1&pop="+popName,
		http.Header{"Authorization": {"Bearer " + stored.Credential}})
	if status != http.StatusUnauthorized || reason != poplink.ByeRepair {
		t.Fatalf("old credential after Re-pair: %d %q", status, reason)
	}
	var pe *popclient.PairError
	if err := pop.Client.Pair(pairCtx(t), first); !errors.As(err, &pe) || pe.Status != http.StatusNotFound {
		t.Fatalf("the superseded string: %v", err)
	}
	if err := pop.Client.Pair(pairCtx(t), second); err != nil {
		t.Fatalf("the current string: %v", err)
	}
	waitHeader(t, pop.Client, popclient.HeaderLinked)
}

// The console tells bye{repair} from bye{revoked}, and recovers the reason
// from 401 / 403 when the bye is lost (here: the PoP is down when home
// acts, and reads the status on its next start).
func TestTheConsoleTellsByeReasonsApartAndRecoversThemFromStatuses(t *testing.T) {
	t.Run("bye revoked", func(t *testing.T) {
		f, pop := StartHomeAndPop(t, 1)
		pairLinked(t, f, pop)
		f.UpdateSpec(t, popName, func(s *pops.PopSpec) { s.Revoked = true })
		st := waitHeader(t, pop.Client, popclient.HeaderRevoked)
		if st.ByeReason != poplink.ByeRevoked {
			t.Errorf("reason = %q", st.ByeReason)
		}
	})
	t.Run("bye expired", func(t *testing.T) {
		f, pop := StartHomeAndPop(t, 1)
		pairLinked(t, f, pop)
		past := metav1.NewTime(time.Now().Add(-time.Minute))
		f.UpdateSpec(t, popName, func(s *pops.PopSpec) { s.NotAfter = &past })
		st := waitHeader(t, pop.Client, popclient.HeaderRevoked)
		if st.ByeReason != poplink.ByeExpired {
			t.Errorf("reason = %q", st.ByeReason)
		}
	})
	t.Run("lost bye, 403", func(t *testing.T) {
		f, pop := StartHomeAndPop(t, 1)
		pairLinked(t, f, pop)
		pop.Stop(t)
		f.UpdateSpec(t, popName, func(s *pops.PopSpec) { s.Revoked = true })
		c := startClient(t, f, pop.StateDir, nil)
		st := waitHeader(t, c, popclient.HeaderRevoked)
		if st.LastStatus != http.StatusForbidden || st.ByeReason != poplink.ByeRevoked {
			t.Errorf("state = %+v", st)
		}
	})
	t.Run("lost bye, 401", func(t *testing.T) {
		f, pop := StartHomeAndPop(t, 1)
		pairLinked(t, f, pop)
		pop.Stop(t)
		f.RePair(t, popName, time.Hour)
		c := startClient(t, f, pop.StateDir, nil)
		st := waitHeader(t, c, popclient.HeaderRepair)
		if st.LastStatus != http.StatusUnauthorized || st.ByeReason != poplink.ByeRepair {
			t.Errorf("state = %+v", st)
		}
	})
}

// A restarted PoP relinks with its stored credential and does not try to
// redeem a -pair string it already paired with (GAWK_PAIR stays set in a
// container).
func TestARestartedPoPRelinksWithItsStoredCredential(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1)
	s := f.CreatePop(t, popName, "Assembly 2026", time.Hour)
	if err := pop.Client.Pair(pairCtx(t), s); err != nil {
		t.Fatal(err)
	}
	waitHeader(t, pop.Client, popclient.HeaderLinked)
	pop.Stop(t)
	c := startClient(t, f, pop.StateDir, func(o *popclient.Options) { o.Pair = s })
	waitHeader(t, c, popclient.HeaderLinked)
	time.Sleep(300 * time.Millisecond)
	if st := c.State(); st.Header != popclient.HeaderLinked || st.LastStatus != 0 {
		t.Fatalf("state after restart = %+v", st)
	}
}

// A paired PoP's 0x18 has linked set and the home origin; an unpaired or
// revoked one has it clear (docs/76 PP7).
func TestLocalRelayInfoFollowsTheLink(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1)
	info := echoInfo(t, f, pop)
	if info.Linked || info.HomeOrigin != "" || info.Name != "Party relay" {
		t.Fatalf("standalone 0x18 = %+v", info)
	}
	pairLinked(t, f, pop)
	info = echoInfo(t, f, pop)
	if !info.Linked || info.HomeOrigin != HarnessHomeOrigin || info.Name != "Assembly 2026" {
		t.Fatalf("linked 0x18 = %+v", info)
	}
	f.UpdateSpec(t, popName, func(s *pops.PopSpec) { s.Revoked = true })
	waitHeader(t, pop.Client, popclient.HeaderRevoked)
	info = echoInfo(t, f, pop)
	if info.Linked || info.HomeOrigin != "" || info.Name != "Party relay" {
		t.Fatalf("revoked 0x18 = %+v", info)
	}
}

func echoInfo(t *testing.T, f *HomeFleet, pop *PoP) wire.LocalRelayInfo {
	t.Helper()
	sess := clientDial(t, f, "https://"+pop.Addr()+"/echo")
	defer sess.CloseWithError(0, "")
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err := sess.AcceptUniStream(ctx)
		cancel()
		if err != nil {
			t.Fatalf("no 0x18 on /echo: %v", err)
		}
		data, _ := io.ReadAll(st)
		if len(data) >= 2 && data[1] == wire.TypeLocalRelayInfo {
			info, err := wire.ParseLocalRelayInfo(data)
			if err != nil {
				t.Fatal(err)
			}
			return info
		}
	}
}

func clientDial(t *testing.T, f *HomeFleet, target string) *webtransport.Session {
	t.Helper()
	d := &webtransport.Transport{
		TLSClientConfig: f.ClientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { d.Close() })
	_, sess, err := d.Dial(pairCtx(t), target, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", target, err)
	}
	return sess
}

// GET /internal/admin/pops on the link-home pod lists the link with the
// PoP's reported stats; other pods list nothing.
func TestAdminPopsListsTheLinkOnItsHomePod(t *testing.T) {
	f, pop := StartHomeAndPop(t, 2, WithPopClient(func(o *popclient.Options) {
		o.Relay.UplinkBudget.Mbps = 10
	}))
	f.Route(1)
	pairLinked(t, f, pop)
	if rows := f.Pods[0].Manager.AdminPops(); len(rows) != 0 {
		t.Fatalf("home-0 lists %+v", rows)
	}
	WaitFor(t, "stats reach home", func() bool {
		rows := f.Pods[1].Manager.AdminPops()
		return len(rows) == 1 && rows[0].Uplink.MaxMbps == 10
	})
	row := f.Pods[1].Manager.AdminPops()[0]
	p := f.GetPop(t, popName)
	if row.Name != popName || row.UID != string(p.UID) || !row.Linked || row.Since == nil || row.Version != "test" ||
		len(row.PublicAddrs) != 1 || row.Uplink.MaxStreams != pops.DefaultUplinkStreams {
		t.Fatalf("row = %+v", row)
	}
	if lp := f.Pods[0].Manager.Linked(); len(lp) != 1 || lp[0].Name != popName || lp[0].UID != string(p.UID) ||
		len(lp[0].PublicAddrs) != 1 || !lp[0].AutoSwitch {
		t.Fatalf("Linked on another pod = %+v", lp)
	}
	if addr, gen, ok := f.Pods[0].Manager.LinkHolder(popName); !ok || addr != f.Pods[1].Addr() || gen != 1 {
		t.Fatalf("LinkHolder from home-0 = %q %d %v", addr, gen, ok)
	}
	if err := f.Pods[0].Manager.SendToLink(popName, poplink.Want{ID: "ABC234"}); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("SendToLink on a non-holder = %v", err)
	}
}

// Records home sends reach the PoP's OnRecord handlers, and the PoP's reach
// home's OnLinkRecord handlers: the extension points Wave 3 builds on.
func TestLinkRecordsReachTheirHandlers(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1)
	wants := make(chan string, 1)
	pop.Client.OnRecord(poplink.TWant, func(rec any) { wants <- rec.(*poplink.Want).ID })
	reserves := make(chan []string, 1)
	f.Pods[0].Manager.OnLinkRecord(poplink.TReserve, func(p string, rec any) {
		if p == popName {
			reserves <- rec.(*poplink.Reserve).IDs
		}
	})
	pairLinked(t, f, pop)
	if err := f.Pods[0].Manager.SendToLink(popName, poplink.Want{ID: "ABC234"}); err != nil {
		t.Fatal(err)
	}
	if err := pop.Client.Send(poplink.Reserve{IDs: []string{"XYZ789"}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case id := <-wants:
			if id != "ABC234" {
				t.Errorf("want = %q", id)
			}
		case ids := <-reserves:
			if len(ids) != 1 || ids[0] != "XYZ789" {
				t.Errorf("reserve = %v", ids)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a record never reached its handler")
		}
	}
}

// /pop/addr records the dial's address in status.link.publicAddrs; a v6
// one as its /64.
func TestPopAddrRecordsThePublicAddress(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1)
	pairLinked(t, f, pop)
	raw, _ := relaystate.LoadSecret(pop.StateDir, popclient.CredentialSecret)
	var stored struct{ Credential string }
	_ = json.Unmarshal(raw, &stored)
	f.Pods[0].Manager.RecordAddr(popName, mustAddr("2001:db8:1:2::77"))
	sess, status, _ := rawDial(t, f, f.Pods[0], "/pop/addr?proto=1&pop="+popName,
		http.Header{"Authorization": {"Bearer " + stored.Credential}})
	if status != http.StatusOK {
		t.Fatalf("/pop/addr = %d", status)
	}
	<-sess.Context().Done()
	addrs := f.GetPop(t, popName).Status.Link.PublicAddrs
	if len(addrs) != 2 || addrs[0] != "127.0.0.1" || addrs[1] != "2001:db8:1:2::/64" {
		t.Fatalf("publicAddrs = %v", addrs)
	}
	if lp := f.Pods[0].Manager.Linked(); len(lp) != 1 || len(lp[0].PublicAddrs) != 2 ||
		!lp[0].PublicAddrs[1].Contains(mustAddr("2001:db8:1:2:ffff::1")) {
		t.Fatalf("Linked = %+v", lp)
	}
	// Without a credential, /pop/addr is refused like the link.
	if _, status, _ := rawDial(t, f, f.Pods[0], "/pop/addr?proto=1", nil); status != http.StatusUnauthorized {
		t.Fatalf("/pop/addr without a credential = %d", status)
	}
}

// With home's -rooms off, /room/new on a linked PoP mints a local room:
// config says home runs no rooms, and room control stays on the PoP.
func TestHomeRoomsOffKeepsRoomsLocalToTheLinkedPoP(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1, WithHomeConfig(func(c *config.Config) { c.Rooms = false }))
	pairLinked(t, f, pop)
	if cfg := pop.Client.State().Config; cfg == nil || cfg.HomeRooms {
		t.Fatalf("config = %+v", cfg)
	}
	ctx := pairCtx(t)
	pub := clientDial(t, f, "https://"+pop.Addr()+"/publish")
	defer pub.CloseWithError(0, "")
	id, token := publisherHandshake(t, ctx, pub)
	q := url.Values{"broadcast": {id}, "resume": {token}, "label": {"pc"}}
	room := clientDial(t, f, "https://"+pop.Addr()+"/room/new?"+q.Encode())
	defer room.CloseWithError(0, "")
	st := roomHello(t, ctx, room)
	if len(st.Code) != 6 || len(st.Attachments) != 1 || st.Attachments[0].BroadcastID != id {
		t.Fatalf("room state = %+v", st)
	}
	if !pop.Rooms.Has(st.Code) {
		t.Fatalf("room %s is not on the PoP", st.Code)
	}
}

func publisherHandshake(t *testing.T, ctx context.Context, sess *webtransport.Session) (id, tokenHex string) {
	t.Helper()
	for id == "" || tokenHex == "" {
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := sess.AcceptUniStream(actx)
		cancel()
		if err != nil {
			t.Fatalf("publisher handshake: %v", err)
		}
		data, _ := io.ReadAll(st)
		_, typ, err := wire.PeekType(data)
		if err != nil {
			continue
		}
		switch typ {
		case wire.TypeBroadcastAnnounce:
			id, _ = wire.ParseBroadcastAnnounce(data)
		case wire.TypeResumeToken:
			tok, _ := wire.ParseResumeToken(data)
			tokenHex = fmt.Sprintf("%x", tok)
		}
	}
	return id, tokenHex
}

func roomHello(t *testing.T, ctx context.Context, sess *webtransport.Session) wire.RoomState {
	t.Helper()
	stream, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := wire.AppendRoomHello(nil, wire.RoomHello{Protocol: wire.RoomProtocolVersion, ClientKind: wire.RoomClientWebViewer, Nickname: "host"})
	rec, _ := wire.AppendRoomRecord(nil, hello)
	if _, err := stream.Write(rec); err != nil {
		t.Fatal(err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(stream, hdr[:]); err != nil {
		t.Fatalf("room record: %v", err)
	}
	n, _ := wire.ParseRoomRecordLength(hdr[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatal(err)
	}
	st, err := wire.ParseRoomState(buf)
	if err != nil {
		t.Fatalf("RoomState: %v", err)
	}
	return st
}

// startClient runs a second PoP link client on stateDir, dialing through
// the harness's load balancer (a restarted relay).
func startClient(t *testing.T, f *HomeFleet, stateDir string, mutate func(*popclient.Options)) *popclient.Client {
	t.Helper()
	base := popclient.Dialer(f.Pool)
	o := popclient.Options{
		StateDir: stateDir, Version: "test", Log: harnessLog(t),
		Dial: func(ctx context.Context, network, target string, h http.Header) (popclient.Session, *http.Response, error) {
			pod := f.target()
			if pod == nil {
				return nil, nil, errors.New("no home pod")
			}
			return base(ctx, network, strings.Replace(target, HarnessEndpoint, pod.Addr(), 1), h)
		},
		RetryMin: 50 * time.Millisecond, RetryMax: 500 * time.Millisecond, TerminalRetry: 500 * time.Millisecond,
		StatsEvery: 200 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&o)
	}
	c := popclient.New(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return c
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }
