package readapi

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

// The dashboard shows the OBFUSCATED broadcast key, because that is the only
// identity telemetry ever learns: the raw 6-character code is a join credential
// and D8 makes the client structurally incapable of reporting one. But an
// operator holding the code they just read off their own screen still has to
// find that stream on the page.
//
// Resolving is therefore one-way and server-side: code -> key, computed with
// the fleet statsKey exactly as the relay's ObfuscateID does
// (hub.go:1354, hex(HMAC-SHA256(statsKey, id)[:6])). The key never becomes a
// code, the code is never stored, and the statsKey never leaves the cluster.
func newResolveAPI(t *testing.T, statsKey []byte) *API {
	t.Helper()
	st, err := store.New(store.Options{Root: t.TempDir(), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	api, err := New(Options{Store: st, Now: time.Now, StatsKey: statsKey})
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return rec
}

// A GOLDEN VECTOR, because the construction cannot be imported: ObfuscateID
// lives in gawk-server's internal/hub and this module can only reach
// gawk-server/wire. Re-deriving the same digest here is a mirror, and every
// mirror in this repo is pinned by a vector rather than trusted (the wire
// package's Go/TS vectors are the precedent).
//
// Verified 2026-07-27 three independent ways, all agreeing:
//
//	Registry.ObfuscateID(statsKey=ab*32, "ABC234")             -> 69a445b44f18
//	printf 'ABC234' | openssl dgst -sha256 -mac HMAC \
//	    -macopt hexkey:$(printf 'ab%.0s' {1..32}) -binary | xxd -p | head -c 12
//	readapi.obfuscate(...)
//
// If this fails, the relay's obfuscation changed and the dashboard's lookup is
// silently resolving to keys that match nothing. Fix both ends together.
func TestObfuscationMatchesTheRelayGoldenVector(t *testing.T) {
	key, _ := hex.DecodeString(strings.Repeat("ab", 32))
	if got := obfuscate(key, "ABC234"); got != "69a445b44f18" {
		t.Errorf("obfuscate = %q, want 69a445b44f18 (the relay's ObfuscateID for this key)", got)
	}
}

func TestResolveMatchesTheRelaysObfuscation(t *testing.T) {
	// The exact digest the relay would publish for this code under this key.
	key, _ := hex.DecodeString(strings.Repeat("ab", 32))
	api := newResolveAPI(t, key)

	rec := post(t, api.Handler(), "/v1/resolve", `{"code":"ABC234"}`)
	if rec.Code != 200 {
		t.Fatalf("HTTP %d, body %s", rec.Code, rec.Body.String())
	}
	var got struct {
		BroadcastKey string `json:"broadcastKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := obfuscate(key, "ABC234")
	if got.BroadcastKey != want {
		t.Errorf("broadcastKey = %q, want %q", got.BroadcastKey, want)
	}
	if len(got.BroadcastKey) != 12 {
		t.Errorf("key = %q, want 12 hex chars to match /statusz", got.BroadcastKey)
	}
}

// Codes are case-insensitive on every other surface (broadcastid.Normalize
// upper-cases before validating), so a lowercase paste must not silently
// resolve to a key that matches nothing.
func TestResolveIsCaseInsensitive(t *testing.T) {
	key, _ := hex.DecodeString(strings.Repeat("cd", 32))
	api := newResolveAPI(t, key)

	upper := post(t, api.Handler(), "/v1/resolve", `{"code":"ABC234"}`).Body.String()
	lower := post(t, api.Handler(), "/v1/resolve", `{"code":"abc234"}`).Body.String()
	if upper != lower {
		t.Errorf("case changed the answer:\n upper %s lower %s", upper, lower)
	}
}

// Off by default. Without a statsKey the service cannot compute the digest at
// all, and must say so rather than returning a wrong or empty key that would
// silently match nothing.
func TestResolveIsOffWithoutAStatsKey(t *testing.T) {
	api := newResolveAPI(t, nil)
	rec := post(t, api.Handler(), "/v1/resolve", `{"code":"ABC234"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("HTTP %d, want 501 when no stats key is configured", rec.Code)
	}
}

// The code is a join credential, so it must never reach a place that gets
// written down: no query string (browser history, proxy access logs, Referer).
func TestResolveRefusesTheCodeInAQueryString(t *testing.T) {
	key, _ := hex.DecodeString(strings.Repeat("ef", 32))
	api := newResolveAPI(t, key)

	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/resolve?code=ABC234", nil))
	if rec.Code == 200 {
		t.Error("a GET with the code in the query string was served; it must not be a routable shape")
	}
}

func TestResolveRejectsJunk(t *testing.T) {
	key, _ := hex.DecodeString(strings.Repeat("11", 32))
	api := newResolveAPI(t, key)

	for name, body := range map[string]string{
		"empty":     `{"code":""}`,
		"oversized": `{"code":"` + strings.Repeat("A", 500) + `"}`,
		"malformed": `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := post(t, api.Handler(), "/v1/resolve", body); rec.Code != 400 {
				t.Errorf("HTTP %d, want 400", rec.Code)
			}
		})
	}
}
