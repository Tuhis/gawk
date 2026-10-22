package tlsutil

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

const day = 24 * time.Hour

func newTestRolling(t *testing.T, dir string, clk *fakeClock) *Rolling {
	t.Helper()
	r, err := NewRolling(RollingOptions{
		Dir:   dir,
		Hosts: func() []string { return []string{"192.168.1.20", "localhost"} },
		Now:   clk.Now,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewRolling: %v", err)
	}
	return r
}

// pinnedHandshake does a real TLS handshake against GetCertificate the way a
// browser with serverCertificateHashes does: the chain is accepted only if
// the leaf's DER hash is pinned, the leaf is valid at the (fake) time, and
// its whole span is at most 14 days (Chromium's limit).
func pinnedHandshake(t *testing.T, r *Rolling, now time.Time, pins ...string) error {
	t.Helper()
	// Loopback TCP, not net.Pipe: an unbuffered pipe deadlocks when the
	// client's alert crosses the server's flight.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srvErr := make(chan error, 1)
	go func() {
		sConn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer sConn.Close()
		s := tls.Server(sConn, &tls.Config{GetCertificate: r.GetCertificate, MinVersion: tls.VersionTLS13})
		srvErr <- s.Handshake()
	}()
	cConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cConn.Close()
	client := tls.Client(cConn, &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return fmt.Errorf("leaf not valid at %v (%v..%v)", now, leaf.NotBefore, leaf.NotAfter)
			}
			if span := leaf.NotAfter.Sub(leaf.NotBefore); span > MaxDevCertValidity {
				return fmt.Errorf("leaf span %v exceeds 14 days", span)
			}
			sum := sha256.Sum256(raw[0])
			got := hex.EncodeToString(sum[:])
			for _, p := range pins {
				if p == got {
					return nil
				}
			}
			return errors.New("leaf hash not pinned")
		},
	})
	err = client.Handshake()
	// Close before waiting: a refused chain leaves the server mid-handshake.
	cConn.Close()
	<-srvErr
	return err
}

func TestRollingFirstStartMintsTwo(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	r := newTestRolling(t, dir, clk)
	cur, next := r.Hashes()
	if cur == "" || next == "" || cur == next {
		t.Fatalf("Hashes = %q, %q; want two distinct hashes", cur, next)
	}
	if err := pinnedHandshake(t, r, clk.Now(), cur); err != nil {
		t.Fatalf("handshake pinning the current hash: %v", err)
	}
	if err := pinnedHandshake(t, r, clk.Now(), next); err == nil {
		t.Fatal("the next certificate is served before its time")
	}
	for _, slot := range []string{"a", "b"} {
		leaf := readLeaf(t, filepath.Join(dir, "cert-"+slot+".pem"))
		// 13 d of validity plus the 1 h backdate: under Chromium's 14 d.
		if span := leaf.NotAfter.Sub(leaf.NotBefore); span != 13*day+time.Hour || span >= MaxDevCertValidity {
			t.Errorf("slot %s span = %v, want 13d+1h (< 14d)", slot, span)
		}
		if len(leaf.IPAddresses) == 0 || leaf.IPAddresses[0].String() != "192.168.1.20" {
			t.Errorf("slot %s SANs = %v, want the LAN address", slot, leaf.IPAddresses)
		}
		fi, err := os.Stat(filepath.Join(dir, "key-"+slot+".pem"))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 { // Unix modes only
			t.Errorf("key-%s mode = %v, want 0600", slot, fi.Mode().Perm())
		}
	}
}

// docs/76 §6.1 item 3: a client pinning both published hashes accepts
// either, so a rotation never breaks a fresh dial; a link made at any time
// keeps working for at least six days.
func TestRollingHandshakesWithEitherHashAcrossTheOverlap(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: t0}
	r := newTestRolling(t, t.TempDir(), clk)
	cur0, next0 := r.Hashes()

	var rotations [][2]string
	cancel := r.Subscribe(func(c, n string) { rotations = append(rotations, [2]string{c, n}) })
	defer cancel()

	// Just before the rotation: still the first certificate.
	clk.Advance(6*day - time.Minute)
	if err := pinnedHandshake(t, r, clk.Now(), cur0, next0); err != nil {
		t.Fatalf("before rotation: %v", err)
	}
	// Just after: the next one serves, and the old link still works.
	clk.Advance(2 * time.Minute)
	if err := pinnedHandshake(t, r, clk.Now(), cur0, next0); err != nil {
		t.Fatalf("after rotation, the link pinning both hashes: %v", err)
	}
	if err := pinnedHandshake(t, r, clk.Now(), cur0); err == nil {
		t.Fatal("still serving the first certificate after its six days")
	}
	cur1, next1 := r.Hashes()
	if cur1 != next0 || next1 == cur0 || next1 == next0 {
		t.Fatalf("after rotation Hashes = %q, %q; want next0 promoted and a fresh next", cur1, next1)
	}
	if len(rotations) != 1 || rotations[0] != [2]string{cur1, next1} {
		t.Fatalf("Subscribe saw %v, want one rotation to (%s, %s)", rotations, cur1, next1)
	}

	// A link made at t0 works until the second rotation (12 d) ...
	clk.Advance(6*day - 2*time.Minute)
	if err := pinnedHandshake(t, r, clk.Now(), cur0, next0); err != nil {
		t.Fatalf("t0 link at 12d-1m: %v", err)
	}
	// ... and the link made at the first rotation keeps working across the
	// second one.
	clk.Advance(2 * time.Minute)
	if err := pinnedHandshake(t, r, clk.Now(), cur1, next1); err != nil {
		t.Fatalf("6d link after the second rotation: %v", err)
	}
	if err := pinnedHandshake(t, r, clk.Now(), cur0, next0); err == nil {
		t.Fatal("a t0 link outlived both its certificates")
	}
}

