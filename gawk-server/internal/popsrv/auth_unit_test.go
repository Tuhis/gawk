package popsrv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrc"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

func TestAuthStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		err           error
		cred, pairing int
	}{
		{pops.ErrRevoked, http.StatusForbidden, http.StatusForbidden},
		{fmt.Errorf("wrapped: %w", pops.ErrExpired), http.StatusForbidden, http.StatusForbidden},
		{pops.ErrCredentialNotCurrent, http.StatusUnauthorized, http.StatusServiceUnavailable},
		{pops.ErrPairingUnknown, http.StatusServiceUnavailable, http.StatusNotFound},
		{pops.ErrPairingUsed, http.StatusServiceUnavailable, http.StatusGone},
		{pops.ErrPairingExpired, http.StatusServiceUnavailable, http.StatusGone},
		{errors.New("api down"), http.StatusServiceUnavailable, http.StatusServiceUnavailable},
	} {
		if got := credentialStatus(tc.err); got != tc.cred {
			t.Errorf("credentialStatus(%v) = %d, want %d", tc.err, got, tc.cred)
		}
		if got := pairingStatus(tc.err); got != tc.pairing {
			t.Errorf("pairingStatus(%v) = %d, want %d", tc.err, got, tc.pairing)
		}
	}
}

func TestSourceAddr(t *testing.T) {
	for in, want := range map[string]string{
		"198.51.100.1:5000":          "198.51.100.1",
		"[::ffff:198.51.100.1]:5000": "198.51.100.1",
		"[2001:db8::1]:443":          "2001:db8::1",
		"198.51.100.2":               "198.51.100.2",
		"2001:db8::2":                "2001:db8::2",
		"not an address":             "invalid IP",
		"":                           "invalid IP",
	} {
		if got := sourceAddr(in).String(); got != want {
			t.Errorf("sourceAddr(%q) = %s, want %s", in, got, want)
		}
	}
	if a := addrOf(nil); a.IsValid() {
		t.Fatal("addrOf(nil) is valid")
	}
	if a := addrOf(&net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 1}); a.String() != "203.0.113.9" {
		t.Fatalf("addrOf(tcp) = %s", a)
	}
	if a := addrOf(net.UDPAddrFromAddrPort(netip.MustParseAddrPort("[::ffff:203.0.113.8]:9"))); a.String() != "203.0.113.8" {
		t.Fatalf("addrOf(udp v4-mapped) = %s", a)
	}
}

// The failure window restarts after failWindow, and a crowded map is swept
// of stale windows when a new one opens.
func TestFailLimiterWindowsAndSweep(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	f := newFailLimiter(func() time.Time { return now })
	a := netip.MustParseAddr("198.51.100.1")
	for range failLimit {
		f.fail(a)
	}
	if !f.blocked(a) {
		t.Fatal("not blocked at the limit")
	}
	now = now.Add(failWindow)
	if f.blocked(a) {
		t.Fatal("still blocked after the window")
	}
	for i := range 4100 {
		f.fail(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}))
	}
	now = now.Add(failWindow)
	f.fail(a) // opens a new window and sweeps the 4100 stale ones
	f.mu.Lock()
	n := len(f.m)
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d windows after the sweep, want 1", n)
	}
	f.fail(a)
	if f.m[a].n != 2 {
		t.Fatalf("count %d in the window, want 2", f.m[a].n)
	}
}

// A pairing check from a source over the failure limit is 429 even for a
// string that would otherwise be 404.
func TestCheckPairingBlockedSource(t *testing.T) {
	h := newUnitHome(t, nil)
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodConnect, "/pop/link?proto=1&pair=nope", nil)
		r.RemoteAddr = "198.51.100.7:4000"
		return r
	}
	for range failLimit {
		if _, status, _ := h.m.CheckPairing(req()); status != http.StatusNotFound {
			t.Fatalf("unknown string = %d, want 404", status)
		}
	}
	if _, status, err := h.m.CheckPairing(req()); status != http.StatusTooManyRequests || !errors.Is(err, ErrTooManyFailures) {
		t.Fatalf("over the limit = %d %v", status, err)
	}
}

