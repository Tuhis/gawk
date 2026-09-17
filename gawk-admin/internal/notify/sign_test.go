package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// goldenID is the CloudEvents id of goldenEvent: UUIDv5 over row 7 in
// eventNamespace, computed with Python's uuid.uuid5 rather than by calling
// EventID.
const goldenID = "3c2e21c2-5f9a-5242-9ecd-5871bb85c4ac"

// goldenBody is the exact delivery body of goldenEvent — the CloudEvent whose
// `data` is the D4 projection of a broadcast.killed row (docs/52 D1, D4):
// the bytes a receiver signs over.
//
// It is a golden: TestGoldenDeliveryBytes proves the code still produces it,
// and the signature vectors below are computed over it. A refactor that
// reorders an attribute, re-enables HTML escaping, or lets a raw ID through
// changes these bytes and fails loudly, which is the point — every deployed
// receiver's signature check depends on them.
const goldenBody = `{"specversion":"1.0","id":"` + goldenID + `","source":"/gawk/admin",` +
	`"type":"fi.ioio.gawk.broadcast.killed","subject":"3f9a1c2b4d5e",` +
	`"time":"2026-08-20T15:04:05Z","datacontenttype":"application/json",` +
	`"dataschema":"https://gawk.ioio.fi/schemas/events/fi.ioio.gawk.broadcast.killed.json",` +
	`"data":{"actor":"juho@example.com","broadcastKey":"3f9a1c2b4d5e",` +
	`"portalUrl":"https://admin.example.com/#/broadcasts?key=3f9a1c2b4d5e",` +
	`"reason":"terms violation","summary":"broadcast 3f9a1c2b4d5e was terminated by juho@example.com"}}`

// goldenPendingBody is the SAME kill, recorded when its Ban CR write did not
// land: the delivery that must not claim a termination that has not happened.
// Same envelope, one more data property (`enforcement`), a graded summary —
// additive, so the type and the dataschema do not move.
const goldenPendingBody = `{"specversion":"1.0","id":"` + goldenID + `","source":"/gawk/admin",` +
	`"type":"fi.ioio.gawk.broadcast.killed","subject":"3f9a1c2b4d5e",` +
	`"time":"2026-08-20T15:04:05Z","datacontenttype":"application/json",` +
	`"dataschema":"https://gawk.ioio.fi/schemas/events/fi.ioio.gawk.broadcast.killed.json",` +
	`"data":{"actor":"juho@example.com","broadcastKey":"3f9a1c2b4d5e","enforcement":"pending",` +
	`"portalUrl":"https://admin.example.com/#/broadcasts?key=3f9a1c2b4d5e",` +
	`"reason":"terms violation","summary":"a kill of broadcast 3f9a1c2b4d5e was recorded by juho@example.com` +
	` — NOT enforced yet, the broadcast is still live"}}`

const goldenTimestamp int64 = 1755702245

// goldenEvent is the row goldenBody is rendered from. Its payload carries
// portal-only context (banId, cooldownSeconds) and its BroadcastID column the
// raw ID — none of which may survive into the body.
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

func goldenPendingEvent() store.Event {
	ev := goldenEvent()
	ev.Payload = json.RawMessage(`{"reason":"terms violation",` +
		`"summary":"a kill of broadcast 3f9a1c2b4d5e was recorded by juho@example.com` +
		` — NOT enforced yet, the broadcast is still live",` +
		`"enforcement":"pending",` +
		`"banId":"11111111-2222-3333-4444-555555555555","cooldownSeconds":600}`)
	return ev
}

func render(t *testing.T, ev store.Event, externalURL string) []byte {
	t.Helper()
	event, err := buildEvent(ev, externalURL)
	if err != nil {
		t.Fatalf("buildEvent: %v", err)
	}
	body, err := events.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func TestGoldenDeliveryBytes(t *testing.T) {
	if got := string(render(t, goldenEvent(), "https://admin.example.com")); got != goldenBody {
		t.Fatalf("delivery bytes changed; every receiver's signature check depends on them\n got: %s\nwant: %s", got, goldenBody)
	}
	if got := string(render(t, goldenPendingEvent(), "https://admin.example.com")); got != goldenPendingBody {
		t.Fatalf("pending delivery bytes changed\n got: %s\nwant: %s", got, goldenPendingBody)
	}
	if EventID(7) != goldenID {
		t.Fatalf("EventID(7) = %s, want %s: the id is derived from the row, and a receiver's dedup depends on it", EventID(7), goldenID)
	}
}

// whsecKey is 24 known bytes, and whsecSecret is how an operator writes them.
var whsecKey = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23}

