package bridge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

// certSource serves the listeners' certificate.
type certSource interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

// fileCert serves a certificate from files and picks up a renewal (a
// cert-manager Secret rewrite) on the next handshake after the file's mtime
// changes, checked at most every 30 s — the relay's tlsutil.Reloader shape.
type fileCert struct {
	certPath, keyPath string
	log               *slog.Logger

	mu       sync.Mutex
	cached   *tls.Certificate
	lastStat time.Time
	lastMod  time.Time
}

const certStatInterval = 30 * time.Second

func newFileCert(certPath, keyPath string, log *slog.Logger) (*fileCert, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load certificate: %w", err)
	}
	f := &fileCert{certPath: certPath, keyPath: keyPath, log: log, cached: &cert, lastStat: time.Now()}
	if fi, err := os.Stat(certPath); err == nil {
		f.lastMod = fi.ModTime()
	}
	return f, nil
}

func (f *fileCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Since(f.lastStat) < certStatInterval {
		return f.cached, nil
	}
	f.lastStat = time.Now()
	fi, err := os.Stat(f.certPath)
	if err != nil || fi.ModTime().Equal(f.lastMod) {
		return f.cached, nil
	}
	cert, err := tls.LoadX509KeyPair(f.certPath, f.keyPath)
	if err != nil {
		f.log.Warn("certificate reload failed, keeping the current one", "err", err)
		return f.cached, nil
	}
	f.cached, f.lastMod = &cert, fi.ModTime()
	f.log.Info("certificate reloaded")
	return f.cached, nil
}

// staticCert serves one certificate.
type staticCert struct{ cert *tls.Certificate }

func (s staticCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return s.cert, nil
}

// NewDevCert returns a self-signed ECDSA certificate for hosts, valid for 13
// days: within WebTransport's 14-day limit for serverCertificateHashes, so a
// browser can pin it by the hash the bridge logs. In memory only.
func NewDevCert(hosts []string) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "gawk-mumble-bridge dev"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(13 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	sum := sha256.Sum256(der)
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, hex.EncodeToString(sum[:]), nil
}

// loadCertPool reads a PEM bundle.
func loadCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificate", path)
	}
	return pool, nil
}
