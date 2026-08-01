// Package config is the broadcaster's persisted settings (R14, docs/19).
//
// The file is 0600 and it matters: it holds the publish secret, which is a
// credential, not a preference.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// FileMode is the config file's permission. A pre-shared secret in a local
// file is consistent with how it already travels (a query param, per R2 — the
// WebTransport JS API cannot set headers); 0600 keeps it from being read by
// other users on the machine.
const FileMode fs.FileMode = 0o600

const (
	// DefaultRelayURL is the reference deployment's relay. An unconfigured
	// binary points here rather than refusing to start: this is the fleet the
	// people who get handed this binary actually broadcast to, and "download it
	// and press Start" is the experience worth having. Any other relay is one
	// -url (or one settings field) away.
	DefaultRelayURL = "https://api.gawk.ioio.fi:4433"

	// DefaultTelemetryURL is that same fleet's R28 ingest endpoint. It is on the
	// *frontend* origin, not the relay's — telemetry is a same-origin path on
	// the app's Ingress while the relay is a UDP LoadBalancer, which is why this
	// is a second constant and not derived from DefaultRelayURL.
	DefaultTelemetryURL = "https://gawk.ioio.fi/api/telemetry/v1/ingest"

	// Off disables telemetry from a place that can only hold a string — a flag,
	// an env var, a settings field. Empty cannot mean "off" here because empty
	// means "use the default", so the opt-out needs a word of its own; the relay
	// already spells the same idea the same way (-metrics-addr off, R9).
	Off = "off"
)

// ResolveRelayURL turns a stored/flag/env relay URL into the one to dial.
// Blank means "whatever the default fleet is", so the default can move with a
// release instead of being frozen into every user's config file the first time
// they press Save.
func ResolveRelayURL(raw string) string {
	if s := strings.TrimSpace(raw); s != "" {
		return s
	}
	return DefaultRelayURL
}

// ResolveTelemetryURL turns a stored/flag/env telemetry URL into the ingest
// endpoint to POST to, or "" for no reporting at all.
//
// Blank means on-by-default, but only against the default fleet. The token in
// an R28 hello is an HMAC minted with *that relay's* telemetry key (docs/33
// D2), so a batch from a self-hosted relay would be rejected by the reference
// collector anyway — and pointing someone's private deployment at a third
// party's collector by default is the wrong default even when nothing lands.
// So the pairing is the rule: default relay ⇒ default collector; any other
// relay ⇒ nothing, unless a telemetry URL is given explicitly.
func ResolveTelemetryURL(relayRaw, telemetryRaw string) string {
	switch s := strings.TrimSpace(telemetryRaw); {
	case strings.EqualFold(s, Off):
		return ""
	case s != "":
		return s
	case isDefaultRelay(relayRaw):
		return DefaultTelemetryURL
	default:
		return ""
	}
}

// isDefaultRelay reports whether raw names the reference fleet's relay.
//
// The comparison is on the parsed address, not on the string. "Is this the
// default fleet?" is a question about which relay this is, and a trailing
// slash or a capitalized scheme does not change the answer — but a raw string
// compare says it does, and then silently reports nowhere. Silence is the
// whole problem: the only symptom is a session that never appears in the
// telemetry service, which looks identical to a client that crashed, a relay
// with no key, and a network that ate the batch.
func isDefaultRelay(raw string) bool {
	return normalizeRelayURL(ResolveRelayURL(raw)) == normalizeRelayURL(DefaultRelayURL)
}

// normalizeRelayURL reduces a relay URL to a comparable form: scheme and host
// lowercased, a bare trailing slash dropped. Only for comparison — what gets
// dialed is always what the user typed, via ResolveRelayURL.
//
// A value that does not parse is returned trimmed but otherwise untouched, so
// a malformed relay URL can only ever fail to match. Erring toward "not the
// default" keeps the pairing rule's guarantee intact: a private deployment is
// never pointed at the reference collector by accident.
func normalizeRelayURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return trimmed
	}
	out := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimSuffix(u.Path, "/")
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.Fragment
	}
	return out
}