const whsecSecret = "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYX"

// TestSignatureVectors is the docs/52 D7 Standard Webhooks vector: fixed key,
// id, timestamp and body, and an expected signature that was computed
// OUTSIDE this package (`openssl dgst -sha256 -hmac … | base64`, and Python's
// hmac for the whsec_ case) so it cannot agree with a broken signer by
// construction.
func TestSignatureVectors(t *testing.T) {
	cases := []struct {
		name      string
		secret    string
		id        string
		timestamp int64
		body      string
		// want is the bare base64 MAC; Sign prefixes it with "v1,".
		want string
	}{
		{
			name:      "the golden delivery",
			secret:    "Z2F3ay13ZWJob29rLXNlY3JldA==",
			id:        goldenID,
			timestamp: goldenTimestamp,
			body:      goldenBody,
			want:      "NZgDeVu2pXZkmgQVJh9HOy6/Ln0V0HOo6oTRGvLhL9Q=",
		},
		{
			// Same key, same id, same body, ONE SECOND later: a completely
			// different MAC. This pins "the timestamp is inside the signed
			// material".
			name:      "one second later, same body",
			secret:    "Z2F3ay13ZWJob29rLXNlY3JldA==",
			id:        goldenID,
			timestamp: goldenTimestamp + 1,
			body:      goldenBody,
			want:      "9/fjkhhvMpyzPTZqlSQZRqATMol3q/ICmbNgUDVqoOo=",
		},
		{
			// Each webhook signs with ITS OWN secret (docs/42 D9).
			name:      "a different webhook's secret",
			secret:    "YS1kaWZmZXJlbnQtc2VjcmV0",
			id:        goldenID,
			timestamp: goldenTimestamp,
			body:      goldenBody,
			want:      "Fy2vyCOi2nt8uOVf5j5tlxqWaVoQzFeAX6So9YuXZck=",
		},
		{
			// The key rule (D5): the key is the base64-DECODED bytes after
			// the optional prefix, which is what a Standard Webhooks library
			// derives from the same string.
			name:      "a whsec_ secret signs with the decoded bytes",
			secret:    whsecSecret,
			id:        goldenID,
			timestamp: goldenTimestamp,
			body:      goldenBody,
			want:      "3EBhhBteJnZ2JvArrmynf3riuIfh6yqODdc0gggRZ/E=",
		},
		{
			// The prefix is optional and changes nothing: the same base64
			// without it is the same key and the same signature — exactly
			// what the reference libraries compute for either spelling.
			name:      "the same secret without the prefix signs identically",
			secret:    "AAECAwQFBgcICQoLDA0ODxAREhMUFRYX",
			id:        goldenID,
			timestamp: goldenTimestamp,
			body:      goldenBody,
			want:      "3EBhhBteJnZ2JvArrmynf3riuIfh6yqODdc0gggRZ/E=",
		},
		{
			name:      "minimal",
			secret:    "cw==", // "s"
			id:        "a",
			timestamp: 0,
			body:      "{}",
			want:      "ow1swrtWnZmKFZcko18mA1wojNcE8D29Ai4qiwodcJw=",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := config.SigningKey(tc.secret)
			if err != nil {
				t.Fatalf("SigningKey: %v", err)
			}
			got := Sign(key, tc.id, tc.timestamp, []byte(tc.body))
			if want := SignatureVersion + "," + tc.want; got != want {
				t.Fatalf("Sign = %q, want %q", got, want)
			}
		})
	}

	// The derivation matters: signing with the secret STRING verbatim is a
	// different MAC (openssl over the same material with the string as the
	// key), and a receiver's library — which always decodes — would reject
	// it. This is the PR #327 review finding, pinned.
	verbatim := Sign([]byte(whsecSecret), goldenID, goldenTimestamp, []byte(goldenBody))
	if verbatim != SignatureVersion+",Jf94dVQSMaYNwdpiXoeCKJXFpG1p+CtUvE+HHUdHyGo=" {
		t.Fatalf("the verbatim-key control vector moved: %s", verbatim)
	}
	if key, _ := config.SigningKey(whsecSecret); Sign(key, goldenID, goldenTimestamp, []byte(goldenBody)) == verbatim {
		t.Fatal("SigningKey did not decode the secret: it signed with the string verbatim")
	}
}

