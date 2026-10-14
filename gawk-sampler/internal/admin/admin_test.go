package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/screening"
)

// fakeIdP serves discovery and a client-credentials token endpoint.
type fakeIdP struct {
	srv    *httptest.Server
	issued atomic.Int32
	life   int
}

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{life: 300}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": f.srv.URL, "token_endpoint": f.srv.URL + "/token"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostForm.Get("client_id") != "gawk-sampler" || r.PostForm.Get("client_secret") != "s3cret&=%" ||
			r.PostForm.Get("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		n := f.issued.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok%d", n), "expires_in": f.life})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newTokens(idp *fakeIdP, now *time.Time) *TokenSource {
	return &TokenSource{Issuer: idp.srv.URL, ClientID: "gawk-sampler", ClientSecret: "s3cret&=%", Client: idp.srv.Client(),
		Now: func() time.Time { return *now }}
}

// D18: the token is cached until shortly before it expires.
func TestTokenSourceCaches(t *testing.T) {
	idp := newFakeIdP(t)
	now := time.Unix(1_000_000, 0)
	ts := newTokens(idp, &now)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if tok, err := ts.Token(ctx); err != nil || tok != "tok1" {
			t.Fatalf("Token = %q, %v", tok, err)
		}
	}
	now = now.Add(269 * time.Second) // 300 s life, 30 s slack
	if tok, _ := ts.Token(ctx); tok != "tok1" {
		t.Fatalf("renewed early: %s", tok)
	}
	now = now.Add(2 * time.Second)
	if tok, _ := ts.Token(ctx); tok != "tok2" {
		t.Fatalf("not renewed near expiry: %s", tok)
	}
	ts.Invalidate()
	if tok, _ := ts.Token(ctx); tok != "tok3" {
		t.Fatalf("not renewed after Invalidate: %s", tok)
	}
}

func TestTokenSourceRefusals(t *testing.T) {
	idp := newFakeIdP(t)
	now := time.Unix(1_000_000, 0)
	ts := newTokens(idp, &now)
	ts.ClientSecret = "wrong"
	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("a wrong secret got a token")
	}
	ts = newTokens(idp, &now)
	ts.Issuer = idp.srv.URL + "/realms/other"
	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("discovery under the wrong issuer was accepted")
	}
}

type staticTokens struct {
	tokens      []string
	invalidated int
}

func (s *staticTokens) Token(context.Context) (string, error) {
	return s.tokens[min(s.invalidated, len(s.tokens)-1)], nil
}
func (s *staticTokens) Invalidate() { s.invalidated++ }

func TestClient(t *testing.T) {
	var gotQuery string
	var flagBody screening.ContentFlag
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case screening.PathTargets:
			gotQuery = r.URL.RawQuery
			json.NewEncoder(w).Encode(screening.TargetsResponse{Schema: screening.SchemaTargets, Shard: 1, Of: 2, FlagRoute: true,
				Targets: []screening.Target{{BroadcastID: "ABC234", BroadcastKey: "0123456789ab", PublisherEpoch: "e1", Relay: "http://10.0.0.1:2112"}}})
		case screening.PathContentFlags:
			json.NewDecoder(r.Body).Decode(&flagBody)
			if flagBody.Mode == screening.ModeFlag && flagBody.BroadcastID == "NOROUT" {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`{"error":{"code":"no_flag_route","message":"no webhook delivers content_flag.raised"}}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(screening.FlagResponse{Status: screening.FlagStatusRaised, FlagID: 7})
		case screening.PathSamples:
			w.Write([]byte(`{"stored":1}`))
		}
	}))
	defer srv.Close()
	tokens := &staticTokens{tokens: []string{"stale", "good"}}
	c := &Client{Base: srv.URL, Tokens: tokens, HTTP: srv.Client()}
	ctx := context.Background()

	// A 401 refreshes the token once and retries.
	tr, err := c.Targets(ctx, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.invalidated != 1 || len(tr.Targets) != 1 || !tr.FlagRoute {
		t.Fatalf("targets %+v, invalidated %d", tr, tokens.invalidated)
	}
	// D22: every poll names its shard.
	if gotQuery != "of=2&shard=1" {
		t.Fatalf("query %q", gotQuery)
	}
	// An answer for another shard is refused.
	if _, err := c.Targets(ctx, 0, 2); err == nil {
		t.Fatal("an answer for shard 1 was accepted for shard 0")
	}

	resp, err := c.PostFlag(ctx, screening.ContentFlag{Schema: screening.SchemaContentFlag, BroadcastID: "ABC234", Mode: screening.ModeFlag})
	if err != nil || resp.Status != screening.FlagStatusRaised || resp.FlagID != 7 {
		t.Fatalf("PostFlag = %+v, %v", resp, err)
	}
	if _, err := c.PostFlag(ctx, screening.ContentFlag{BroadcastID: "NOROUT", Mode: screening.ModeFlag}); !errors.Is(err, ErrNoFlagRoute) {
		t.Fatalf("503 no_flag_route: %v", err)
	}
	if err := c.PostSamples(ctx, screening.SampleBatch{}); err != nil {
		t.Fatal(err)
	}

	// A token gawk-admin keeps refusing is an error, not a loop.
	c.Tokens = &staticTokens{tokens: []string{"bad"}}
	if _, err := c.Targets(ctx, 1, 2); err == nil {
		t.Fatal("401 twice succeeded")
	}
}
