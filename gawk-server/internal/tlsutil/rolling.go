package tlsutil

// The rolling pinned certificate of gawk-relay (R74 PP2, docs/76 §6.1
// item 3).
//
// Two slots, a and b, each a dev-cert pair in the state directory, plus
// rolling.json naming the slot that serves and when the other one was
// minted. Each certificate is minted Lead (6 d) before it starts serving and
// serves for Serve (6 d), inside a 13 d validity: with the 1 h backdate its
// span is 13 d + 1 h, under Chromium's 14 d limit for pinned hashes. The
// relay publishes the hash it serves and the next one; a client pinning
// both accepts either, so a rotation never breaks a link, and a link handed
// out at any moment keeps working for at least six days (up to twelve).
//
// The schedule is anchored: rotation k happens at mintedAt + k·Serve
// whatever the time the rotation actually runs, and the certificate minted
// by it is issued at that scheduled time. A late rotation (no handshake for
// a while, a suspended laptop) therefore still promotes a certificate that
// is valid for its whole serve window. A relay that was off for longer than
// both windows mints a fresh pair.

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Rolling defaults (docs/76 §6.1 item 3).
const (
	DefaultRollingValidity = 13 * 24 * time.Hour
	DefaultRollingLead     = 6 * 24 * time.Hour
	DefaultRollingServe    = 6 * 24 * time.Hour
)

// rollingStateFile names the slot that serves and the anchor of the
// schedule. MintedAt is when the current rotation happened, which is also
// when the next slot's certificate was (notionally) issued.
const rollingStateFile = "rolling.json"

type rollingState struct {
	Slot     string    `json:"slot"`
	MintedAt time.Time `json:"mintedAt"`
}

// RollingOptions configures NewRolling. Zero durations take the defaults.
type RollingOptions struct {
	// Dir holds the slots and the state file (created 0700).
	Dir string
	// Hosts supplies the SANs at each mint. Pinned clients do not check
	// them (docs/76 M5), so a DHCP change waits for the next mint.
	Hosts func() []string
	// Validity of each certificate; at most MaxDevCertValidity minus the
	// 1 h backdate.
	Validity time.Duration
	// Lead is how long before it serves a certificate is minted (and
	// published as "next"). It must equal Serve: the next certificate is
	// minted at each rotation, which is what keeps two hashes published at
	// all times.
	Lead time.Duration
	// Serve is how long each certificate serves.
	Serve time.Duration
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	Log *slog.Logger
}

// Rolling serves the rolling pinned certificate. Safe for concurrent use.
type Rolling struct {
	o RollingOptions

	mu       sync.Mutex
	slot     string // the serving slot, "a" or "b"
	mintedAt time.Time
	cur      *tls.Certificate
	next     *tls.Certificate
	subs     map[int]func(current, next string)
	subID    int
}

// NewRolling loads the slots from Dir, repairing or minting what is missing,
// and rotates if a rotation is due.
func NewRolling(o RollingOptions) (*Rolling, error) {
	if o.Dir == "" {
		return nil, errors.New("tlsutil: rolling certificate needs a directory")
	}
	if o.Validity == 0 {
		o.Validity = DefaultRollingValidity
	}
	if o.Lead == 0 {
		o.Lead = DefaultRollingLead
	}
	if o.Serve == 0 {
		o.Serve = DefaultRollingServe
	}
	switch {
	case o.Lead < 0 || o.Serve <= 0 || o.Validity < 0:
		return nil, fmt.Errorf("tlsutil: rolling durations must be positive")
	case o.Lead != o.Serve:
		return nil, fmt.Errorf("tlsutil: rolling lead %v must equal serve %v", o.Lead, o.Serve)
	case o.Validity > MaxDevCertValidity-clockSkew:
		return nil, fmt.Errorf("tlsutil: rolling validity %v exceeds %v", o.Validity, MaxDevCertValidity-clockSkew)
	case o.Validity < o.Lead+o.Serve:
		return nil, fmt.Errorf("tlsutil: rolling validity %v is shorter than lead+serve %v", o.Validity, o.Lead+o.Serve)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("tlsutil: rolling directory: %w", err)
	}
	r := &Rolling{o: o, subs: map[int]func(string, string){}}
	r.mu.Lock()
	err := r.loadLocked()
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r.Tick()
	return r, nil
}

// GetCertificate satisfies tls.Config.GetCertificate, rotating first when a
// rotation is due (the Reloader pattern: no goroutine needed).
func (r *Rolling) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.Tick()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur, nil
}

// Hashes returns the CertHashHex of the serving and the next certificate:
// the pair offers, the console, the link and config.js publish.
func (r *Rolling) Hashes() (current, next string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return CertHashHex(r.cur.Leaf), CertHashHex(r.next.Leaf)
}

// Subscribe registers fn to run after every rotation with the new hashes.
func (r *Rolling) Subscribe(fn func(current, next string)) (cancel func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.subID
	r.subID++
	r.subs[id] = fn
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.subs, id)
	}
}