// The rotation is persisted: a restart serves the same pair and keeps the
// schedule, so links handed out before the restart keep working.
func TestRollingRotationPersistsAcrossRestart(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	r := newTestRolling(t, dir, clk)
	cur0, next0 := r.Hashes()

	// Restart before any rotation: same pair.
	clk.Advance(day)
	r2 := newTestRolling(t, dir, clk)
	if c, n := r2.Hashes(); c != cur0 || n != next0 {
		t.Fatalf("restart before rotation: %q, %q; want %q, %q", c, n, cur0, next0)
	}

	clk.Advance(5*day + time.Hour)
	r2.Tick()
	cur1, next1 := r2.Hashes()
	if cur1 != next0 {
		t.Fatalf("Tick did not rotate: %q, want %q", cur1, next0)
	}
	r3 := newTestRolling(t, dir, clk)
	if c, n := r3.Hashes(); c != cur1 || n != next1 {
		t.Fatalf("restart after rotation: %q, %q; want %q, %q", c, n, cur1, next1)
	}
	// And the schedule is anchored, not reset: the next rotation is due six
	// days after the first one (12 d), not six days after the Tick that ran
	// it (12 d + 1 h).
	clk.Advance(6*day - 30*time.Minute)
	r3.Tick()
	if c, _ := r3.Hashes(); c != next1 {
		t.Fatalf("second rotation not on schedule after a restart: current %q, want %q", c, next1)
	}
}

// A rotation that is merely late (no handshake, relay suspended) promotes
// the next certificate on the anchored schedule; a relay that was off for
// longer than both serve windows mints a fresh pair rather than serving an
// expired certificate.
func TestRollingLateTicks(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	r := newTestRolling(t, dir, clk)
	_, next0 := r.Hashes()

	clk.Advance(11 * day) // rotation due at 6 d, nothing ticked since
	if err := pinnedHandshake(t, r, clk.Now(), next0); err != nil {
		t.Fatalf("late rotation: %v", err)
	}
	cur, next := r.Hashes()
	// The new next starts serving at 12 d and must still be valid then.
	clk.Advance(day + time.Minute)
	if err := pinnedHandshake(t, r, clk.Now(), cur, next); err != nil {
		t.Fatalf("anchored next after a late rotation: %v", err)
	}

	off := newTestRolling(t, dir, clk)
	c0, n0 := off.Hashes()
	clk.Advance(30 * day)
	off2 := newTestRolling(t, dir, clk)
	c1, n1 := off2.Hashes()
	if c1 == c0 || c1 == n0 || n1 == c0 || n1 == n0 {
		t.Fatal("a relay off for 30 days kept a stale certificate")
	}
	if err := pinnedHandshake(t, off2, clk.Now(), c1); err != nil {
		t.Fatalf("fresh pair after a long downtime: %v", err)
	}
}

// A torn rotation (state file written, the next slot not yet) re-mints the
// next slot instead of publishing the previous current as "next".
func TestRollingRepairsATornRotation(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	r := newTestRolling(t, dir, clk)
	cur0, next0 := r.Hashes()
	clk.Advance(6*day + time.Hour)
	// Simulate the crash: the state file says b serves since 6 d, but slot a
	// still holds the old current.
	if err := writeRollingState(dir, rollingState{Slot: "b", MintedAt: clk.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r2 := newTestRolling(t, dir, clk)
	c, n := r2.Hashes()
	if c != next0 {
		t.Fatalf("current = %q, want the promoted %q", c, next0)
	}
	if n == cur0 || n == "" {
		t.Fatalf("next = %q: the previous current was published as next", n)
	}
	// A garbage state file is a fresh start, not a failure.
	if err := os.WriteFile(filepath.Join(dir, "rolling.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRolling(RollingOptions{Dir: dir, Now: clk.Now}); err != nil {
		t.Fatalf("corrupt state: %v", err)
	}
}

func TestRollingRejectsBadOptions(t *testing.T) {
	for name, o := range map[string]RollingOptions{
		"no dir":          {},
		"validity > 14d":  {Dir: t.TempDir(), Validity: 14 * day},
		"lead != serve":   {Dir: t.TempDir(), Lead: day, Serve: 2 * day},
		"serve too long":  {Dir: t.TempDir(), Validity: 10 * day, Lead: 6 * day, Serve: 6 * day},
		"negative window": {Dir: t.TempDir(), Lead: -day, Serve: -day},
	} {
		if _, err := NewRolling(o); err == nil {
			t.Errorf("%s: NewRolling accepted %+v", name, o)
		}
	}
}

func TestGenerateDevCertAtUsesTheGivenClock(t *testing.T) {
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	c, err := GenerateDevCertAt([]string{"localhost"}, at, 13*day)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Leaf.NotBefore.Equal(at.Add(-time.Hour)) || !c.Leaf.NotAfter.Equal(at.Add(13*day)) {
		t.Errorf("validity %v..%v, want %v..%v", c.Leaf.NotBefore, c.Leaf.NotAfter, at.Add(-time.Hour), at.Add(13*day))
	}
}

func readLeaf(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	keyPath := filepath.Join(filepath.Dir(path), "key-"+filepath.Base(path)[len("cert-"):])
	c, err := loadPairWithLeaf(path, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return c.Leaf
}
