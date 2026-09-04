package transport

// R42 room route gates, handler-level (docs/44 §4.2, §4.4): the pre-upgrade
// vocabulary of CONNECT /room/new and /room/{code} driven straight into the
// handlers the way outcomes_test drives /publish — every status is asserted
// together with the outcome metric it must land under, because the metric
// is what an operator alerts on. The mint gates walk every roomsrv sentinel
// (403/404/409/429/503) plus the catch-all 500; the shared gates (451 ban,
// 503 draining, 429 rate limit, 404 registry-less) are proven on both routes.
//
// A recorder cannot be upgraded, so a request that clears every gate ends
// in the upgrade-failed branch: 403, the upgrade_failed outcome and, for a
// mint, the just-created room ended so the broadcast is not left reserved.

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// newRoomOutcomeServer is newOutcomeServer with -rooms on and a registry
// installed, unless withRegistry is false (the "SetRooms never called"
// shape).
func newRoomOutcomeServer(t *testing.T, cfg config.Config, withRegistry bool, mutate func(*roomsrv.Options)) (*Server, *metrics.ServerMetrics, *hub.Registry, *roomsrv.Registry) {
	t.Helper()
	cfg.Rooms = true
	srv, sm, r := newOutcomeServer(t, cfg, hub.Options{})
	if !withRegistry {
		return srv, sm, r, nil
	}
	opts := roomsrv.Options{Broadcasts: hubBroadcastsAdapter{r}, Obfuscate: r.ObfuscateID, Log: discardLog}
	if mutate != nil {
		mutate(&opts)
	}
	reg := roomsrv.NewRegistry(opts)
	srv.SetRooms(reg)
	return srv, sm, r, reg
}

// liveBroadcast starts a publisher in the hub and returns its ID and the
// hex resume token the mint route expects as proof.
func liveBroadcast(t *testing.T, srv *Server, r *hub.Registry) (id, tokenHex string) {
	t.Helper()
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	t.Cleanup(pub.Close)
	return id, hex.EncodeToString(srv.resume.mint(id))
}

func roomNewReq(query string) *http.Request {
	return connectReq("https://relay/room/new?"+query, nil)
}

func roomJoinReq(code, query string) *http.Request {
	target := "https://relay/room/" + code
	if query != "" {
		target += "?" + query
	}
	return connectReq(target, map[string]string{"code": code})
}

// A banned IP is 451 on both room routes, before the registry is consulted
// (the request here is one a registry-less relay would otherwise 404).
func TestRoomRoutesRejectBannedIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Server, http.ResponseWriter)
	}{
		{"mint", func(s *Server, w http.ResponseWriter) { s.handleRoomNew(w, roomNewReq("broadcast=ABC23Z")) }},
		{"join", func(s *Server, w http.ResponseWriter) { s.handleRoom(w, roomJoinReq("abcdef", "")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, sm, _, _ := newRoomOutcomeServer(t, config.Config{}, false, nil)
			srv.SetModeration(banSet(t, ipBan("203.0.113.0/24", "abusive host")))
			w := httptest.NewRecorder()
			tc.call(srv, w)
			if w.Code != http.StatusUnavailableForLegalReasons {
				t.Fatalf("status = %d, want 451", w.Code)
			}
			if got := sm.ConnectionCount("room", metrics.OutcomeBanned); got != 1 {
				t.Errorf("room/banned = %v, want 1", got)
			}
			// An unrelated CIDR ban does not fire.
			srv.SetModeration(banSet(t, ipBan("198.51.100.0/24", "someone else")))
			w = httptest.NewRecorder()
			tc.call(srv, w)
			if w.Code == http.StatusUnavailableForLegalReasons {
				t.Fatal("an unrelated CIDR ban rejected the room request")
			}
			if got := sm.ConnectionCount("room", metrics.OutcomeBanned); got != 1 {
				t.Errorf("room/banned after the unrelated ban = %v, want still 1", got)
			}
		})
	}
}

