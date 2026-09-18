package eventbus

import "crypto/tls"

// insecureTLS backs -eventbus-insecure: the docs/41 compose lane only. It is a
// flag with no chart value, it warns at every start, and it lives in its own
// file so a grep for InsecureSkipVerify lands on the comment that says why.
func insecureTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the flag's whole purpose
}
