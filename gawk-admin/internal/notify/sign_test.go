package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
)

// goldenBody is the exact payload docs/42 §4.10 prints, plus the `summary`
// field the same section requires — the bytes a receiver signs over.
//
// It is a golden: TestGoldenPayloadBytes proves the code still produces it, and
// the signature vectors below are computed over it. A refactor that reorders a
// field, re-enables HTML escaping, or adds one changes these bytes and fails
// loudly, which is the point — every deployed receiver's signature check
// depends on them.
const goldenBody = `{"schema":"gawk.moderation-event.v1","type":"broadcast.killed",` +
	`"occurredAt":"2026-08-20T15:04:05Z","actor":"juho@example.com",` +
	`"broadcastKey":"3f9a1c2b4d5e","reason":"terms violation",` +
	`"portalUrl":"https://admin.example.com/#/broadcasts?key=3f9a1c2b4d5e",` +
	`"summary":"broadcast 3f9a1c2b4d5e was terminated by juho@example.com"}`

// goldenPendingBody is the SAME kill, recorded when its Ban CR write did not
// land: the delivery that must not claim a termination that has not happened.
//
// It is the second golden because a pending body is a wire shape too — an
// existing receiver's signature check has to hold over it, and the field a
// receiver branches on (`enforcement`) and the sentence it renders (`summary`)
// are both pinned here byte-for-byte. Note what is unchanged: everything the
// in-sync body carries, in the same order, with one field appended. That is
// what makes this additive rather than a schema revision.
const goldenPendingBody = `{"schema":"gawk.moderation-event.v1","type":"broadcast.killed",` +
	`"occurredAt":"2026-08-20T15:04:05Z","actor":"juho@example.com",` +
	`"broadcastKey":"3f9a1c2b4d5e","reason":"terms violation",` +
	`"portalUrl":"https://admin.example.com/#/broadcasts?key=3f9a1c2b4d5e",` +
	`"summary":"a kill of broadcast 3f9a1c2b4d5e was recorded by juho@example.com` +
	` — NOT enforced yet, the broadcast is still live",` +
	`"enforcement":"pending"}`

const goldenTimestamp int64 = 1755702245

// goldenEvent is the event goldenBody is rendered from. Its payload carries
// portal-only context (banId, cooldownSeconds) that must NOT survive into the
// body — see TestGoldenPayloadBytes.
func goldenEvent() store.Event {
	return store.Event{
		ID:           7,
		Type:         store.EventBroadcastKilled,
		OccurredAt:   time.Date(2026, 8, 20, 15, 4, 5, 0, time.UTC),
		Actor:        "juho@example.com",
		BroadcastKey: "3f9a1c2b4d5e",
		BroadcastID:  "ABC123",
		Payload: json.RawMessage(`{"reason":"terms violation",` +
			`"summary":"broadcast 3f9a1c2b4d5e was terminated by juho@example.com",` +
			`"banId":"11111111-2222-3333-4444-555555555555","cooldownSeconds":600}`),
	}
}

// goldenPendingEvent is goldenEvent's kill as internal/api records it when the
// CR projection failed: the same event, graded pending, with the summary the
// one summariser produces for that grade.
func goldenPendingEvent() store.Event {
	ev := goldenEvent()
	ev.Payload = json.RawMessage(`{"reason":"terms violation",` +
		`"summary":"a kill of broadcast 3f9a1c2b4d5e was recorded by juho@example.com` +
		` — NOT enforced yet, the broadcast is still live",` +
		`"enforcement":"pending",` +
		`"banId":"11111111-2222-3333-4444-555555555555","cooldownSeconds":600}`)
	return ev
}