// Once the drain has begun both room routes — and the internal one — are
// 503 without touching the registry.
func TestRoomRoutesRejectWhileDraining(t *testing.T) {
	srv, sm, _, _ := newRoomOutcomeServer(t, config.Config{}, true, nil)
	srv.draining.Store(true)
	for _, tc := range []struct {
		route string
		call  func(http.ResponseWriter)
	}{
		{"room", func(w http.ResponseWriter) { srv.handleRoomNew(w, roomNewReq("broadcast=ABC23Z")) }},
		{"room", func(w http.ResponseWriter) { srv.handleRoom(w, roomJoinReq("abcdef", "")) }},
		{"internal-room", func(w http.ResponseWriter) { srv.handleInternalRoom(w, roomJoinReq("abcdef", "psk=x&gen=1")) }},
	} {
		w := httptest.NewRecorder()
		tc.call(w)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s while draining: status = %d, want 503", tc.route, w.Code)
		}
	}
	if got := sm.ConnectionCount("room", metrics.OutcomeDraining); got != 2 {
		t.Errorf("room/draining = %v, want 2", got)
	}
	if got := sm.ConnectionCount("internal-room", metrics.OutcomeDraining); got != 1 {
		t.Errorf("internal-room/draining = %v, want 1", got)
	}
}

// The connection rate limiter covers the room routes: a burst of one lets
// the first attempt through (to whatever gate follows) and 429s the next.
func TestRoomRoutesAreRateLimited(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Server, http.ResponseWriter)
	}{
		{"mint", func(s *Server, w http.ResponseWriter) { s.handleRoomNew(w, roomNewReq("broadcast=ABC23Z")) }},
		{"join", func(s *Server, w http.ResponseWriter) { s.handleRoom(w, roomJoinReq("abcdef", "")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, sm, _, _ := newRoomOutcomeServer(t, config.Config{ConnRateLimit: 0.001, ConnBurstLimit: 1}, true, nil)
			w := httptest.NewRecorder()
			tc.call(srv, w)
			if w.Code != http.StatusNotFound {
				t.Fatalf("first attempt: status = %d, want 404 (unknown target, past the limiter)", w.Code)
			}
			w = httptest.NewRecorder()
			tc.call(srv, w)
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("second attempt: status = %d, want 429", w.Code)
			}
			if got := sm.ConnectionCount("room", metrics.OutcomeNotFound); got != 1 {
				t.Errorf("room/not_found = %v, want 1 (the 429 must not count as a room outcome)", got)
			}
		})
	}
}