// RedeemPairing: no status writer is 404; a refused redeem maps its
// error; a good one returns the Pop with the credential recorded.
func TestRedeemPairing(t *testing.T) {
	bare := New(Options{Log: harnessLog(t)})
	if _, _, status, err := bare.RedeemPairing(context.Background(), &pops.Pop{}); status != http.StatusNotFound || err == nil {
		t.Fatalf("no status writer = %d %v", status, err)
	}
	h := newUnitHome(t, nil)
	p := h.pop.DeepCopy()
	p.Spec.Pairing.TokenSHA256 = pops.TokenSHA256("pair-me")
	h.status.redeemErr = pops.ErrPairingUsed
	if _, _, status, err := h.m.RedeemPairing(context.Background(), p); status != http.StatusGone || !errors.Is(err, pops.ErrPairingUsed) {
		t.Fatalf("used string = %d %v", status, err)
	}
	h.status.redeemErr = nil
	h.status.StatusWriter = acceptRedeem{h.status.StatusWriter}
	out, cred, status, err := h.m.RedeemPairing(context.Background(), p)
	if err != nil || status != http.StatusOK || cred == "" {
		t.Fatalf("redeem = %d %v", status, err)
	}
	if out.Status.Credential == nil || out.Status.Credential.SHA256 != pops.CredentialSHA256(cred) ||
		out.Status.Credential.PairingTokenSHA256 != p.Spec.Pairing.TokenSHA256 {
		t.Fatalf("redeemed credential %+v", out.Status.Credential)
	}
	if p.Status.Credential != nil && p.Status.Credential.SHA256 == out.Status.Credential.SHA256 {
		t.Fatal("RedeemPairing wrote into the caller's Pop")
	}
}

// acceptRedeem redeems any string (memStatus refuses every one).
type acceptRedeem struct{ popsrc.StatusWriter }

func (acceptRedeem) RedeemPairing(context.Context, string, string, string, time.Time) error {
	return nil
}

// config carries the wall code, the issued named host (only while the
// tier is on) and the end date.
func TestConfigRecord(t *testing.T) {
	h := newUnitHome(t, func(o *Options) { o.HomeRooms = true; o.ScreeningNotice = true })
	p := h.pop.DeepCopy()
	end := metav1.NewTime(time.Unix(2_000_000_000, 0))
	p.Spec.NotAfter = &end
	p.Spec.Wall = &pops.Wall{RoomCode: "PARTY1"}
	p.Status.TLS = &pops.TLS{Host: "lan.pop.example"}
	c := h.m.config(p)
	if c.WallCode != "PARTY1" || c.NamedHost != "" || c.NotAfter == nil || !c.NotAfter.Equal(end.Time) || !c.HomeRooms || !c.ScreeningNotice {
		t.Fatalf("config %+v", c)
	}
	p.Spec.NamedAddress.Enabled = true
	if c := h.m.config(p); c.NamedHost != "lan.pop.example" {
		t.Fatalf("named host %q with the tier on", c.NamedHost)
	}
}

func TestMemReservationsEdges(t *testing.T) {
	ctx := context.Background()
	r := NewMemReservations()
	if _, err := r.Reserve(ctx, "K7XQ2M", "", 0); err == nil {
		t.Fatal("a reservation without a PoP name")
	}
	if got, _ := r.Reserve(ctx, "K7XQ2M", "a", 1); got != ReserveOK {
		t.Fatal(got)
	}
	if got, _ := r.Reserve(ctx, "K7XQ2M", "a", 1); got != ReserveOK {
		t.Fatalf("re-reserve at the limit = %q, want ok", got)
	}
	if err := r.Release(ctx, "K7XQ2M", "b"); err == nil {
		t.Fatal("another PoP released a's reservation")
	}
	if err := r.Release(ctx, "NEVER1", "a"); err != nil {
		t.Fatalf("releasing a missing ID = %v", err)
	}
	_, _ = r.Reserve(ctx, "ABC234", "a", 0)
	_, _ = r.Reserve(ctx, "DEF567", "b", 0)
	if err := r.RenewAll(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if ids := r.List("a"); len(ids) != 2 || ids[0] != "ABC234" {
		t.Fatalf("List = %v", ids)
	}
	if err := r.ReleaseAll(ctx, "a"); err != nil || len(r.List("a")) != 0 || len(r.List("b")) != 1 {
		t.Fatalf("ReleaseAll: %v, a=%v b=%v", err, r.List("a"), r.List("b"))
	}
}
