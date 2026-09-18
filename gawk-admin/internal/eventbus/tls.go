package eventbus

import (
	"crypto/tls"
	"strings"
)

// insecureTLS backs -eventbus-insecure: the docs/41 compose lane only. It is a
// flag with no chart value, it warns at every start, and it lives in its own
// file so a grep for InsecureSkipVerify lands on the comment that says why.
func insecureTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the flag's whole purpose
}

// wantsTLS reports whether a NATS URL asks for a TLS connection.
//
// nats.Secure REQUIRES TLS rather than merely relaxing it, so applying it to a
// plain nats:// server makes every handshake fail — and the client hides that
// in its reconnect loop, so the portal just never receives anything. The flag
// means "do not verify the certificate", which a connection without one cannot
// do.
func wantsTLS(url string) bool {
	return strings.HasPrefix(url, "tls://") || strings.HasPrefix(url, "wss://")
}
