package popsrv

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// ErrTooManyFailures: the source address failed authentication too often
// (429). The /pop/* routes are exempt from the per-IP connection limiter
// and IP bans (docs/76 item 30), so this limiter is their only brake.
var ErrTooManyFailures = errors.New("popsrv: too many failed PoP authentications")

// Failure limiter: failLimit failures per failWindow per source address.
const (
	failLimit  = 20
	failWindow = time.Minute
)

// Authenticate checks a /pop/* request's credential (Authorization:
// Bearer). The PoP names itself with ?pop= once it knows its name; without
// it every Pop's credential hash is tried. A miss in the informer cache is
// read again fresh, because a credential redeemed on another pod a moment
// ago may not have reached this pod's cache yet.
//
// Statuses: 401 for a credential that is not current (unknown, or
// superseded by a Re-pair), 403 for a revoked or expired PoP, 429 for a
// source over the failure limit.
func (m *Manager) Authenticate(r *http.Request) (*pops.Pop, int, error) {
	src := sourceAddr(r.RemoteAddr)
	if m.limiter.blocked(src) {
		return nil, http.StatusTooManyRequests, ErrTooManyFailures
	}
	cred := poplink.BearerToken(r.Header.Get("Authorization"))
	name := r.URL.Query().Get("pop")
	p, err := m.checkCredential(r.Context(), cred, name)
	if err != nil {
		m.limiter.fail(src)
		return nil, credentialStatus(err), err
	}
	return p, http.StatusOK, nil
}

func (m *Manager) checkCredential(ctx context.Context, cred, name string) (*pops.Pop, error) {
	if cred == "" {
		return nil, pops.ErrCredentialNotCurrent
	}
	now := m.now()
	var p *pops.Pop
	if name != "" {
		p, _ = m.pop(name)
	} else {
		p = m.findByCredential(cred)
	}
	err := pops.ErrCredentialNotCurrent
	if p != nil {
		err = p.CheckCredential(cred, now)
	}
	if errors.Is(err, pops.ErrCredentialNotCurrent) && name != "" && m.opts.Status != nil {
		if fresh, ferr := m.opts.Status.Get(ctx, name); ferr == nil {
			p, err = fresh, fresh.CheckCredential(cred, now)
		}
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (m *Manager) findByCredential(cred string) *pops.Pop {
	if m.opts.Source == nil {
		return nil
	}
	sum := pops.CredentialSHA256(cred)
	for _, p := range m.opts.Source.List() {
		if p.Status.Credential != nil && hexEqual(sum, p.Status.Credential.SHA256) {
			return &p
		}
	}
	return nil
}

// CheckPairing validates a pairing dial (/pop/link?pair=<token>) BEFORE the
// session upgrade, so a refusal is an HTTP status the PoP can read: 404 for
// an unknown string, 410 for a used or expired one, 403 for a revoked or
// ended PoP. The redeem itself is RedeemPairing, after the upgrade, so an
// upgrade that fails never burns the string.
func (m *Manager) CheckPairing(r *http.Request) (*pops.Pop, int, error) {
	src := sourceAddr(r.RemoteAddr)
	if m.limiter.blocked(src) {
		return nil, http.StatusTooManyRequests, ErrTooManyFailures
	}
	tokenSHA := pops.TokenSHA256(r.URL.Query().Get("pair"))
	p := m.findByPairing(tokenSHA)
	err := pops.ErrPairingUnknown
	if p != nil {
		err = p.Redeemable(tokenSHA, m.now())
	}
	if err != nil {
		m.limiter.fail(src)
		return nil, pairingStatus(err), err
	}
	return p, http.StatusOK, nil
}

func (m *Manager) findByPairing(tokenSHA string) *pops.Pop {
	if m.opts.Source == nil || tokenSHA == "" {
		return nil
	}
	for _, p := range m.opts.Source.List() {
		if p.Spec.Pairing.TokenSHA256 != "" && hexEqual(tokenSHA, p.Spec.Pairing.TokenSHA256) {
			return &p
		}
	}
	return nil
}

// RedeemPairing mints the PoP's credential and records its hash in
// status.credential, a CAS on the fresh CR (popsrc.StatusWriter), so a
// string pairs exactly one PoP even when two dials race. It returns the
// Pop as written and the credential, which the link sends once.
func (m *Manager) RedeemPairing(ctx context.Context, p *pops.Pop) (*pops.Pop, string, int, error) {
	if m.opts.Status == nil {
		return nil, "", http.StatusNotFound, pops.ErrPairingUnknown
	}
	cred, err := pops.NewCredential()
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	now := m.now()
	sum := pops.CredentialSHA256(cred)
	if err := m.opts.Status.RedeemPairing(ctx, p.Name, p.Spec.Pairing.TokenSHA256, sum, now); err != nil {
		return nil, "", pairingStatus(err), err
	}
	out := p.DeepCopy()
	out.Status.Credential = &pops.Credential{SHA256: sum, PairingTokenSHA256: p.Spec.Pairing.TokenSHA256}
	m.log.Info("pop paired", "pop", p.Name)
	return out, cred, http.StatusOK, nil
}

// credentialStatus maps a credential check to the link/leg status.
func credentialStatus(err error) int {
	switch {
	case errors.Is(err, pops.ErrRevoked), errors.Is(err, pops.ErrExpired):
		return http.StatusForbidden
	case errors.Is(err, pops.ErrCredentialNotCurrent):
		return http.StatusUnauthorized
	}
	return http.StatusServiceUnavailable
}

// pairingStatus maps a pairing refusal to its status.
func pairingStatus(err error) int {
	switch {
	case errors.Is(err, pops.ErrPairingUnknown):
		return http.StatusNotFound
	case errors.Is(err, pops.ErrPairingUsed), errors.Is(err, pops.ErrPairingExpired):
		return http.StatusGone
	case errors.Is(err, pops.ErrRevoked), errors.Is(err, pops.ErrExpired):
		return http.StatusForbidden
	}
	return http.StatusServiceUnavailable
}

func hexEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(a)), []byte(strings.ToLower(b))) == 1
}

// sourceAddr is a request's peer address without the port (invalid when
// unparseable, which shares one limiter bucket).
func sourceAddr(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// failLimiter counts authentication failures per source address in fixed
// windows.
type failLimiter struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[netip.Addr]*failWindowCount
}

type failWindowCount struct {
	start time.Time
	n     int
}

func newFailLimiter(now func() time.Time) *failLimiter {
	return &failLimiter{now: now, m: map[netip.Addr]*failWindowCount{}}
}

func (f *failLimiter) blocked(a netip.Addr) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.m[a]
	if !ok {
		return false
	}
	if f.now().Sub(w.start) >= failWindow {
		delete(f.m, a)
		return false
	}
	return w.n >= failLimit
}

func (f *failLimiter) fail(a netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	w, ok := f.m[a]
	if !ok || now.Sub(w.start) >= failWindow {
		// Sweep stale windows now and then so the map stays bounded by the
		// addresses failing within one window.
		if len(f.m) > 4096 {
			for k, v := range f.m {
				if now.Sub(v.start) >= failWindow {
					delete(f.m, k)
				}
			}
		}
		f.m[a] = &failWindowCount{start: now, n: 1}
		return
	}
	w.n++
}
