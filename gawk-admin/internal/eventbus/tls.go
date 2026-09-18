package eventbus

import (
	"crypto/tls"

	"github.com/nats-io/nats.go"
)

// insecureSkipVerify backs -eventbus-insecure: the docs/41 compose lane only.
// It is a flag with no chart value, it warns at every start, and it lives in
// its own file so a grep for InsecureSkipVerify lands on the comment that says
// why.
//
// It relaxes verification WITHOUT requiring TLS. nats.Secure does both, and
// the difference is not cosmetic: applied to a plain nats:// server it makes
// every handshake fail and the client buries that in its reconnect loop —
// the portal simply never receives anything. It matters the other way round
// too, because a NATS that requires TLS may still be dialled as nats://: the
// client upgrades from the server's INFO, TLS is not implied by the scheme.
func insecureSkipVerify() nats.Option {
	return func(o *nats.Options) error {
		if o.TLSConfig == nil {
			o.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		o.TLSConfig.InsecureSkipVerify = true //nolint:gosec // the flag's whole purpose
		return nil
	}
}
