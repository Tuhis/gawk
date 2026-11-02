package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
)

// D10's peer lookup. Every gateway pod serves an internal listener (a
// separate port the NetworkPolicy opens only to other gateway pods, never
// routed publicly), authenticated with the shared internal token. A pod
// that holds neither a key's live session nor its running backoff asks
// every peer, at once, for what they hold; a peer that doesn't answer
// within the peer timeout (200 ms) counts as holding nothing.
//
// Routes (all Authorization: Bearer <internalToken>; {hash} is the hex
// SHA-256 of the full stream key, never the key itself):
//
//	GET    /internal/v1/keys/{hash}            this pod's snapshot for the key
//	DELETE /internal/v1/keys/{hash}?version=N  takeover ack: drop the record if still at N
//	GET    /internal/v1/keys/{hash}/status     this pod's status JSON (forwarded GET /whip/status)
//	DELETE /internal/v1/keys/{hash}/status     Retry now here (forwarded DELETE /whip/status)
//	DELETE /internal/v1/sessions/{id}?key={hash}  end a session (forwarded WHIP DELETE)
//
// Forwarded requests are served locally and never looked up again, so a
// request crosses at most one hop.

// snapshot is what one pod holds for a key.
type snapshot struct {
	// Found: the pod holds a record or a session.
	Found bool `json:"found"`
	// Live: the pod holds a session for the key that is live or still
	// being set up or torn down (D10: exempts a reconnect from the cap).
	Live      bool           `json:"live"`
	SessionID string         `json:"sessionId,omitempty"`
	Backoff   bool           `json:"backoff"`
	Record    *status.Record `json:"record,omitempty"`
}

// better reports whether a wins over b: a live session over any record, then
// a running backoff, then the newest record (wall clock, compared within the
// cluster).
func better(a, b snapshot) bool {
	if a.Live != b.Live {
		return a.Live
	}
	if a.Backoff != b.Backoff {
		return a.Backoff
	}
	if !a.Found || a.Record == nil {
		return false
	}
	if !b.Found || b.Record == nil {
		return true
	}
	return a.Record.UpdatedAt.After(b.Record.UpdatedAt)
}

func (s *Server) localSnapshot(h status.Hash) snapshot {
	now := s.now()
	var sn snapshot
	if ss := s.liveSession(h); ss != nil {
		sn.Found, sn.Live, sn.SessionID = true, true, ss.id
	}
	if rec, ok := s.table.Get(h, now); ok {
		sn.Found = true
		sn.Record = &rec
		sn.Backoff = rec.BackoffRunning(now)
	}
	return sn
}

// takeover adopts everything a peer holds for the key (its backoff, its
// lastError and its ended session) and acknowledges, so the peer drops its
// copy (D10). Unacknowledged, the peer keeps a stale copy, bounded by its
// backoff or the record lifetime.
func (s *Server) takeover(ctx context.Context, h status.Hash, from peerResult) status.Record {
	rec := *from.snap.Record
	peerVersion := rec.Version
	adopted := s.table.Put(h, rec)
	if !s.opts.Hooks.SkipTakeoverAck {
		s.peers.ack(ctx, from.peer, h, peerVersion)
	}
	s.log.Info("took over a key's records from a peer", "key", hex.EncodeToString(h[:4]))
	return adopted
}

