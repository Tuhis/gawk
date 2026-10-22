//go:build !windows

package relaystate

// Off Windows a secret is protected by the 0600 file mode and the 0700 state
// directory alone, as an SSH key is.
func protect(b []byte) ([]byte, error) { return append([]byte(nil), b...), nil }

func unprotect(b []byte) ([]byte, error) { return b, nil }