func TestGoldenPayloadBytes(t *testing.T) {
	body, err := marshal(buildPayload(goldenEvent(), "https://admin.example.com"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(body) != goldenBody {
		t.Fatalf("payload bytes changed; every receiver's signature check depends on them\n got: %s\nwant: %s", body, goldenBody)
	}
}

// The pending body is a golden too, and the in-sync one above is unchanged by
// its existence — together they are the backward-compatibility claim that let
// `enforcement` be added without revving PayloadSchema.
func TestGoldenPendingPayloadBytes(t *testing.T) {
	body, err := marshal(buildPayload(goldenPendingEvent(), "https://admin.example.com"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(body) != goldenPendingBody {
		t.Fatalf("pending payload bytes changed\n got: %s\nwant: %s", body, goldenPendingBody)
	}
}

// TestSignatureVectors is the §4.10 test vector: fixed secret, fixed timestamp,
// fixed body, and an expected hex digest that was computed OUTSIDE this
// package (Python's hmac/hashlib) so it cannot agree with a broken signer by
// construction.
func TestSignatureVectors(t *testing.T) {
	cases := []struct {
		name      string
		secret    string
		timestamp int64
		body      string
		// want is the bare hex digest; Sign prefixes it with "sha256=".
		want string
	}{
		{
			name:      "the docs/42 §4.10 payload",
			secret:    "gawk-webhook-secret",
			timestamp: goldenTimestamp,
			body:      goldenBody,
			want:      "a29f6bb3090c782d7ceaa1b5e1738fb6d7073936eeccea577400d22af2b364ff",
		},
		{
			// Same secret, same body, ONE SECOND later: a completely
			// different digest. This is the vector that pins "the timestamp
			// is inside the signed material".
			name:      "one second later, same body",
			secret:    "gawk-webhook-secret",
			timestamp: goldenTimestamp + 1,
			body:      goldenBody,
			want:      "747b7ee4228d8105beac7517d2aedcf60f909e58eb3615edd1841674bea7e81b",
		},
		{
			// Same timestamp, same body, another webhook's key: each webhook
			// signs with its OWN secret (D9).
			name:      "a different webhook's secret",
			secret:    "a-different-secret",
			timestamp: goldenTimestamp,
			body:      goldenBody,
			want:      "a9f00957c3369db1c04a5ea0b57892631120d8dbe5bcb822485cdc4d2c3bdeb8",
		},
		{
			// The pending delivery signs like any other: the enforcement
			// grade is inside the signed material, so a receiver cannot be
			// handed a "pending" body with a signature computed over the
			// in-sync one. Computed in Python's hmac/hashlib over the UTF-8
			// bytes (the summary's em dash is multi-byte), never by calling
			// Sign.
			name:      "the §4.10 payload, enforcement pending",
			secret:    "gawk-webhook-secret",
			timestamp: goldenTimestamp,
			body:      goldenPendingBody,
			want:      "70234bab7a43c475595611badd979141d3449da3f9e8e480dc578378e27fad83",
		},
		{
			name:      "minimal",
			secret:    "s",
			timestamp: 0,
			body:      "{}",
			want:      "e79b1f685c78e46b2e3f94b5fbf00964e3e3d5ed4d1e270dcf671ac4cd2233fa",
		},
		{
			// An empty secret is a misconfiguration, not a crash: HMAC is
			// defined for it and the delivery still goes out signed (with a
			// signature the receiver will reject).
			name:      "empty secret",
			secret:    "",
			timestamp: 1,
			body:      "x",
			want:      "d356c76f2be16120eca994746a279fd612267e791d245c4745ebbf0f98fc31f2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Sign(tc.secret, tc.timestamp, []byte(tc.body))
			if want := SignaturePrefix + tc.want; got != want {
				t.Fatalf("Sign = %q, want %q", got, want)
			}
		})
	}
}

// verifyIndependently is the receiver's side of the contract, written from
// first principles: HMAC-SHA256 over `timestamp + "." + body`, compared in
// constant time. Nothing here calls Sign, so a change to the signed material
// makes this fail rather than agree.
func verifyIndependently(t *testing.T, secret, timestampHeader, signatureHeader string, body []byte) bool {
	t.Helper()
	hexDigest, ok := strings.CutPrefix(signatureHeader, "sha256=")
	if !ok {
		t.Fatalf("signature %q does not carry the sha256= prefix", signatureHeader)
	}
	got, err := hex.DecodeString(hexDigest)
	if err != nil {
		t.Fatalf("signature %q is not hex: %v", signatureHeader, err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestampHeader + "."))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func TestVerifyIndependentlyAcceptsAndRejects(t *testing.T) {
	body := []byte(goldenBody)
	sig := Sign("gawk-webhook-secret", goldenTimestamp, body)
	ts := strconv.FormatInt(goldenTimestamp, 10)

	if !verifyIndependently(t, "gawk-webhook-secret", ts, sig, body) {
		t.Fatal("an independent HMAC implementation rejected a signature Sign produced")
	}

	// Mutating the timestamp alone must invalidate the signature — that is
	// what makes a receiver's |now - timestamp| > 300 s replay window
	// enforceable rather than advisory (§4.10).
	moved := strconv.FormatInt(goldenTimestamp+1, 10)
	if verifyIndependently(t, "gawk-webhook-secret", moved, sig, body) {
		t.Fatal("the signature survived a re-dated timestamp: the timestamp is not in the signed material")
	}

	// Mutating the body must invalidate it too.
	if verifyIndependently(t, "gawk-webhook-secret", ts, sig, append(body, ' ')) {
		t.Fatal("the signature survived a modified body")
	}

	// Another webhook's secret must not verify this delivery.
	if verifyIndependently(t, "a-different-secret", ts, sig, body) {
		t.Fatal("a different secret verified the signature")
	}
}
