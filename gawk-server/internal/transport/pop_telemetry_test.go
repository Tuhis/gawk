package transport_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

var popTelemetryKey = bytes.Repeat([]byte{0x5a}, wire.TelemetryKeySize)

const popTelemetryURL = "https://telemetry.example/api/telemetry/v1/ingest"

// uniCollector keeps every uni-stream message a session receives, by type.
type uniCollector struct {
	mu   sync.Mutex
	msgs map[uint8][]byte
}

func collectUni(sess *webtransport.Session) *uniCollector {
	c := &uniCollector{msgs: map[uint8][]byte{}}
	go func() {
		for {
			st, err := sess.AcceptUniStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				data, _ := io.ReadAll(st)
				if _, typ, err := wire.PeekType(data); err == nil {
					c.mu.Lock()
					c.msgs[typ] = data
					c.mu.Unlock()
				}
			}()
		}
	}()
	return c
}

func (c *uniCollector) get(typ uint8) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.msgs[typ]
	return m, ok
}

func (c *uniCollector) await(t *testing.T, what string, typ uint8) []byte {
	t.Helper()
	var msg []byte
	popsrv.WaitFor(t, what, func() bool {
		m, ok := c.get(typ)
		msg = m
		return ok
	})
	return msg
}

func startTelemetryFleet(t *testing.T) *uplinkFleet {
	t.Helper()
	u := startUplinkFleet(t, 2, fleetOpts{home: func(c *config.Config) {
		c.TelemetryKey = popTelemetryKey
		c.TelemetryAdvertiseURL = popTelemetryURL
	}})
	for _, pod := range u.f.Pods {
		pod.Manager.StartTelemetry(popsrv.TelemetryOptions{
			Key:              popTelemetryKey,
			ReportIntervalMs: pod.Server.TelemetryReportIntervalMs(),
			AdvertiseURL:     popTelemetryURL,
			Reservations:     popsrv.NewClusterReservations(pod.Coord),
			ServesDownlink:   pod.Server.PopServesDownlink,
		})
	}
	u.pair(t)
	return u
}

// checkPopHello asserts a hello is one home minted for this PoP: it
// verifies under home's key, over home's broadcast key for id, and names the
// PoP's issuer (docs/78 D1, D3).
func checkPopHello(t *testing.T, u *uplinkFleet, msg []byte, id string, role wire.TelemetryRole) string {
	t.Helper()
	h, err := wire.ParseTelemetryHello(msg)
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	holder := u.f.LinkHolder(upPop)
	if want := holder.Registry.ObfuscateID(id); hex.EncodeToString(h.BroadcastKey) != want {
		t.Fatalf("hello broadcast key %x, want home's %s", h.BroadcastKey, want)
	}
	sid, err := wire.VerifyTelemetrySessionToken(popTelemetryKey, h.Token, h.BroadcastKey, role, time.Now())
	if err != nil {
		t.Fatalf("%s token does not verify under home's key: %v", role, err)
	}
	issuer, ok := wire.TelemetryTokenIssuer(popTelemetryKey, h.Token)
	uid := u.pop.Client.State().Config.UID
	if !ok || !bytes.Equal(issuer, wire.TelemetryIssuerKey(popTelemetryKey, uid)) {
		t.Fatalf("%s token issuer %x/%v, want the PoP's", role, issuer, ok)
	}
	return sid
}

// docs/78 PT2, PT3, PT5: a party broadcaster and its LAN viewer, and a LAN
// viewer of a home broadcast, all get hellos home minted; the PoP's
// relay-side view of their sessions reaches the link-home pod's ops route,
// keyed by home's broadcast key, with no raw ID or wall code in it.
func TestPopSessionsGetHomeMintedTelemetry(t *testing.T) {
	u := startTelemetryFleet(t)
	if cfg := u.pop.Client.State().Config; cfg.Telemetry == nil || cfg.Telemetry.AdvertiseURL != popTelemetryURL {
		t.Fatalf("config.telemetry = %+v, want home's settings", cfg.Telemetry)
	}

	// The party broadcaster.
	pubSess, err := dialWT(t, u.f.ClientTLS, "https://"+u.pop.Addr()+"/publish")
	if err != nil {
		t.Fatalf("publish on the PoP: %v", err)
	}
	pubMsgs := collectUni(pubSess)
	ann := pubMsgs.await(t, "the announce", wire.TypeBroadcastAnnounce)
	id, err := wire.ParseBroadcastAnnounce(ann)
	if err != nil {
		t.Fatal(err)
	}
	pubSID := checkPopHello(t, u, pubMsgs.await(t, "the broadcaster's hello", wire.TypeTelemetryHello), id, wire.TelemetryRoleBroadcaster)
	url, err := wire.ParseTelemetryEndpoint(pubMsgs.await(t, "the ingest URL", wire.TypeTelemetryEndpoint))
	if err != nil || url != popTelemetryURL {
		t.Fatalf("endpoint = %q, %v; want %q", url, err, popTelemetryURL)
	}

	// Its viewer on the LAN.
	vSess, err := dialWT(t, u.f.ClientTLS, "https://"+u.pop.Addr()+"/subscribe/"+id)
	if err != nil {
		t.Fatalf("party viewer: %v", err)
	}
	vSID := checkPopHello(t, u, collectUni(vSess).await(t, "the viewer's hello", wire.TypeTelemetryHello), id, wire.TelemetryRoleViewer)

	// A LAN viewer of a HOME broadcast, over the downlink.
	homeSess, err := dialWT(t, u.f.ClientTLS, "https://"+u.f.Pods[0].Addr()+"/publish")
	if err != nil {
		t.Fatalf("home publish: %v", err)
	}
	homeID, err := wire.ParseBroadcastAnnounce(collectUni(homeSess).await(t, "home's announce", wire.TypeBroadcastAnnounce))
	if err != nil {
		t.Fatal(err)
	}
	_ = homeSess.SendDatagram(wire.AppendClockMapping(nil, 1))
	dSess, err := dialWT(t, u.f.ClientTLS, "https://"+u.pop.Addr()+"/subscribe/"+homeID)
	if err != nil {
		t.Fatalf("downlink viewer: %v", err)
	}
	checkPopHello(t, u, collectUni(dSess).await(t, "the downlink viewer's hello", wire.TypeTelemetryHello), homeID, wire.TelemetryRoleViewer)

	// The PoP's relay-side view, through home.
	holder := u.f.LinkHolder(upPop)
	issuer := hex.EncodeToString(wire.TelemetryIssuerKey(popTelemetryKey, u.pop.Client.State().Config.UID))
	var body []byte
	popsrv.WaitFor(t, "the PoP's observations reach home", func() bool {
		doc := holder.Manager.Telemetry().Doc()
		po, ok := doc.Pops[issuer]
		if !ok {
			return false
		}
		raw, ok := po.Broadcasts[holder.Registry.ObfuscateID(id)]
		if !ok {
			return false
		}
		var b struct {
			PublisherSessionID string `json:"publisherSessionId"`
			SubscriberDetails  []struct {
				SessionID string `json:"sessionId"`
			} `json:"subscriberDetails"`
		}
		if json.Unmarshal(raw, &b) != nil || b.PublisherSessionID != pubSID {
			return false
		}
		for _, s := range b.SubscriberDetails {
			if s.SessionID == vSID {
				body, _ = json.Marshal(doc)
				return po.DisplayName == "Assembly 2026"
			}
		}
		return false
	})
	for _, secret := range []string{id, homeID, upPop} {
		if strings.Contains(string(body), secret) {
			t.Errorf("the ops body carries %q (a raw ID or the wall code)", secret)
		}
	}
	for _, pod := range u.f.Pods {
		if pod != holder && len(pod.Manager.Telemetry().Doc().Pops) != 0 {
			t.Errorf("%s holds no link but serves PoP observations", pod.Name)
		}
	}
}

