package transport

// R39 AP2 (docs/42 §4.3): publish-path ban enforcement.
//
// Every rejection here is pre-upgrade, so these run as plain httptest
// requests — the same harness outcomes_test.go uses for the secret and
// resume-token gates.

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// bannedTestID is a valid broadcast ID (the 31-symbol alphabet, length 6).
const bannedTestID = "ABC23Z"

func banSet(t *testing.T, recs ...moderation.Record) *moderation.Set {
	t.Helper()
	set := moderation.NewSet()
	for _, r := range recs {
		if err := set.Upsert(r); err != nil {
			t.Fatalf("Upsert(%+v): %v", r.Target, err)
		}
	}
	return set
}

func idBan(id, reason string) moderation.Record {
	return moderation.Record{
		Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: id},
		Reason: reason, CreatedBy: "juho@example.com",
	}
}

func ipBan(cidr, reason string) moderation.Record {
	return moderation.Record{
		Target: moderation.Target{Type: moderation.TargetIP, Value: cidr},
		Reason: reason, CreatedBy: "juho@example.com",
	}
}

// A banned ID is rejected with 451 BEFORE resume.verify runs — proven with a
// VALID resume token, which is the case that matters: the token is
// HMAC(key, broadcastID), so a killed broadcaster still holds a good one and
// checking the ban after verification would let it resurrect the broadcast
// (docs/42 §4.1 step 4).
func TestPublishBannedIDRejectedBeforeResumeVerify(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t, config.Config{}, hub.Options{})
	srv.SetModeration(banSet(t, idBan(bannedTestID, "fraudulent stream")))

	token := hex.EncodeToString(srv.resume.mint(bannedTestID))
	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq(
		"https://relay/publish/"+bannedTestID+"?resume="+token,
		map[string]string{"id": bannedTestID}))

	if w.Code != http.StatusUnavailableForLegalReasons {
		t.Fatalf("status = %d, want 451", w.Code)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeBanned); got != 1 {
		t.Errorf("publish/banned = %v, want 1", got)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeUnauthorized); got != 0 {
		t.Errorf("publish/unauthorized = %v, want 0 — the ban must short-circuit the token gate", got)
	}

	// Control: the very same token, with the ban lifted, sails past the
	// resume gate (and dies at the upgrade, as a non-WebTransport request
	// must). That is what proves the token above was genuinely valid.
	srv2, sm2, _ := newOutcomeServer(t, config.Config{}, hub.Options{})
	token2 := hex.EncodeToString(srv2.resume.mint(bannedTestID))
	w = httptest.NewRecorder()
	srv2.handlePublish(w, connectReq(
		"https://relay/publish/"+bannedTestID+"?resume="+token2,
		map[string]string{"id": bannedTestID}))
	if got := sm2.ConnectionCount("publish", metrics.OutcomeUpgradeFailed); got != 1 {
		t.Fatalf("control: publish/upgrade_failed = %v, want 1 (the token was not accepted)", got)
	}
}

// A banned ID with an INVALID token is 451, not 403: the ordering again, from
// the other side. A 403 here would mean the token gate ran first.
func TestPublishBannedIDBeatsTokenRejection(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t, config.Config{}, hub.Options{})
	srv.SetModeration(banSet(t, idBan(bannedTestID, "fraudulent stream")))

	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq(
		"https://relay/publish/"+bannedTestID+"?resume=deadbeef",
		map[string]string{"id": bannedTestID}))
	if w.Code != http.StatusUnavailableForLegalReasons {
		t.Fatalf("status = %d, want 451 (403 means the token gate ran first)", w.Code)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeBanned); got != 1 {
		t.Errorf("publish/banned = %v, want 1", got)
	}
}

// A ban on a DIFFERENT ID must not touch this one.
func TestPublishUnbannedIDUnaffected(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t, config.Config{}, hub.Options{})
	srv.SetModeration(banSet(t, idBan("ZZZ23Z", "someone else")))

	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq(
		"https://relay/publish/"+bannedTestID+"?resume=deadbeef",
		map[string]string{"id": bannedTestID}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (the normal tokenless rejection)", w.Code)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeBanned); got != 0 {
		t.Errorf("publish/banned = %v, want 0", got)
	}
}

