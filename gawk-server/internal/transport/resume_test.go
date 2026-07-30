// R17 W2 (docs/22 Decision 7): resume tokens.
//
// The two route tests below were written test-first against the pre-W2
// behavior they fix (CODE-REVIEW.md): reclaiming a graced ID needed no proof
// of ownership beyond the global publish secret (the hijack), and an ID the
// process didn't know 404'd even for its legitimate owner (a relay restart
// orphaned every broadcast).
package transport

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func TestResumeTokenMintVerify(t *testing.T) {
	withSecret := config.Config{PublishSecret: "test-secret"}
	rt := newResumeTokens(withSecret)

	token := rt.mint("K7XQ2M")
	if len(token) != resumeTokenBytes {
		t.Fatalf("minted token = %d bytes, want %d", len(token), resumeTokenBytes)
	}
	tokenHex := hex.EncodeToString(token)

	if !rt.verify("K7XQ2M", tokenHex) {
		t.Error("valid token rejected")
	}
	if rt.verify("ABC234", tokenHex) {
		t.Error("token accepted for a different broadcast ID")
	}
	if rt.verify("K7XQ2M", "") {
		t.Error("empty token accepted")
	}
	if rt.verify("K7XQ2M", "zznothex") {
		t.Error("non-hex token accepted")
	}
	if rt.verify("K7XQ2M", tokenHex[:16]) {
		t.Error("truncated token accepted")
	}

	// Stateless across instances: any pod with the same secret verifies.
	rt2 := newResumeTokens(withSecret)
	if !rt2.verify("K7XQ2M", tokenHex) {
		t.Error("token minted by one instance rejected by another with the same publish secret")
	}
	// A different secret revokes everything (the rotation story).
	rtOther := newResumeTokens(config.Config{PublishSecret: "rotated"})
	if rtOther.verify("K7XQ2M", tokenHex) {
		t.Error("token accepted after publish-secret rotation")
	}
}

func TestResumeTokenKeyModes(t *testing.T) {
	// Explicit resume-token key (no publish secret): shared across instances.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	a := newResumeTokens(config.Config{ResumeTokenKey: key})
	b := newResumeTokens(config.Config{ResumeTokenKey: key})
	tok := hex.EncodeToString(a.mint("K7XQ2M"))
	if !b.verify("K7XQ2M", tok) {
		t.Error("shared resume-token key: token not portable across instances")
	}

	// An explicitly-provisioned key WINS over the publish secret (PR #47
	// security review): the publish secret is distributed to every
	// broadcaster, so a key derived from it is computable by every
	// broadcaster — tokens would gate nothing between secret-holders. The
	// chart key never leaves the server side, which is what makes the token
	// a real per-broadcast ownership proof.
	c := newResumeTokens(config.Config{PublishSecret: "s", ResumeTokenKey: key})
	if !c.verify("K7XQ2M", tok) {
		t.Error("explicit resume-token key ignored when a publish secret is set")
	}

	// Dev fallback: per-process random — instances don't agree.
	d := newResumeTokens(config.Config{})
	e := newResumeTokens(config.Config{})
	if e.verify("K7XQ2M", hex.EncodeToString(d.mint("K7XQ2M"))) {
		t.Error("per-process random keys unexpectedly agree")
	}
	// ...but one instance verifies its own mints (today's process-lifetime reclaim).
	if !d.verify("K7XQ2M", hex.EncodeToString(d.mint("K7XQ2M"))) {
		t.Error("per-process key does not verify its own token")
	}
}

// readPublisherHandshake reads the two server-initiated uni streams (announce
// + resume token, R17 W2) and returns the broadcast ID and hex token,
// dispatching by wire type so stream arrival order doesn't matter.
func readPublisherHandshake(t *testing.T, ctx context.Context, sess *webtransport.Session) (id, tokenHex string) {
	t.Helper()
	// Loop until BOTH are in hand rather than reading a fixed number of
	// streams: the relay sends a publisher other session-start messages too
	// (R29 RelayCapabilities, R28 TelemetryHello), and webtransport-go does
	// not accept in open order (docs/22 finding 9). A fixed count silently
	// becomes wrong the next time a message is added — which is exactly how
	// this helper broke when R29 landed.
	for id == "" || tokenHex == "" {
		acceptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		stream, err := sess.AcceptUniStream(acceptCtx)
		cancel()
		if err != nil {
			t.Fatalf("AcceptUniStream: %v", err)
		}
		data, err := io.ReadAll(stream)
		if err != nil {
			t.Fatalf("read uni stream: %v", err)
		}
		_, typ, err := wire.PeekType(data)
		if err != nil {
			t.Fatalf("PeekType: %v", err)
		}
		switch typ {
		case wire.TypeBroadcastAnnounce:
			id, err = wire.ParseBroadcastAnnounce(data)
			if err != nil {
				t.Fatalf("ParseBroadcastAnnounce: %v", err)
			}
		case wire.TypeResumeToken:
			token, err := wire.ParseResumeToken(data)
			if err != nil {
				t.Fatalf("ParseResumeToken: %v", err)
			}
			tokenHex = hex.EncodeToString(token)
		default:
			// Any other server-initiated message is legitimate and not this
			// helper's business. Skipping it keeps the handshake assertion
			// about the handshake.
		}
	}
	if id == "" || tokenHex == "" {
		t.Fatalf("incomplete publisher handshake: id=%q tokenSet=%v", id, tokenHex != "")
	}
	return id, tokenHex
}

