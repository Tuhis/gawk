package tlsutil

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// statInterval is how often GetCertificate is willing to hit the filesystem
// to check for a renewed certificate. cert-manager renews weeks before
// expiry, so anything under a minute is effectively instant.
const statInterval = 30 * time.Second

// Reloader serves a certificate from files and picks up renewals (e.g.
// cert-manager rewriting a mounted Secret) without a restart. It checks the
// cert file's mtime at most once per statInterval, from the TLS handshake
// path, so no background goroutine or fsnotify is needed.
type Reloader struct {
	certPath string
	keyPath  string
	log      *slog.Logger
	now      func() time.Time // injectable for tests

	mu       sync.Mutex
	cached   *tls.Certificate
	lastStat time.Time // when we last checked the file
	lastMod  time.Time // cert file mtime at last successful load
}

// NewReloader loads the certificate pair immediately and fails fast if it is
// unreadable, so a misconfigured server never starts.
func NewReloader(certPath, keyPath string, log *slog.Logger) (*Reloader, error) {
	r := &Reloader{
		certPath: certPath,
		keyPath:  keyPath,
		log:      log,
		now:      time.Now,
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: load certificate: %w", err)
	}
	r.cached = &cert
	if fi, err := os.Stat(certPath); err == nil {
		r.lastMod = fi.ModTime()
	}
	r.lastStat = r.now()
	return r, nil
}

// GetCertificate satisfies tls.Config.GetCertificate. It returns the cached
// pair, reloading it first when the cert file's mtime has changed. A failed
// reload keeps serving the previous certificate.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	if now.Sub(r.lastStat) >= statInterval {
		r.lastStat = now
		r.maybeReloadLocked()
	}
	return r.cached, nil
}

// maybeReloadLocked checks the cert file mtime and swaps in a freshly loaded
// pair if it changed. Caller must hold r.mu.
func (r *Reloader) maybeReloadLocked() {
	fi, err := os.Stat(r.certPath)
	if err != nil {
		r.log.Warn("cert stat failed, keeping current certificate", "path", r.certPath, "err", err)
		return
	}
	if fi.ModTime().Equal(r.lastMod) {
		return
	}
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		r.log.Warn("cert reload failed, keeping current certificate", "path", r.certPath, "err", err)
		return
	}
	r.cached = &cert
	r.lastMod = fi.ModTime()
	r.log.Info("certificate reloaded", "path", r.certPath, "mtime", fi.ModTime())
}