// An IP ban gates BOTH the mint and the claim path — an IP is the only handle
// that spans a re-mint loop (docs/42 D4), so a mint-path hole would make it
// useless.
func TestPublishBannedIPRejectedOnMintAndClaim(t *testing.T) {
	// connectReq dials from 203.0.113.20; ban the /24 it sits in so the test
	// also exercises a non-/32 prefix.
	for _, tc := range []struct {
		name   string
		target string
		values map[string]string
	}{
		{"mint", "https://relay/publish", nil},
		{"claim", "https://relay/publish/" + bannedTestID, map[string]string{"id": bannedTestID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, sm, _ := newOutcomeServer(t, config.Config{}, hub.Options{})
			srv.SetModeration(banSet(t, ipBan("203.0.113.0/24", "abusive host")))

			target := tc.target
			if tc.values != nil {
				target += "?resume=" + hex.EncodeToString(srv.resume.mint(bannedTestID))
			}
			w := httptest.NewRecorder()
			srv.handlePublish(w, connectReq(target, tc.values))

			if w.Code != http.StatusUnavailableForLegalReasons {
				t.Fatalf("status = %d, want 451", w.Code)
			}
			if got := sm.ConnectionCount("publish", metrics.OutcomeBanned); got != 1 {
				t.Errorf("publish/banned = %v, want 1", got)
			}
		})
	}
}

// The IP gate must not fire on an unrelated peer.
func TestPublishUnbannedIPUnaffected(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t, config.Config{}, hub.Options{})
	srv.SetModeration(banSet(t, ipBan("198.51.100.0/24", "someone else")))

	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq("https://relay/publish", nil))
	if w.Code == http.StatusUnavailableForLegalReasons {
		t.Fatal("an unrelated CIDR ban rejected the publisher")
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeBanned); got != 0 {
		t.Errorf("publish/banned = %v, want 0", got)
	}
}

// The publish secret is checked first: a banned peer without the secret is
// still 401, so the ban gate never becomes an oracle for "is this a valid
// secret?" (and the ordering matches docs/42 §4.3's diagram).
func TestPublishSecretGateStillRunsFirst(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t, config.Config{PublishSecret: "hunter2"}, hub.Options{})
	srv.SetModeration(banSet(t, ipBan("203.0.113.0/24", "abusive host")))

	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq("https://relay/publish?secret=wrong", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeBanned); got != 0 {
		t.Errorf("publish/banned = %v, want 0", got)
	}
}

// Expiry is evaluated at check time against the relay's own clock, with no
// janitor involved (docs/42 §6).
func TestPublishBanExpiresWithoutAJanitor(t *testing.T) {
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	exp := base.Add(10 * time.Minute)

	srv, sm, _ := newOutcomeServer(t, config.Config{}, hub.Options{})
	rec := idBan(bannedTestID, "10 minute cooldown")
	rec.ExpiresAt = &exp
	srv.SetModeration(banSet(t, rec))

	now := base
	srv.now = func() time.Time { return now }

	token := hex.EncodeToString(srv.resume.mint(bannedTestID))
	req := func() *http.Request {
		return connectReq("https://relay/publish/"+bannedTestID+"?resume="+token,
			map[string]string{"id": bannedTestID})
	}

	w := httptest.NewRecorder()
	srv.handlePublish(w, req())
	if w.Code != http.StatusUnavailableForLegalReasons {
		t.Fatalf("during the cooldown: status = %d, want 451", w.Code)
	}

	// Past the expiry the very same CR is still in the Set — nothing deleted
	// it — and the claim must go through.
	now = exp.Add(time.Second)
	w = httptest.NewRecorder()
	srv.handlePublish(w, req())
	if w.Code == http.StatusUnavailableForLegalReasons {
		t.Fatal("after the expiry: still 451; expiry must be evaluated lazily at check time")
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeBanned); got != 1 {
		t.Errorf("publish/banned = %v, want exactly the one rejection", got)
	}
}

