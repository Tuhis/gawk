package eventbus

import (
	"crypto/tls"
	"strings"

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
//
// Since nats.go v1.56 a non-nil Options.TLSConfig itself asks for TLS, so the
// switch may only touch a TLS config when something else already asks for
// TLS: a tls:// or wss:// URL, or a CA or client certificate (nats.RootCAs and
// nats.ClientCert fill TLSConfig in, which is why this option is applied after
// them). On a plain nats:// URL it does nothing; a TLS-requiring server with a
// certificate nothing trusts must therefore be spelled tls:// to be reached
// with the switch on.
func insecureSkipVerify() nats.Option {
	return func(o *nats.Options) error {
		if o.TLSConfig == nil {
			if !natsURLsAskForTLS(o) {
				return nil
			}
			o.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		o.TLSConfig.InsecureSkipVerify = true //nolint:gosec // the flag's whole purpose
		return nil
	}
}

// natsURLsAskForTLS reports whether any configured server URL has a TLS
// scheme, which makes nats.go use TLS for the whole connection.
func natsURLsAskForTLS(o *nats.Options) bool {
	urls := append([]string{o.Url}, o.Servers...)
	for _, u := range urls {
		for _, one := range strings.Split(u, ",") {
			scheme, _, ok := strings.Cut(strings.TrimSpace(one), "://")
			if ok && (strings.EqualFold(scheme, "tls") || strings.EqualFold(scheme, "wss")) {
				return true
			}
		}
	}
	return false
}
