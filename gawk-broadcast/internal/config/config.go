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

// DefaultServerName is the reserved profile name for the pinned default relay
// (docs/40 §4.1.1's `"default"` id, restated for this module). The default's
// *identity* is never persisted — its URL is DefaultRelayURL, recomputed on
// every use — so a ServerProfile with this name is only a credentials record:
// a publish secret saved against the default, keyed to the URL it was saved
// against so a moved default discards it (F9) rather than presenting it to
// the new host.
const DefaultServerName = "default"

// ServerProfile is one saved relay server (R37 SP8, docs/40 §4.8): the
// browser picker's entry schema in this module's shape. There is no
// certHashHex here because the native broadcaster has no
// serverCertificateHashes dance — it trusts the system store, with -insecure
// as the dev-cert escape hatch (see dialRelay in internal/engine).
type ServerProfile struct {
	// Name is the user-editable display name; it doubles as the selection key
	// (Config.SelectedServer), so it is unique within Servers. The name
	// "default" is reserved (DefaultServerName).
	Name string `json:"name"`
	// URL is the relay's https:// origin.
	URL string `json:"url,omitempty"`
	// PublishSecret is this server's own R2 publish secret. Per-server on
	// purpose (docs/40 D4): the secret stored for server A is never presented
	// to server B.
	PublishSecret string `json:"publishSecret,omitempty"`
}

// Config is the persisted settings. Everything is optional; the zero value is
// a usable first run.
type Config struct {
	// RelayURL is the relay's https:// origin. Blank uses DefaultRelayURL —
	// read it through Relay() or Server(), never directly.
	//
	// Since R37 this is an *override* slot, not the home of the relay choice:
	// Load migrates a pre-R37 flat value into Servers (docs/40 §4.1.2), and
	// what remains here afterwards is only what -url/GAWK_URL write per run.
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

	// Servers is the saved server list (R37 SP8): custom profiles plus at most
	// one credentials-only record for the pinned default (DefaultServerName).
	Servers []ServerProfile `json:"servers,omitempty"`
	// SelectedServer names the profile broadcasts dial: a ServerProfile.Name,
	// or DefaultServerName. Absent or unknown means the default (docs/40
	// §4.1.1's rule).
	SelectedServer string `json:"selectedServer,omitempty"`

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
	c.Migrate()
	return c, nil
}