// -rooms on but SetRooms never called (main's wiring failed half-way): the
// routes exist and answer 404, never a nil dereference.
func TestRoomRoutesWithoutARegistryAre404(t *testing.T) {
	srv, sm, _, _ := newRoomOutcomeServer(t, config.Config{}, false, nil)
	if srv.RoomStatsSource() != nil {
		t.Fatal("RoomStatsSource with no registry must be a nil interface")
	}
	if srv.RoomStats() != nil {
		t.Fatal("RoomStats with no registry must be nil so /statusz omits the section")
	}
	w := httptest.NewRecorder()
	srv.handleRoomNew(w, roomNewReq("broadcast=ABC23Z"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("mint: status = %d, want 404", w.Code)
	}
	w = httptest.NewRecorder()
	srv.handleRoom(w, roomJoinReq("abcdef", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("join: status = %d, want 404", w.Code)
	}
	if got := sm.ConnectionCount("room", metrics.OutcomeNotFound); got != 2 {
		t.Errorf("room/not_found = %v, want 2", got)
	}
}

// An empty path value (a route registered as /room/{code} matched with
// nothing after the slash) is 404 even with a registry installed.
func TestRoomJoinWithEmptyCodeIs404(t *testing.T) {
	srv, sm, _, _ := newRoomOutcomeServer(t, config.Config{}, true, nil)
	w := httptest.NewRecorder()
	srv.handleRoom(w, roomJoinReq("", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if got := sm.ConnectionCount("room", metrics.OutcomeNotFound); got != 1 {
		t.Errorf("room/not_found = %v, want 1", got)
	}
}

// The mint route's status vocabulary, one registry sentinel per case, each
// under the outcome the design assigns it (docs/44 §4.4 step 1).
func TestRoomMintStatusVocabulary(t *testing.T) {
	t.Run("wrong create secret is 403 unauthorized", func(t *testing.T) {
		srv, sm, r, _ := newRoomOutcomeServer(t, config.Config{}, true, func(o *roomsrv.Options) { o.CreateSecret = "invite" })
		id, tok := liveBroadcast(t, srv, r)
		w := httptest.NewRecorder()
		srv.handleRoomNew(w, roomNewReq("broadcast="+id+"&resume="+tok+"&create=wrong"))
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		if got := sm.ConnectionCount("room", metrics.OutcomeUnauthorized); got != 1 {
			t.Errorf("room/unauthorized = %v, want 1", got)
		}
	})
	t.Run("unknown broadcast is 404 not_found", func(t *testing.T) {
		srv, sm, _, _ := newRoomOutcomeServer(t, config.Config{}, true, nil)
		w := httptest.NewRecorder()
		srv.handleRoomNew(w, roomNewReq("broadcast=ZZZZZZ&resume=00"))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
		if got := sm.ConnectionCount("room", metrics.OutcomeNotFound); got != 1 {
			t.Errorf("room/not_found = %v, want 1", got)
		}
	})
	t.Run("broadcast already in a room is 409 conflict", func(t *testing.T) {
		srv, sm, r, reg := newRoomOutcomeServer(t, config.Config{}, true, nil)
		id, tok := liveBroadcast(t, srv, r)
		raw, _ := hex.DecodeString(tok)
		if _, err := reg.Mint(context.Background(), roomsrv.MintRequest{BroadcastID: id, ResumeToken: raw}); err != nil {
			t.Fatalf("first mint: %v", err)
		}
		w := httptest.NewRecorder()
		srv.handleRoomNew(w, roomNewReq("broadcast="+id+"&resume="+tok))
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", w.Code)
		}
		if got := sm.ConnectionCount("room", metrics.OutcomeConflict); got != 1 {
			t.Errorf("room/conflict = %v, want 1", got)
		}
	})
	t.Run("-max-rooms is 429 limit_rejected", func(t *testing.T) {
		srv, sm, r, reg := newRoomOutcomeServer(t, config.Config{}, true, func(o *roomsrv.Options) { o.MaxRooms = 1 })
		first, tok1 := liveBroadcast(t, srv, r)
		raw1, _ := hex.DecodeString(tok1)
		if _, err := reg.Mint(context.Background(), roomsrv.MintRequest{BroadcastID: first, ResumeToken: raw1}); err != nil {
			t.Fatalf("first mint: %v", err)
		}
		second, tok2 := liveBroadcast(t, srv, r)
		w := httptest.NewRecorder()
		srv.handleRoomNew(w, roomNewReq("broadcast="+second+"&resume="+tok2))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", w.Code)
		}
		if got := sm.ConnectionCount("room", metrics.OutcomeLimitRejected); got != 1 {
			t.Errorf("room/limit_rejected = %v, want 1", got)
		}
	})
	t.Run("store unavailable is 503 error", func(t *testing.T) {
		srv, sm, r, _ := newRoomOutcomeServer(t, config.Config{}, true, func(o *roomsrv.Options) {
			o.Reserve = func(context.Context, *rooms.Room) error { return roomsrv.ErrUnavailable }
		})
		id, tok := liveBroadcast(t, srv, r)
		w := httptest.NewRecorder()
		srv.handleRoomNew(w, roomNewReq("broadcast="+id+"&resume="+tok))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", w.Code)
		}
		if got := sm.ConnectionCount("room", metrics.OutcomeError); got != 1 {
			t.Errorf("room/error = %v, want 1", got)
		}
	})
	t.Run("an unmapped mint failure is 500 error", func(t *testing.T) {
		// Every reservation "taken": Mint re-mints until its retry budget
		// is spent and returns a collision error no case maps — the
		// catch-all must be a 500, never a misleading client-side status.
		srv, sm, r, _ := newRoomOutcomeServer(t, config.Config{}, true, func(o *roomsrv.Options) {
			o.Reserve = func(context.Context, *rooms.Room) error { return errors.New("taken") }
		})
		id, tok := liveBroadcast(t, srv, r)
		w := httptest.NewRecorder()
		srv.handleRoomNew(w, roomNewReq("broadcast="+id+"&resume="+tok))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
		if got := sm.ConnectionCount("room", metrics.OutcomeError); got != 1 {
			t.Errorf("room/error = %v, want 1", got)
		}
	})
}

// The two status mappers as tables: every sentinel has a deliberate
// status, and anything else is a 500 (CODE-REVIEW "error mapping at
// boundaries").
func TestRoomStatusMappers(t *testing.T) {
	for _, tc := range []struct {
		err         error
		wantStatus  int
		wantOutcome string
	}{
		{roomsrv.ErrForbidden, http.StatusForbidden, metrics.OutcomeUnauthorized},
		{roomsrv.ErrNotFound, http.StatusNotFound, metrics.OutcomeNotFound},
		{roomsrv.ErrAlreadyAttached, http.StatusConflict, metrics.OutcomeConflict},
		{roomsrv.ErrMaxRooms, http.StatusTooManyRequests, metrics.OutcomeLimitRejected},
		{roomsrv.ErrUnavailable, http.StatusServiceUnavailable, metrics.OutcomeError},
		{errors.New("something else"), http.StatusInternalServerError, metrics.OutcomeError},
	} {
		if status, outcome := roomMintStatus(tc.err); status != tc.wantStatus || outcome != tc.wantOutcome {
			t.Errorf("roomMintStatus(%v) = %d/%s, want %d/%s", tc.err, status, outcome, tc.wantStatus, tc.wantOutcome)
		}
	}
	for _, tc := range []struct {
		err         error
		wantStatus  int
		wantOutcome string
	}{
		{roomsrv.ErrNotFound, http.StatusNotFound, metrics.OutcomeNotFound},
		{roomsrv.ErrForbidden, http.StatusForbidden, metrics.OutcomeUnauthorized},
		{roomsrv.ErrFull, http.StatusTooManyRequests, metrics.OutcomeLimitRejected},
		{errors.New("something else"), http.StatusInternalServerError, metrics.OutcomeError},
	} {
		if status, outcome := roomJoinStatus(tc.err); status != tc.wantStatus || outcome != tc.wantOutcome {
			t.Errorf("roomJoinStatus(%v) = %d/%s, want %d/%s", tc.err, status, outcome, tc.wantStatus, tc.wantOutcome)
		}
	}
}

// A mint whose session upgrade fails must not leave the room standing: the
// broadcast would sit reserved (409 for the broadcaster's retry) for the
// whole empty grace. The recorder cannot upgrade, which is exactly that
// failure.
func TestRoomMintUpgradeFailureEndsTheRoom(t *testing.T) {
	srv, sm, r, reg := newRoomOutcomeServer(t, config.Config{}, true, nil)
	id, tok := liveBroadcast(t, srv, r)
	w := httptest.NewRecorder()
	srv.handleRoomNew(w, roomNewReq("broadcast="+id+"&resume="+tok))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no implicit 200)", w.Code)
	}
	if got := sm.ConnectionCount("room", metrics.OutcomeUpgradeFailed); got != 1 {
		t.Errorf("room/upgrade_failed = %v, want 1", got)
	}
	if rows := reg.Stats(); len(rows) != 0 {
		t.Fatalf("the room outlived its failed upgrade: %+v", rows)
	}
	// The broadcast is mintable again right away.
	raw, _ := hex.DecodeString(tok)
	if _, err := reg.Mint(context.Background(), roomsrv.MintRequest{BroadcastID: id, ResumeToken: raw}); err != nil {
		t.Fatalf("re-mint after the failed upgrade: %v", err)
	}
}

// The join route's upgrade failure: 403 under upgrade_failed, and the room
// (which the joiner never entered) is untouched.
func TestRoomJoinUpgradeFailure(t *testing.T) {
	srv, sm, _, reg := newRoomOutcomeServer(t, config.Config{}, true, nil)
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "TuhisRoom"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.handleRoom(w, roomJoinReq("TUHISROOM", ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if got := sm.ConnectionCount("room", metrics.OutcomeUpgradeFailed); got != 1 {
		t.Errorf("room/upgrade_failed = %v, want 1", got)
	}
	if info, ok := reg.Lookup("tuhisroom"); !ok || info.Participants != 0 {
		t.Fatalf("room after the failed join = %+v, %v", info, ok)
	}
}