// Tick rotates if a rotation is due. A failed rotation keeps serving the
// current certificate (it has a day of validity past its window) and is
// retried on the next call.
func (r *Rolling) Tick() {
	r.mu.Lock()
	rotated, err := r.tickLocked(r.o.Now())
	var subs []func(string, string)
	var cur, next string
	if rotated {
		cur, next = CertHashHex(r.cur.Leaf), CertHashHex(r.next.Leaf)
		for _, fn := range r.subs {
			subs = append(subs, fn)
		}
	}
	r.mu.Unlock()
	if err != nil {
		r.o.Log.Error("pinned certificate rotation failed; still serving the current one", "err", err)
		return
	}
	if rotated {
		r.o.Log.Info("pinned certificate rotated", "cert_hash_hex", cur, "next_cert_hash_hex", next)
		for _, fn := range subs {
			fn(cur, next)
		}
	}
}

func (r *Rolling) tickLocked(now time.Time) (bool, error) {
	due := r.mintedAt.Add(r.o.Serve)
	switch {
	case now.Before(due):
		return false, nil
	case now.Before(due.Add(r.o.Serve)) && now.Before(r.next.Leaf.NotAfter):
		// One rotation, on the anchored schedule: the next slot serves and
		// the old serving slot takes a certificate issued at the rotation's
		// scheduled time. The state file goes first, so a crash before the
		// new pair is written is repaired by loadLocked (the next slot's
		// issue time then disagrees with mintedAt).
		newSlot := otherSlot(r.slot)
		if err := writeRollingState(r.o.Dir, rollingState{Slot: newSlot, MintedAt: due}); err != nil {
			return false, err
		}
		next, err := r.mintLocked(r.slot, due)
		if err != nil {
			return false, err
		}
		r.slot, r.mintedAt, r.cur, r.next = newSlot, due, r.next, next
		return true, nil
	default:
		// Off for longer than both windows: nothing on disk can serve.
		if err := r.freshLocked(now); err != nil {
			return false, err
		}
		return true, nil
	}
}

// loadLocked reads the slots; anything missing, foreign or inconsistent is
// replaced, never an error (the state directory is the relay's own).
func (r *Rolling) loadLocked() error {
	st, err := readRollingState(r.o.Dir)
	if err != nil || (st.Slot != "a" && st.Slot != "b") {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.o.Log.Warn("pinned certificate state unreadable; minting a fresh pair", "err", err)
		}
		return r.freshLocked(r.o.Now())
	}
	cur, err := r.loadSlot(st.Slot)
	if err != nil || !isReplaceableDevCert(cur.Leaf) {
		r.o.Log.Warn("serving pinned certificate unusable; minting a fresh pair", "slot", st.Slot, "err", err)
		return r.freshLocked(r.o.Now())
	}
	r.slot, r.mintedAt, r.cur = st.Slot, st.MintedAt, cur
	nextSlot := otherSlot(st.Slot)
	next, err := r.loadSlot(nextSlot)
	if err != nil || !isReplaceableDevCert(next.Leaf) || !issuedAt(next.Leaf, st.MintedAt) {
		// A torn rotation: the state was written, the new pair was not.
		r.o.Log.Warn("next pinned certificate does not match the schedule; re-minting it", "slot", nextSlot, "err", err)
		if next, err = r.mintLocked(nextSlot, st.MintedAt); err != nil {
			return err
		}
	}
	r.next = next
	return nil
}

// freshLocked mints both slots at now: a serves from now, b from now+Serve.
func (r *Rolling) freshLocked(now time.Time) error {
	cur, err := r.mintLocked("a", now)
	if err != nil {
		return err
	}
	next, err := r.mintLocked("b", now)
	if err != nil {
		return err
	}
	if err := writeRollingState(r.o.Dir, rollingState{Slot: "a", MintedAt: now}); err != nil {
		return err
	}
	r.slot, r.mintedAt, r.cur, r.next = "a", now, cur, next
	return nil
}

func (r *Rolling) mintLocked(slot string, at time.Time) (*tls.Certificate, error) {
	var hosts []string
	if r.o.Hosts != nil {
		hosts = r.o.Hosts()
	}
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1"}
	}
	c, err := GenerateDevCertAt(hosts, at, r.o.Validity)
	if err != nil {
		return nil, err
	}
	certPath, keyPath := r.slotPaths(slot)
	if err := WriteCertPair(certPath, keyPath, c); err != nil {
		return nil, err
	}
	loaded, err := loadPairWithLeaf(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &loaded, nil
}

func (r *Rolling) loadSlot(slot string) (*tls.Certificate, error) {
	certPath, keyPath := r.slotPaths(slot)
	c, err := loadPairWithLeaf(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *Rolling) slotPaths(slot string) (cert, key string) {
	return filepath.Join(r.o.Dir, "cert-"+slot+".pem"), filepath.Join(r.o.Dir, "key-"+slot+".pem")
}

func otherSlot(s string) string {
	if s == "a" {
		return "b"
	}
	return "a"
}

// issuedAt reports whether leaf was minted at t (to the second: x509 times
// carry no more).
func issuedAt(leaf *x509.Certificate, t time.Time) bool {
	d := leaf.NotBefore.Add(clockSkew).Sub(t.Truncate(time.Second))
	return d > -time.Second && d < time.Second
}

func readRollingState(dir string) (rollingState, error) {
	var st rollingState
	b, err := os.ReadFile(filepath.Join(dir, rollingStateFile))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("tlsutil: %s: %w", rollingStateFile, err)
	}
	return st, nil
}

// writeRollingState replaces the state file atomically (temp + rename).
func writeRollingState(dir string, st rollingState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, rollingStateFile+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, rollingStateFile))
}
