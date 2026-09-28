// Package clientinfo turns the client-identity dial parameters (R59,
// docs/61 D1) into metric label values from a closed vocabulary.
//
// A client names itself with ?app=, ?os= and ?browser= on its publish or
// subscribe dial. Nothing here is trusted or logged: a value outside the
// vocabulary becomes "other" and an absent one "unknown", so whatever a
// client sends, the label set stays bounded.
package clientinfo

import "net/url"

const (
	// Unknown is the label for a parameter the client did not send (every
	// pre-R59 client, and the frozen Go broadcaster).
	Unknown = "unknown"
	// Other is the label for a value outside the vocabulary.
	Other = "other"
)

var (
	apps     = map[string]bool{"web": true, "desktop": true}
	oses     = map[string]bool{"windows": true, "macos": true, "linux": true, "android": true, "ios": true, "chromeos": true}
	browsers = map[string]bool{"chromium": true, "firefox": true, "safari": true}
)

// Info is one client's normalized identity.
type Info struct {
	App     string
	OS      string
	Browser string
}

// FromQuery reads the identity parameters from a dial URL's query.
func FromQuery(q url.Values) Info {
	return Info{
		App:     pick(q, "app", apps),
		OS:      pick(q, "os", oses),
		Browser: pick(q, "browser", browsers),
	}
}

func pick(q url.Values, key string, vocab map[string]bool) string {
	v, ok := q[key]
	if !ok || len(v) == 0 || v[0] == "" {
		return Unknown
	}
	if vocab[v[0]] {
		return v[0]
	}
	return Other
}