// Config is the persisted settings. Everything is optional; the zero value is
// a usable first run.
type Config struct {
	// RelayURL is the relay's https:// origin. Blank uses DefaultRelayURL —
	// read it through Relay(), never directly.
	RelayURL string `json:"relayUrl,omitempty"`
	// AppURL is the *frontend* origin, used to build "join:" links and Copy
	// link. It is deliberately separate: it is not derivable from the relay
	// URL — they are different hosts (the relay is a UDP LoadBalancer, the app
	// is behind an nginx Ingress).
	AppURL string `json:"appUrl,omitempty"`
	// PublishSecret is R2's pre-shared publish secret.
	PublishSecret string `json:"publishSecret,omitempty"`
	// TelemetryURL is the R28 ingest endpoint (docs/33 D1), e.g.
	// https://gawk.example/api/telemetry/v1/ingest. Blank reports to the default
	// fleet's collector when the relay is also the default one; the literal
	// Off disables reporting entirely, and this binary then makes no telemetry
	// request at all. Read it through Telemetry(), never directly.
	//
	// It is NOT derivable from RelayURL: telemetry is served from the frontend
	// origin, which is a different host by construction (the relay is a UDP
	// LoadBalancer, the app an Ingress) — the same reason AppURL exists as its
	// own field.
	TelemetryURL string `json:"telemetryUrl,omitempty"`
	// Origin overrides the Origin header sent on the CONNECT dial. Empty uses
	// engine.DefaultOrigin. Set it to reuse an already-whitelisted origin (e.g.
	// the frontend's) instead of adding a new -allowed-origins entry on the
	// relay.
	Origin string `json:"origin,omitempty"`
	// LastBroadcastID backs the GUI's Resume.
	LastBroadcastID string `json:"lastBroadcastId,omitempty"`
	// LastResumeToken is the relay-minted resume token for LastBroadcastID
	// (R17, hex). A reclaim without it is refused by an R17 relay, so it is
	// persisted — and like the publish secret it is a credential (whoever
	// holds it can take over the broadcast ID), which the 0600 file mode
	// already covers.
	LastResumeToken string `json:"lastResumeToken,omitempty"`
	// LastGoodEncoder is the cached cascade winner (Decision 4). Re-verified
	// on use, never trusted.
	LastGoodEncoder string `json:"lastGoodEncoder,omitempty"`
	// LastGoodAudioSource is the cached audio cascade winner (R25, docs/28
	// Decision 2). Same rule as LastGoodEncoder: re-verified, never trusted.
	LastGoodAudioSource string `json:"lastGoodAudioSource,omitempty"`

	// DisableAudio turns the system-audio lane off. Spelled as a *disable*
	// flag on purpose, so the zero value — a fresh config, or one written
	// before R25 — means audio on (docs/28 Decision 11).
	DisableAudio bool `json:"disableAudio,omitempty"`
	// AudioDevice pins one capture device by name (pulsesrc's device
	// property), skipping the cascade. Empty probes.
	AudioDevice string `json:"audioDevice,omitempty"`
	// AudioApp is the application whose audio is captured when a *window* is
	// shared (R35, docs/39 AD2/AD3): its `application.process.binary`. It is
	// persisted only as the GUI's preselection — the whose-audio step still
	// appears on every start, the same way the portal picker does (AD3), so
	// this can never silently change what a broadcast publishes. Ignored when
	// a whole screen is shared, and by AudioDevice, which is the bigger
	// hammer.
	AudioApp string `json:"audioApp,omitempty"`

	// Rung.
	Width      int `json:"width,omitempty"`
	Height     int `json:"height,omitempty"`
	Fps        int `json:"fps,omitempty"`
	BitrateBps int `json:"bitrateBps,omitempty"`
	// Encoder pins one cascade candidate.
	Encoder string `json:"encoder,omitempty"`

	// path is where this was loaded from; Save writes back to it.
	path string
}

// DefaultPath is ~/.config/gawk/broadcast.json, honouring XDG_CONFIG_HOME.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gawk", "broadcast.json"), nil
}

// Load reads the config. A missing file is not an error — it is a first run.
func Load(path string) (*Config, error) {
	c := &Config{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		// A corrupt config must not brick the app: say so, keep the defaults.
		return c, fmt.Errorf("config %s is not valid JSON (using defaults): %w", path, err)
	}
	c.path = path
	return c, nil
}

// Save writes the config atomically, 0600.
//
// Atomic because a crash mid-write would otherwise corrupt the file (the
// "unexpected end of JSON input" a truncated write produces), losing the relay
// URL, secret and last-good encoder in one go.
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config: no path to save to")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".broadcast-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	// Chmod before writing: the secret must never exist on disk world-readable,
	// not even briefly.
	if err := tmp.Chmod(FileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, c.path)
}

// Relay is the relay URL to dial, defaults applied.
func (c *Config) Relay() string { return ResolveRelayURL(c.RelayURL) }

// Telemetry is the ingest endpoint to POST to, defaults applied; "" means this
// process reports nowhere.
func (c *Config) Telemetry() string { return ResolveTelemetryURL(c.RelayURL, c.TelemetryURL) }

// Path returns where this config lives.
func (c *Config) Path() string { return c.path }

// SetPath points the config at a file (for a config built from flags).
func (c *Config) SetPath(p string) { c.path = p }