// InternalHandler serves D10's internal routes.
func (s *Server) InternalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/v1/keys/{hash}", func(w http.ResponseWriter, r *http.Request) {
		h, ok := pathHash(w, r)
		if !ok {
			return
		}
		writeJSON(w, s.localSnapshot(h))
	})
	mux.HandleFunc("DELETE /internal/v1/keys/{hash}", func(w http.ResponseWriter, r *http.Request) {
		h, ok := pathHash(w, r)
		if !ok {
			return
		}
		v, err := strconv.ParseUint(r.URL.Query().Get("version"), 10, 64)
		if err != nil {
			http.Error(w, "bad version", http.StatusBadRequest)
			return
		}
		if !s.table.DeleteIfVersion(h, v) {
			http.Error(w, "changed", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /internal/v1/keys/{hash}/status", func(w http.ResponseWriter, r *http.Request) {
		h, ok := pathHash(w, r)
		if !ok {
			return
		}
		writeJSON(w, s.localStatus(h))
	})
	mux.HandleFunc("DELETE /internal/v1/keys/{hash}/status", func(w http.ResponseWriter, r *http.Request) {
		h, ok := pathHash(w, r)
		if !ok {
			return
		}
		s.clearBackoff(h)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /internal/v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		raw, err := hex.DecodeString(r.URL.Query().Get("key"))
		if err != nil || len(raw) != len(status.Hash{}) {
			http.Error(w, "bad key", http.StatusBadRequest)
			return
		}
		var h status.Hash
		copy(h[:], raw)
		if !s.deleteLocal(r.PathValue("id"), h) {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return s.internalAuth(mux)
}

func (s *Server) internalAuth(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.opts.InternalToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if s.opts.InternalToken == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func pathHash(w http.ResponseWriter, r *http.Request) (status.Hash, bool) {
	var h status.Hash
	raw, err := hex.DecodeString(r.PathValue("hash"))
	if err != nil || len(raw) != len(h) {
		http.Error(w, "bad hash", http.StatusBadRequest)
		return h, false
	}
	copy(h[:], raw)
	return h, true
}

// peerClient talks to the other pods.
type peerClient struct {
	peers   []string
	token   string
	timeout time.Duration
	hc      *http.Client
	m       *Metrics
}

type peerResult struct {
	peer string
	snap snapshot
}

func newPeerClient(peers []string, token string, timeout time.Duration, m *Metrics) *peerClient {
	var clean []string
	for _, p := range peers {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			clean = append(clean, p)
		}
	}
	return &peerClient{peers: clean, token: token, timeout: timeout, m: m,
		hc: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute}}}
}

func (c *peerClient) enabled() bool { return c != nil && len(c.peers) > 0 }

func (c *peerClient) do(ctx context.Context, op, method, rawURL string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		cancel()
		c.m.peer.WithLabelValues(op, "error").Inc()
		return nil, err
	}
	c.m.peer.WithLabelValues(op, strconv.Itoa(resp.StatusCode)).Inc()
	// The body is read by the caller within the same deadline.
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

func keyPath(h status.Hash) string { return "/internal/v1/keys/" + hex.EncodeToString(h[:]) }

// best asks every peer at once and returns the winning snapshot (better),
// ok=false when no peer holds anything.
func (c *peerClient) best(ctx context.Context, h status.Hash) (peerResult, bool) {
	if !c.enabled() {
		return peerResult{}, false
	}
	results := make([]peerResult, len(c.peers))
	var wg sync.WaitGroup
	for i, p := range c.peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.do(ctx, "lookup", http.MethodGet, p+keyPath(h))
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return
			}
			var sn snapshot
			if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&sn) == nil {
				results[i] = peerResult{peer: p, snap: sn}
			}
		}()
	}
	wg.Wait()
	var best peerResult
	found := false
	for _, r := range results {
		if !r.snap.Found {
			continue
		}
		if !found || better(r.snap, best.snap) {
			best, found = r, true
		}
	}
	return best, found
}

func (c *peerClient) ack(ctx context.Context, peer string, h status.Hash, version uint64) bool {
	resp, err := c.do(ctx, "takeover_ack", http.MethodDelete, peer+keyPath(h)+"?version="+strconv.FormatUint(version, 10))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusNoContent
}

func (c *peerClient) status(ctx context.Context, peer string, h status.Hash) (json.RawMessage, bool) {
	resp, err := c.do(ctx, "status_get", http.MethodGet, peer+keyPath(h)+"/status")
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var raw json.RawMessage
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&raw) != nil {
		return nil, false
	}
	return raw, true
}

func (c *peerClient) clearStatus(ctx context.Context, peer string, h status.Hash) bool {
	resp, err := c.do(ctx, "status_delete", http.MethodDelete, peer+keyPath(h)+"/status")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusNoContent
}

func (c *peerClient) deleteSession(ctx context.Context, peer, id string, h status.Hash) bool {
	// The WHIP DELETE waits for StopBroadcast's close, which can take one
	// relay round trip; give it longer than a lookup.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		peer+"/internal/v1/sessions/"+url.PathEscape(id)+"?key="+hex.EncodeToString(h[:]), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		c.m.peer.WithLabelValues("session_delete", "error").Inc()
		return false
	}
	resp.Body.Close()
	c.m.peer.WithLabelValues("session_delete", strconv.Itoa(resp.StatusCode)).Inc()
	return resp.StatusCode == http.StatusNoContent
}