// docs/78 D4: home mints a broadcaster token only for the PoP's own
// reservation, and no more than its bucket allows.
func TestHomeMintsOnlyWithinScopeAndRate(t *testing.T) {
	u := startTelemetryFleet(t)
	var mu sync.Mutex
	var refused []poplink.TelemetryRefused
	u.pop.Client.OnRecord(poplink.TTelemetryRefused, func(rec any) {
		if r, ok := rec.(*poplink.TelemetryRefused); ok {
			mu.Lock()
			refused = append(refused, *r)
			mu.Unlock()
		}
	})
	if err := u.pop.Client.Send(poplink.TelemetryMint{ID: "ZZZZZZ", Role: "broadcaster", N: 1}); err != nil {
		t.Fatal(err)
	}
	popsrv.WaitFor(t, "a scope refusal", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(refused) == 1 && refused[0].Reason == poplink.TelemetryRefusedScope
	})

	// Past the burst (100) a mint is refused for rate. The bucket refills at
	// 10/s while the mints go out, so a fixed 104 sent slowly enough (a
	// loaded runner under coverage, PR #595) never empties it: keep minting
	// until refused, bounded well past what the refill allows in the wait.
	p := publishOnPoP(t, u)
	u.reserved(t, p.id)
	rateRefused := func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, r := range refused {
			if r.Reason == poplink.TelemetryRefusedRate {
				return true
			}
		}
		return false
	}
	start := time.Now()
	deadline := start.Add(10 * time.Second)
	sent := 0
	for !rateRefused() {
		if time.Now().After(deadline) {
			t.Fatalf("no rate refusal after %d mints of 8", sent)
		}
		if err := u.pop.Client.Send(poplink.TelemetryMint{ID: p.id, Role: "viewer", N: 8}); err != nil {
			t.Fatal(err)
		}
		sent++
		if sent >= 13 { // the burst is spent: give the refusal time to arrive
			time.Sleep(20 * time.Millisecond)
		}
	}
	// The bucket held the burst plus what refilled while minting; the mints
	// that got through cannot have been more than that (one mint of slack).
	allowed := 100 + time.Since(start).Seconds()*10
	if got := float64((sent - 1) * 8); got > allowed+8 {
		t.Fatalf("%d tokens minted before the refusal, the bucket allows %.0f", (sent-1)*8, allowed)
	}
}

// docs/78 D6: a home that collects no telemetry tells its PoP nothing, and
// the PoP's sessions get no hello — R74's behaviour, unchanged.
func TestAPopOfAHomeWithoutTelemetrySendsNoHello(t *testing.T) {
	u := startUplinkFleet(t, 1, fleetOpts{})
	u.pair(t)
	if cfg := u.pop.Client.State().Config; cfg.Telemetry != nil {
		t.Fatalf("config.telemetry = %+v from a home with no key", cfg.Telemetry)
	}
	sess, err := dialWT(t, u.f.ClientTLS, "https://"+u.pop.Addr()+"/publish")
	if err != nil {
		t.Fatal(err)
	}
	msgs := collectUni(sess)
	msgs.await(t, "the announce", wire.TypeBroadcastAnnounce)
	time.Sleep(1500 * time.Millisecond)
	if _, ok := msgs.get(wire.TypeTelemetryHello); ok {
		t.Fatal("a PoP of a home without telemetry sent a hello")
	}
}