// The publisher handshake delivers a resume token alongside the announce.
func TestPublishHandshakeDeliversResumeToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	port, clientTLS, _, _ := startTestServer(t, ctx, 2)

	sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	defer sess.CloseWithError(0, "")
	id, tokenHex := readPublisherHandshake(t, ctx, sess)
	if len(id) != 6 {
		t.Errorf("announced ID %q, want 6 chars", id)
	}
	if len(tokenHex) != resumeTokenBytes*2 {
		t.Errorf("token hex length = %d, want %d", len(tokenHex), resumeTokenBytes*2)
	}
}

// The hijack regression test (docs/22 Background): with W2, knowing a
// broadcast ID (viewers do) plus the publish secret must no longer be enough
// to take over a disconnected broadcaster's ID — /publish/{id} without a
// valid resume token is rejected even while the hub sits in grace.
func TestReclaimWithoutTokenRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 2)

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	id, _ := readPublisherHandshake(t, ctx, pub)
	pub.CloseWithError(0, "") // graced, publisher slot free

	waitFor(t, 5*time.Second, func() bool {
		return !r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherActive
	}, "publisher inactive")

	// No resume param at all.
	rsp, sess, err := dialOnce(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish/%s", port, id), clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("tokenless reclaim of a graced ID succeeded; want rejection")
	}
	if rsp != nil && rsp.StatusCode != 403 {
		t.Errorf("tokenless reclaim status = %d, want 403", rsp.StatusCode)
	}

	// A wrong token is just as dead.
	badToken := "00000000000000000000000000000000"
	rsp, sess, err = dialOnce(t, ctx,
		fmt.Sprintf("https://127.0.0.1:%d/publish/%s?resume=%s", port, id, badToken), clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("wrong-token reclaim succeeded; want rejection")
	}
	if rsp != nil && rsp.StatusCode != 403 {
		t.Errorf("wrong-token reclaim status = %d, want 403", rsp.StatusCode)
	}
}

// A valid token still reclaims a graced hub (the manual-restart flow).
func TestReclaimWithTokenSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 2)

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	id, tokenHex := readPublisherHandshake(t, ctx, pub)
	pub.CloseWithError(0, "")

	waitFor(t, 5*time.Second, func() bool {
		return !r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherActive
	}, "publisher inactive")

	pub2 := dial(t, ctx,
		fmt.Sprintf("https://127.0.0.1:%d/publish/%s?resume=%s", port, id, tokenHex), clientTLS)
	defer pub2.CloseWithError(0, "")
	id2, token2 := readPublisherHandshake(t, ctx, pub2)
	if id2 != id {
		t.Errorf("reclaim announced %q, want %q", id2, id)
	}
	if token2 != tokenHex {
		t.Errorf("reclaim re-minted a different token (deterministic HMAC expected)")
	}
}

// The restart-orphan fix (docs/22 Background, the headline of W2): a pod
// that has never seen a broadcast ID accepts a valid-token claim and CREATES
// the hub — broadcasts survive relay restarts and re-home across pods. Two
// servers share the publish secret, standing in for two pods (or the same
// pod before/after a restart).
func TestResumeUnknownIDWithTokenCreatesHub(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := config.Config{
		MaxSubscribers:  2,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
		PublishSecret:   "shared-secret",
	}
	portA, tlsA, _, _ := startTestServerCfg(t, ctx, cfg)
	portB, tlsB, registryB, _ := startTestServerCfg(t, ctx, cfg)

	// Mint on A.
	pubA := dial(t, ctx,
		fmt.Sprintf("https://127.0.0.1:%d/publish?secret=shared-secret", portA), tlsA)
	id, tokenHex := readPublisherHandshake(t, ctx, pubA)
	pubA.CloseWithError(0, "")

	// Claim on B, which has never heard of this ID.
	pubB := dial(t, ctx,
		fmt.Sprintf("https://127.0.0.1:%d/publish/%s?secret=shared-secret&resume=%s", portB, id, tokenHex), tlsB)
	defer pubB.CloseWithError(0, "")
	idB, _ := readPublisherHandshake(t, ctx, pubB)
	if idB != id {
		t.Fatalf("resume on fresh server announced %q, want %q", idB, id)
	}

	// The hub exists on B: a viewer can subscribe to the same URL.
	if err := registryB.CheckSubscribe(id); err != nil {
		t.Fatalf("CheckSubscribe(%s) on the resumed server: %v", id, err)
	}

	// And without a valid token, B still refuses to invent hubs (404-era
	// behavior for strangers is now 403).
	rsp, sess, err := dialOnce(t, ctx,
		fmt.Sprintf("https://127.0.0.1:%d/publish/%s?secret=shared-secret", portB, "AAAAAA"), tlsB)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("tokenless unknown-ID claim succeeded; want rejection")
	}
	if rsp != nil && rsp.StatusCode != 403 {
		t.Errorf("tokenless unknown-ID claim status = %d, want 403", rsp.StatusCode)
	}
}