// The ban REASON carries operator-private context and must never reach Warn
// (docs/42 §4.3). Debug is where it lives.
func TestPublishBanReasonNeverLoggedAtWarn(t *testing.T) {
	const secretReason = "reported by xyzzy-private-source, pending police report"

	newSrv := func(level slog.Level) (*Server, *syncBuffer) {
		buf := &syncBuffer{}
		log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level}))
		r := hub.NewRegistry(discardLog, hub.Options{})
		srv := New(config.Config{}, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil },
			log, metrics.NewServerMetrics(prometheus.NewRegistry()))
		srv.SetModeration(banSet(t, idBan(bannedTestID, secretReason)))
		return srv, buf
	}

	srv, buf := newSrv(slog.LevelWarn)
	srv.handlePublish(httptest.NewRecorder(), connectReq(
		"https://relay/publish/"+bannedTestID, map[string]string{"id": bannedTestID}))
	out := buf.String()
	if !strings.Contains(out, "publish rejected: banned") {
		t.Fatalf("the rejection was not logged at Warn at all:\n%s", out)
	}
	if strings.Contains(out, secretReason) || strings.Contains(out, "xyzzy") {
		t.Fatalf("the ban reason leaked into Warn-level output:\n%s", out)
	}
	if !strings.Contains(out, "target_type=broadcastId") {
		t.Errorf("Warn output is missing the target type:\n%s", out)
	}
	if !strings.Contains(out, "203.0.113.20") {
		t.Errorf("Warn output is missing the remote:\n%s", out)
	}

	// ...and it IS available to an operator who turns Debug on, so the
	// reason is withheld from the default log, not thrown away.
	srv, buf = newSrv(slog.LevelDebug)
	srv.handlePublish(httptest.NewRecorder(), connectReq(
		"https://relay/publish/"+bannedTestID, map[string]string{"id": bannedTestID}))
	if !strings.Contains(buf.String(), "xyzzy") {
		t.Fatalf("the ban reason is missing from Debug-level output:\n%s", buf.String())
	}
}

// A relay with no moderation source configured must behave exactly as it did
// before R39: a nil ban set, and no ban check cost.
func TestPublishWithoutModerationSourceIsUnchanged(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t, config.Config{}, hub.Options{})
	if srv.bans != nil {
		t.Fatal("a Server with no SetModeration call must carry a nil ban set")
	}
	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq("https://relay/publish/"+bannedTestID,
		map[string]string{"id": bannedTestID}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want the pre-R39 403", w.Code)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeBanned); got != 0 {
		t.Errorf("publish/banned = %v, want 0", got)
	}
}

func TestRemoteIPParsing(t *testing.T) {
	tests := []struct {
		in   string
		want string // "" = invalid
	}{
		{"203.0.113.7:40000", "203.0.113.7"},
		{"[2001:db8::1]:40000", "2001:db8::1"},
		{"203.0.113.7", "203.0.113.7"}, // no port
		{"[::ffff:203.0.113.7]:40000", "203.0.113.7"},
		{"[fe80::1%eth0]:40000", "fe80::1"}, // zone stripped
		{"not-an-address:1", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := remoteIP(tt.in)
		if tt.want == "" {
			if got.IsValid() {
				t.Errorf("remoteIP(%q) = %v, want invalid", tt.in, got)
			}
			continue
		}
		if got.String() != tt.want {
			t.Errorf("remoteIP(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// AP3 consumes this bookkeeping to kill live sessions on an IP ban
// (docs/42 §4.3), so AP2 records it and pins it here.
func TestTrackPublisherRecordsRemoteIP(t *testing.T) {
	srv := New(config.Config{Addr: "127.0.0.1:0"}, nil, nil, discardLog, nil)
	sess := &fakeDrainSession{}
	untrack := srv.trackPublisher(bannedTestID, sess, netip.MustParseAddr("203.0.113.7"))

	got, ok := srv.publisherRemote(bannedTestID)
	if !ok || got.String() != "203.0.113.7" {
		t.Fatalf("publisherRemote = %v/%v, want 203.0.113.7/true", got, ok)
	}
	untrack()
	if _, ok := srv.publisherRemote(bannedTestID); ok {
		t.Error("publisherRemote still reports a publisher after untrack")
	}

	// An unparseable RemoteAddr yields no address rather than a bogus one.
	untrack = srv.trackPublisher(bannedTestID, sess, netip.Addr{})
	defer untrack()
	if _, ok := srv.publisherRemote(bannedTestID); ok {
		t.Error("an invalid remote was reported as usable")
	}
}

// The wiring proof for the bookkeeping above: a real publisher over QUIC must
// land in s.publishers with the address the relay actually saw, not a zero
// value — a field populated only by the unit test would be dead in
// production (the R2 lesson).
func TestPublisherRemoteIPRecordedEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
	}, discardLog)

	_, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	waitFor(t, 5*time.Second, func() bool {
		_, ok := srv.publisherRemote(id)
		return ok
	}, "publisher remote address recorded")

	got, _ := srv.publisherRemote(id)
	if !got.IsLoopback() {
		t.Fatalf("publisherRemote(%s) = %v, want the loopback address the test dialed from", id, got)
	}
}