// Migrate folds the pre-R37 flat relay/secret fields into the server list
// (docs/40 §4.1.2, SP8) and applies the F9 guard. Load applies it; it is
// exported for shells that build a Config by hand. Idempotent — a migrated
// config passes through unchanged.
//
// The two legacy shapes, exactly as §4.1.2:
//
//   - flat URL names the default fleet (or is blank) → the secret becomes the
//     default's credentials-only record, keyed to the normalized URL it was
//     saved against, and the default is selected;
//   - any other URL → one custom "Migrated server" profile carrying URL and
//     secret, selected — a user who pointed their install at a custom relay
//     keeps working without noticing.
//
// The flat fields are cleared afterwards (the frontend removes its legacy
// localStorage keys the same way); from then on they are per-run override
// slots for -url/GAWK_URL and -secret/GAWK_SECRET.
func (c *Config) Migrate() {
	// F9 guard, applied on every load, migrated or not: the default record's
	// credentials are keyed to the URL they were saved against. If the
	// built-in default has moved (a binary upgrade), discard them — the old
	// relay's secret must not be presented to the new host.
	kept := c.Servers[:0]
	for _, p := range c.Servers {
		if p.Name == DefaultServerName && normalizeRelayURL(p.URL) != normalizeRelayURL(DefaultRelayURL) {
			continue
		}
		kept = append(kept, p)
	}
	c.Servers = kept

	if len(c.Servers) > 0 || c.SelectedServer != "" {
		return // already migrated; the flat fields are overrides now
	}
	if strings.TrimSpace(c.RelayURL) == "" && c.PublishSecret == "" {
		return // nothing legacy to migrate (a first run)
	}
	if isDefaultRelay(c.RelayURL) {
		if c.PublishSecret != "" {
			c.Servers = append(c.Servers, ServerProfile{
				Name:          DefaultServerName,
				URL:           normalizeRelayURL(DefaultRelayURL),
				PublishSecret: c.PublishSecret,
			})
		}
		c.SelectedServer = DefaultServerName
	} else {
		c.Servers = append(c.Servers, ServerProfile{
			Name:          "Migrated server",
			URL:           strings.TrimSpace(c.RelayURL),
			PublishSecret: c.PublishSecret,
		})
		c.SelectedServer = "Migrated server"
	}
	c.RelayURL, c.PublishSecret = "", ""
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

// Server resolves the relay to dial and the publish secret to present — one
// call, because since R37 the two must never be resolved separately: secrets
// are stored per server (docs/40 D4) and attach only to the server they were
// saved against.
//
// URL precedence: the flat RelayURL (where -url/GAWK_URL overrides land) >
// the selected profile > DefaultRelayURL. The secret is the resolved
// *server's* own: an override URL picks up a stored secret only when it names
// a saved profile's URL (normalized comparison, same rule as isDefaultRelay);
// pointing at a server nothing is saved for presents no stored credential.
// The flat PublishSecret (an explicit -secret/GAWK_SECRET) always wins.
func (c *Config) Server() (relayURL, publishSecret string) {
	p := c.selectedProfile()
	urlRaw, secret := p.URL, p.PublishSecret
	if s := strings.TrimSpace(c.RelayURL); s != "" {
		if normalizeRelayURL(ResolveRelayURL(s)) != normalizeRelayURL(ResolveRelayURL(urlRaw)) {
			// The override names a different server than the selected profile:
			// its secret must not travel (D4). A profile saved for the
			// override's URL, if any, supplies the credential instead.
			secret = c.storedSecretFor(s)
		}
		urlRaw = s
	}
	if c.PublishSecret != "" {
		secret = c.PublishSecret
	}
	return ResolveRelayURL(urlRaw), secret
}

// selectedProfile resolves SelectedServer: the named custom profile, or the
// pinned default when the selection is absent, "default", or names a profile
// that no longer exists (docs/40 §4.1.1: absent/unknown ⇒ default).
func (c *Config) selectedProfile() ServerProfile {
	if name := c.SelectedServer; name != "" && name != DefaultServerName {
		for _, p := range c.Servers {
			if p.Name == name {
				return p
			}
		}
	}
	return c.defaultProfile()
}

// defaultProfile is the pinned default: URL recomputed from DefaultRelayURL
// (never persisted, docs/40 D5), credentials from the default record when one
// exists.
func (c *Config) defaultProfile() ServerProfile {
	p := ServerProfile{Name: DefaultServerName, URL: DefaultRelayURL}
	for _, q := range c.Servers {
		if q.Name == DefaultServerName {
			p.PublishSecret = q.PublishSecret
			break
		}
	}
	return p
}

// storedSecretFor returns the secret saved against rawURL's server, or "" when
// no profile names it. The default record participates like any other entry.
func (c *Config) storedSecretFor(rawURL string) string {
	n := normalizeRelayURL(ResolveRelayURL(rawURL))
	for _, p := range c.Servers {
		if normalizeRelayURL(ResolveRelayURL(p.URL)) == n {
			return p.PublishSecret
		}
	}
	return ""
}

// DefaultSecret is the publish secret saved against the pinned default, "" for
// none.
func (c *Config) DefaultSecret() string { return c.defaultProfile().PublishSecret }

// SetDefaultSecret stores, rotates, or ("") clears the pinned default's publish
// secret — the docs/40 F4 rotation path. The record it writes is keyed to the
// current default URL, so the F9 guard can discard it if the default moves.
func (c *Config) SetDefaultSecret(secret string) {
	for i := range c.Servers {
		if c.Servers[i].Name != DefaultServerName {
			continue
		}
		if secret == "" {
			c.Servers = append(c.Servers[:i], c.Servers[i+1:]...)
			return
		}
		c.Servers[i].URL = normalizeRelayURL(DefaultRelayURL)
		c.Servers[i].PublishSecret = secret
		return
	}
	if secret != "" {
		c.Servers = append(c.Servers, ServerProfile{
			Name:          DefaultServerName,
			URL:           normalizeRelayURL(DefaultRelayURL),
			PublishSecret: secret,
		})
	}
}

// CustomServers returns the saved custom profiles in order, the default's
// credentials record excluded.
func (c *Config) CustomServers() []ServerProfile {
	var out []ServerProfile
	for _, p := range c.Servers {
		if p.Name != DefaultServerName {
			out = append(out, p)
		}
	}
	return out
}

// AddCustomServer appends a new empty profile under a unique placeholder name
// and returns it.
func (c *Config) AddCustomServer() ServerProfile {
	name := "New server"
	for i := 2; c.profileNameTaken(name); i++ {
		name = fmt.Sprintf("New server %d", i)
	}
	p := ServerProfile{Name: name}
	c.Servers = append(c.Servers, p)
	return p
}

// UpdateCustomServer replaces the i-th custom profile (CustomServers order).
// A rename follows the selection so the profile stays selected; an empty,
// reserved, or already-taken new name keeps the old one — Name is the
// selection key, so a collision would make two profiles indistinguishable.
func (c *Config) UpdateCustomServer(i int, p ServerProfile) {
	idx := c.customIndex(i)
	if idx < 0 {
		return
	}
	old := c.Servers[idx]
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || p.Name == DefaultServerName || (p.Name != old.Name && c.profileNameTaken(p.Name)) {
		p.Name = old.Name
	}
	c.Servers[idx] = p
	if c.SelectedServer == old.Name {
		c.SelectedServer = p.Name
	}
}

// RemoveCustomServer deletes the i-th custom profile (CustomServers order); a
// selection pointing at it falls back to the default.
func (c *Config) RemoveCustomServer(i int) {
	idx := c.customIndex(i)
	if idx < 0 {
		return
	}
	if c.SelectedServer == c.Servers[idx].Name {
		c.SelectedServer = DefaultServerName
	}
	c.Servers = append(c.Servers[:idx], c.Servers[idx+1:]...)
}

// customIndex maps a CustomServers index to a Servers index, or -1.
func (c *Config) customIndex(i int) int {
	n := 0
	for idx, p := range c.Servers {
		if p.Name == DefaultServerName {
			continue
		}
		if n == i {
			return idx
		}
		n++
	}
	return -1
}

func (c *Config) profileNameTaken(name string) bool {
	for _, p := range c.Servers {
		if p.Name == name {
			return true
		}
	}
	return false
}

// Relay is the relay URL to dial, defaults applied.
func (c *Config) Relay() string {
	relayURL, _ := c.Server()
	return relayURL
}

// Telemetry is the ingest endpoint to POST to, defaults applied; "" means this
// process reports nowhere. The pairing rule (see ResolveTelemetryURL) is
// applied against the *resolved* relay, so a selected foreign profile reports
// nowhere unless an endpoint is configured explicitly — exactly the docs/40
// §4.10 guard, which a relay-advertised endpoint (wire 0x12) then overrides
// at the reporter.
func (c *Config) Telemetry() string {
	relayURL, _ := c.Server()
	return ResolveTelemetryURL(relayURL, c.TelemetryURL)
}

// TelemetryOff reports whether raw is the explicit telemetry opt-out. It
// exists for the shells' docs/40 §4.10 wiring: ResolveTelemetryURL returns ""
// both for "off" and for "unset on a foreign relay", and only the former
// forbids honoring a relay-advertised 0x12 endpoint — off means off.
func TelemetryOff(raw string) bool { return strings.EqualFold(strings.TrimSpace(raw), Off) }

// Path returns where this config lives.
func (c *Config) Path() string { return c.path }

// SetPath points the config at a file (for a config built from flags).
func (c *Config) SetPath(p string) { c.path = p }
