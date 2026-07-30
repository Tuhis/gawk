// gawk-echo is a CLI connectivity probe: it dials a running gawk-server's
// /echo endpoint over WebTransport, round-trips a datagram and prints the
// round-trip time.
//
// Certificate verification mirrors the browser's serverCertificateHashes
// mechanism: pass -cert-hash with the hex SHA-256 the server logs at
// startup. Without it the certificate is not verified (dev tool).
//
// If the target's -allowed-origins is non-empty (e.g. a production
// deployment), pass -origin with one of the allowed values or the CONNECT
// is rejected with "request origin not allowed".
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gawk-echo:", err)
		os.Exit(1)
	}
}

func run() error {
	url := flag.String("url", "https://localhost:4433/echo", "echo endpoint URL")
	certHash := flag.String("cert-hash", "", "hex SHA-256 of the server cert DER (from server startup log); empty skips verification")
	origin := flag.String("origin", "", "Origin header to send; required if the target's -allowed-origins is non-empty (unset skips it, fine for dev)")
	message := flag.String("message", "ping", "datagram payload to send")
	count := flag.Int("count", 3, "number of round-trips")
	timeout := flag.Duration("timeout", 5*time.Second, "per-operation timeout")
	flag.Parse()

	tlsConf := &tls.Config{
		NextProtos:         []string{http3.NextProtoH3},
		InsecureSkipVerify: true,
	}
	if *certHash != "" {
		want, err := hex.DecodeString(*certHash)
		if err != nil || len(want) != sha256.Size {
			return fmt.Errorf("invalid -cert-hash: want 64 hex chars")
		}
		tlsConf.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			for _, raw := range rawCerts {
				if sum := sha256.Sum256(raw); string(sum[:]) == string(want) {
					return nil
				}
			}
			return fmt.Errorf("no presented certificate matches -cert-hash")
		}
	}

	d := webtransport.Transport{
		TLSClientConfig: tlsConf,
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
		},
	}
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var hdr http.Header
	if *origin != "" {
		hdr = http.Header{"Origin": {*origin}}
	}

	dialStart := time.Now()
	_, sess, err := d.Dial(ctx, *url, hdr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", *url, err)
	}
	defer sess.CloseWithError(0, "done")
	fmt.Printf("connected to %s in %v (cert verification: %v)\n",
		*url, time.Since(dialStart).Round(time.Millisecond), *certHash != "")

	for i := range *count {
		payload := fmt.Appendf(nil, "%s #%d", *message, i+1)
		start := time.Now()
		if err := sess.SendDatagram(payload); err != nil {
			return fmt.Errorf("send: %w", err)
		}
		recvCtx, recvCancel := context.WithTimeout(context.Background(), *timeout)
		got, err := sess.ReceiveDatagram(recvCtx)
		recvCancel()
		if err != nil {
			return fmt.Errorf("receive: %w", err)
		}
		fmt.Printf("echo %q  rtt=%v\n", got, time.Since(start).Round(time.Microsecond))
	}
	return nil
}