// TestSigningKeyRule: strip an optional whsec_ prefix, then ALWAYS decode
// base64 — exactly what the Standard Webhooks reference libraries do, so the
// operator can paste one string into both sides. Nothing is ever used
// verbatim, and a secret that is not base64 is refused where it is
// configured, never signed with.
func TestSigningKeyRule(t *testing.T) {
	for _, secret := range []string{whsecSecret, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYX"} {
		key, err := config.SigningKey(secret)
		if err != nil || string(key) != string(whsecKey) {
			t.Fatalf("SigningKey(%q) = %x, %v; want the decoded bytes", secret, key, err)
		}
	}
	// `openssl rand -base64 32`, the form the docs tell an operator to use.
	if key, err := config.SigningKey("q0Y8Xz6pQm2Jv7Lw3Nc9Rt5Ub1Ye4Ka8Hd0Sf2Gi6Xo="); err != nil || len(key) != 32 {
		t.Fatalf("a 32-byte base64 secret: %x, %v", key, err)
	}
	for _, bad := range []string{"", "whsec_", "gawk-webhook-secret", "hunter2", "whsec_not base64!", "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRY"} {
		if _, err := config.SigningKey(bad); !errors.Is(err, config.ErrInvalidSecret) {
			t.Errorf("SigningKey(%q) = %v, want ErrInvalidSecret", bad, err)
		}
	}
}

// verifyIndependently is the receiver's side of the contract, written from
// the Standard Webhooks specification text rather than from Sign: derive the
// key as the reference libraries do (optional whsec_ prefix, then base64),
// split the header on spaces, take every `v1,<base64>` entry, HMAC-SHA256
// the key over `id.timestamp.body`, and accept if any entry matches in
// constant time.
// Nothing here calls Sign, so a change to the signed material makes this fail
// rather than agree.
func verifyIndependently(t *testing.T, secret, idHeader, timestampHeader, signatureHeader string, body []byte) bool {
	t.Helper()
	// The libraries' rule: optional prefix, then always decode.
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		t.Fatalf("secret %q is not base64: %v", secret, err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(idHeader + "." + timestampHeader + "."))
	mac.Write(body)
	want := mac.Sum(nil)

	if signatureHeader == "" {
		t.Fatal("no signature header")
	}
	for _, entry := range strings.Fields(signatureHeader) {
		version, encoded, ok := strings.Cut(entry, ",")
		if !ok || version != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("signature %q is not base64: %v", entry, err)
		}
		if hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

func TestVerifyIndependentlyAcceptsAndRejects(t *testing.T) {
	body := []byte(goldenBody)
	ts := strconv.FormatInt(goldenTimestamp, 10)
	for _, secret := range []string{"Z2F3ay13ZWJob29rLXNlY3JldA==", whsecSecret} {
		key, _ := config.SigningKey(secret)
		sig := Sign(key, goldenID, goldenTimestamp, body)

		if !verifyIndependently(t, secret, goldenID, ts, sig, body) {
			t.Fatalf("%q: an independent implementation of the spec rejected a signature Sign produced", secret)
		}
		// Mutating the timestamp alone must invalidate the signature — that
		// is what makes a receiver's |now - timestamp| > 300 s replay window
		// enforceable rather than advisory.
		if verifyIndependently(t, secret, goldenID, strconv.FormatInt(goldenTimestamp+1, 10), sig, body) {
			t.Fatal("the signature survived a re-dated timestamp")
		}
		// Mutating the id must too: a captured delivery cannot be
		// re-identified as another event.
		if verifyIndependently(t, secret, "another-id", ts, sig, body) {
			t.Fatal("the signature survived a re-identified delivery")
		}
		if verifyIndependently(t, secret, goldenID, ts, sig, append(append([]byte(nil), body...), ' ')) {
			t.Fatal("the signature survived a modified body")
		}
		if verifyIndependently(t, "YS1kaWZmZXJlbnQtc2VjcmV0", goldenID, ts, sig, body) {
			t.Fatal("a different secret verified the signature")
		}
	}
	// The format permits several space-separated signatures; a receiver
	// accepts if any verifies. Sign sends one, but the verifier must be
	// written for the format.
	key, _ := config.SigningKey("Z2F3ay13ZWJob29rLXNlY3JldA==")
	both := "v1,AAAA " + Sign(key, goldenID, goldenTimestamp, body)
	if !verifyIndependently(t, "Z2F3ay13ZWJob29rLXNlY3JldA==", goldenID, ts, both, body) {
		t.Fatal("a multi-signature header with one valid entry was rejected")
	}
}
