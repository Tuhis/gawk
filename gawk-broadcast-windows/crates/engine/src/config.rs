//! Persisted settings (docs/38 D13/D14), ported from
//! gawk-broadcast/internal/config: `%APPDATA%\gawk\broadcast.json` on
//! Windows (an injected path in tests and on other hosts), the same
//! lowerCamelCase key names as the Linux config where fields are shared,
//! atomic writes, and a corrupt file that warns and keeps defaults rather
//! than failing.
//!
//! Two rules carry over verbatim:
//! - **Blank means "the default", resolved at use, never at save** — the
//!   file never stores a resolved default, so a release that moves the
//!   fleet address moves every user with it.
//! - **The telemetry pairing rule**: blank telemetry reports to the
//!   reference collector only when the relay is also the default one (the
//!   session token is an HMAC minted by the relay you connected to, and
//!   pointing a private deployment at a third party's collector by default
//!   is the wrong default even when nothing lands). Comparison is on the
//!   PARSED address, never the raw string — a malformed URL can only ever
//!   fail to match.

use crate::defaults;
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};

/// Disables telemetry from a place that can only hold a string. Empty cannot
/// mean "off" because empty means "use the default", so the opt-out needs a
/// word of its own.
pub const OFF: &str = "off";

/// The reserved profile name for the pinned default relay (docs/40 §4.1.1's
/// `"default"` id, in this module's shape — same vocabulary as the Linux
/// broadcaster's `config.DefaultServerName`, docs/38 D14). The default's
/// *identity* is never stored — its URL is the compile-time default,
/// re-resolved every load — but a record with this name may exist to carry
/// the default's credentials, keyed to the URL they were saved against (F9).
pub const DEFAULT_SERVER_NAME: &str = "default";

/// One saved relay server (R37 SP9, docs/40 §4.8): the same shape and JSON
/// keys as the Linux broadcaster's `config.ServerProfile` (docs/38 D14 —
/// shared vocabulary keeps docs and diagnostics legible cross-OS). No
/// cert-hash field: the native broadcasters trust the system store.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct ServerProfile {
    /// The user-editable display name; it doubles as the selection key
    /// (`selected_server`), so it is unique within `servers`.
    /// [`DEFAULT_SERVER_NAME`] is reserved.
    pub name: String,
    /// https origin, e.g. "https://relay.example.com:4433". For the default's
    /// credentials-only record this holds the default URL the credentials
    /// were saved against — the F9 guard key.
    pub url: String,
    /// Per-server credential (DPAPI-wrapped on Windows, like the flat field;
    /// docs/40 D4: the secret stored for server A is never presented to B).
    pub publish_secret: String,
}

/// The persisted settings. Field names are the wire-visible JSON keys —
/// lowerCamelCase, matching the Linux broadcaster's file where shared.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Config {
    pub relay_url: String,
    pub app_url: String,
    /// A credential (DPAPI-wrapped on Windows — WB6 supplies the wrapper;
    /// see [`Credentials`]).
    pub publish_secret: String,
    pub telemetry_url: String,
    pub origin: String,
    pub last_broadcast_id: String,
    /// Hex; a credential like the secret.
    pub last_resume_token: String,
    pub last_good_encoder: String,
    pub disable_audio: bool,
    /// "app" or "screen" — the Windows-only capture mode (docs/38 D6).
    pub capture_mode: String,
    pub width: u32,
    pub height: u32,
    pub fps: u32,
    pub bitrate_bps: u32,
    /// Saved relay servers (R37 SP9). The legacy flat `relay_url` /
    /// `publish_secret` pair migrates into this list once ([`migrate`]).
    pub servers: Vec<ServerProfile>,
    /// The selected profile's name; blank, `"default"`, or unknown ⇒ the
    /// built-in default (docs/40 §4.1.1).
    pub selected_server: String,
}

impl Config {
    /// The selected custom server profile, or `None` when the built-in
    /// default is selected (blank/`"default"`/unknown name — the default's
    /// identity is never a stored profile's).
    pub fn selected_profile(&self) -> Option<&ServerProfile> {
        if self.selected_server.is_empty() || self.selected_server == DEFAULT_SERVER_NAME {
            return None;
        }
        self.servers.iter().find(|p| p.name == self.selected_server)
    }

    /// The raw (unresolved) relay URL the selection points at: the selected
    /// custom profile's URL, else the legacy flat field (blank after
    /// migration ⇒ the default, resolved at use).
    fn selected_relay_raw(&self) -> &str {
        match self.selected_profile() {
            Some(p) => &p.url,
            None => &self.relay_url,
        }
    }

    /// The relay URL to dial: the selected profile's, blank meaning the
    /// default fleet.
    pub fn resolve_relay_url(&self) -> String {
        resolve_relay_url(self.selected_relay_raw())
    }

    /// The publish secret to present: the selected custom profile's, or —
    /// on the default — the default's credentials-only record, falling back
    /// to the legacy flat field until migration has run.
    pub fn resolve_publish_secret(&self) -> String {
        if let Some(p) = self.selected_profile() {
            return p.publish_secret.clone();
        }
        if let Some(rec) = self.servers.iter().find(|p| p.name == DEFAULT_SERVER_NAME) {
            return rec.publish_secret.clone();
        }
        self.publish_secret.clone()
    }

    /// Stores, rotates, or (`""`) clears the pinned default's publish secret
    /// — the docs/40 F4 rotation path, mirroring the Linux
    /// `Config.SetDefaultSecret`. The record is keyed to the current default
    /// URL, so the F9 guard can discard it if the default moves.
    pub fn set_default_secret(&mut self, secret: &str) {
        if let Some(i) = self
            .servers
            .iter()
            .position(|p| p.name == DEFAULT_SERVER_NAME)
        {
            if secret.is_empty() {
                self.servers.remove(i);
            } else {
                self.servers[i].url = normalize_relay_url(defaults::RELAY_URL);
                self.servers[i].publish_secret = secret.to_owned();
            }
            return;
        }
        if !secret.is_empty() {
            self.servers.push(ServerProfile {
                name: DEFAULT_SERVER_NAME.to_owned(),
                url: normalize_relay_url(defaults::RELAY_URL),
                publish_secret: secret.to_owned(),
            });
        }
    }

    /// Appends a new empty profile under a unique placeholder name and
    /// returns that name (the Linux `Config.AddCustomServer`).
    pub fn add_custom_server(&mut self) -> String {
        let mut name = "New server".to_owned();
        let mut i = 2;
        while self.profile_name_taken(&name) {
            name = format!("New server {i}");
            i += 1;
        }
        self.servers.push(ServerProfile {
            name: name.clone(),
            url: String::new(),
            publish_secret: String::new(),
        });
        name
    }

    /// True when a profile (the default's record included) claims `name` —
    /// names are the selection key, so a collision would make two profiles
    /// indistinguishable.
    pub fn profile_name_taken(&self, name: &str) -> bool {
        name == DEFAULT_SERVER_NAME || self.servers.iter().any(|p| p.name == name)
    }

    /// The frontend origin for join links: blank means the default.
    pub fn resolve_app_url(&self) -> String {
        let s = self.app_url.trim();
        if s.is_empty() {
            defaults::APP_URL.to_owned()
        } else {
            s.to_owned()
        }
    }

    /// The Origin header value: blank means the default.
    pub fn resolve_origin(&self) -> String {
        let s = self.origin.trim();
        if s.is_empty() {
            defaults::ORIGIN.to_owned()
        } else {
            s.to_owned()
        }
    }

    /// The telemetry ingest to POST to, or `None` for no reporting (the
    /// pairing rule against the SELECTED relay; `"off"` always wins).
    pub fn resolve_telemetry_url(&self) -> Option<String> {
        resolve_telemetry_url(self.selected_relay_raw(), &self.telemetry_url)
    }

    /// The R37 precedence (docs/40 §4.10 D15) over [`resolve_telemetry_url`]:
    /// a relay-advertised 0x12 ingest URL wins over the configured one, but
    /// the user's explicit `"off"` still wins over everything — the advertised
    /// URL moves the destination, never the opt-out. The §4.10 guard is the
    /// pairing rule itself: on a non-default relay with no advertised URL and
    /// no explicit telemetry URL this returns `None`, and the reporter sends
    /// nothing.
    pub fn effective_telemetry_url(&self, advertised: Option<&str>) -> Option<String> {
        effective_telemetry_url(self.selected_relay_raw(), &self.telemetry_url, advertised)
    }

    /// The encode rung, with zeros meaning the fixed default (docs/38 D11).
    pub fn resolve_rung(&self) -> (u32, u32, u32, u32) {
        (
            if self.width == 0 {
                defaults::WIDTH
            } else {
                self.width
            },
            if self.height == 0 {
                defaults::HEIGHT
            } else {
                self.height
            },
            if self.fps == 0 {
                defaults::FPS
            } else {
                self.fps
            },
            if self.bitrate_bps == 0 {
                defaults::BITRATE_BPS
            } else {
                self.bitrate_bps
            },
        )
    }
}

/// Blank means "whatever the default fleet is".
pub fn resolve_relay_url(raw: &str) -> String {
    let s = raw.trim();
    if s.is_empty() {
        defaults::RELAY_URL.to_owned()
    } else {
        s.to_owned()
    }
}

/// The pairing rule: `off` ⇒ none; explicit ⇒ that; blank ⇒ the default
/// collector only when the relay is the default one.
pub fn resolve_telemetry_url(relay_raw: &str, telemetry_raw: &str) -> Option<String> {
    let s = telemetry_raw.trim();
    if s.eq_ignore_ascii_case(OFF) {
        return None;
    }
    if !s.is_empty() {
        return Some(s.to_owned());
    }
    if is_default_relay(relay_raw) {
        return Some(defaults::TELEMETRY_URL.to_owned());
    }
    None
}

/// The 0x12 precedence (docs/40 §4.10 D15): `"off"` → nothing (the local
/// opt-out beats an advertised destination); a valid advertised URL → that;
/// else the existing pairing rule. An advertised value that is not an
/// absolute https URL is ignored, never adopted (the wire parser already
/// refused it; this re-validates because the caller is a seam tests drive
/// with arbitrary strings).
pub fn effective_telemetry_url(
    relay_raw: &str,
    telemetry_raw: &str,
    advertised: Option<&str>,
) -> Option<String> {
    if telemetry_raw.trim().eq_ignore_ascii_case(OFF) {
        return None;
    }
    if let Some(a) = advertised
        && is_valid_ingest_url(a)
    {
        return Some(a.to_owned());
    }
    resolve_telemetry_url(relay_raw, telemetry_raw)
}

/// Absolute https, parseable, bounded — the client-side restatement of the
/// wire package's TelemetryEndpoint URL rule (docs/40 §5).
fn is_valid_ingest_url(s: &str) -> bool {
    if s.is_empty() || s.len() > 512 {
        return false;
    }
    match url::Url::parse(s) {
        Ok(u) => u.scheme() == "https" && u.host_str().is_some_and(|h| !h.is_empty()),
        Err(_) => false,
    }
}

fn is_default_relay(raw: &str) -> bool {
    normalize_relay_url(&resolve_relay_url(raw)) == normalize_relay_url(defaults::RELAY_URL)
}

/// Reduces a relay URL to a comparable form: scheme and host lowercased, a
/// bare trailing slash dropped. Only for comparison — what gets dialed is
/// always what the user typed. A value that does not parse is returned
/// trimmed but otherwise untouched, so a malformed URL can only ever fail to
/// match ("not the default" is the safe direction).
fn normalize_relay_url(raw: &str) -> String {
    let trimmed = raw.trim();
    let Ok(u) = url::Url::parse(trimmed) else {
        return trimmed.to_owned();
    };
    let Some(host) = u.host_str() else {
        return trimmed.to_owned();
    };
    let mut out = format!("{}://{}", u.scheme().to_lowercase(), host.to_lowercase());
    if let Some(port) = u.port() {
        out.push_str(&format!(":{port}"));
    }
    out.push_str(u.path().trim_end_matches('/'));
    if let Some(q) = u.query() {
        out.push('?');
        out.push_str(q);
    }
    if let Some(f) = u.fragment() {
        out.push('#');
        out.push_str(f);
    }
    out
}

/// Wraps/unwraps the two credential fields for storage. The Windows shell
/// supplies a DPAPI implementation (docs/38 D14: `dpapi:<base64>`,
/// per-user, so a copied file leaks nothing on another machine); tests and
/// non-Windows hosts use [`Plaintext`]. Plaintext values found in the file
/// are always accepted — hand-editing stays possible — and re-wrapped on
/// the next save.
pub trait Credentials {
    fn wrap(&self, value: &str) -> String;
    fn unwrap(&self, stored: &str) -> String;
}

/// Identity wrapper: what the file holds is the value.
pub struct Plaintext;

impl Credentials for Plaintext {
    fn wrap(&self, value: &str) -> String {
        value.to_owned()
    }
    fn unwrap(&self, stored: &str) -> String {
        stored.to_owned()
    }
}

/// The config file location: `%APPDATA%\gawk\broadcast.json` on Windows —
/// same filename as the Linux broadcaster's (no collision is possible
/// cross-OS, and shared vocabulary keeps docs legible).
pub fn default_path() -> Option<PathBuf> {
    #[cfg(windows)]
    {
        std::env::var_os("APPDATA").map(|d| PathBuf::from(d).join("gawk").join("broadcast.json"))
    }
    #[cfg(not(windows))]
    {
        // Dev hosts only (the product is Windows): keep the Linux
        // broadcaster's location so a dev box has one gawk config story.
        std::env::var_os("HOME").map(|d| {
            PathBuf::from(d)
                .join(".config")
                .join("gawk")
                .join("broadcast.json")
        })
    }
}

/// Loads the config. A missing file is defaults; a CORRUPT file is a warning
/// and defaults — never fatal (the Go rule: the app must start).
pub fn load(path: &Path, creds: &dyn Credentials) -> (Config, Option<String>) {
    let bytes = match std::fs::read(path) {
        Ok(b) => b,
        Err(_) => return (Config::default(), None),
    };
    match serde_json::from_slice::<Config>(&bytes) {
        Ok(mut cfg) => {
            cfg.publish_secret = creds.unwrap(&cfg.publish_secret);
            cfg.last_resume_token = creds.unwrap(&cfg.last_resume_token);
            for p in &mut cfg.servers {
                p.publish_secret = creds.unwrap(&p.publish_secret);
            }
            (cfg, None)
        }
        Err(e) => (
            Config::default(),
            Some(format!(
                "config file {} is corrupt ({e}); using defaults",
                path.display()
            )),
        ),
    }
}

/// The R37 SP9 migration (docs/40 §4.1.2) plus the F9 guard, mirroring the
/// Linux `Config.Migrate` decision for decision. Run in memory after every
/// load; returns true when it changed anything (the shell then saves, which
/// is what "legacy keys removed after a successful write" means for a file
/// config). Idempotent — a migrated config passes through unchanged.
///
/// The two legacy shapes, exactly as §4.1.2:
///
/// - Flat URL blank or naming the built-in default (parsed comparison) ⇒ the
///   flat secret (if any) becomes the default's credentials-only record,
///   keyed to the normalized URL it was saved against, and the default is
///   selected.
/// - Any other URL ⇒ one custom "Migrated server" profile carrying URL and
///   secret, selected — a user who pointed their install at a custom relay
///   keeps working without noticing.
///
/// F9, applied on every load, migrated or not: a default credential record
/// whose key no longer matches the recomputed default URL is discarded — a
/// binary upgrade that moves the fleet must not present the old relay's
/// secret to the new host.
pub fn migrate(cfg: &mut Config) -> bool {
    // F9 prune first, so stale default credentials never survive a load.
    let default_key = normalize_relay_url(defaults::RELAY_URL);
    let before = cfg.servers.len();
    cfg.servers
        .retain(|p| p.name != DEFAULT_SERVER_NAME || normalize_relay_url(&p.url) == default_key);
    let changed = cfg.servers.len() != before;

    if !cfg.servers.is_empty() || !cfg.selected_server.is_empty() {
        return changed; // already migrated
    }
    let flat_url = cfg.relay_url.trim().to_owned();
    let flat_secret = cfg.publish_secret.clone();
    if flat_url.is_empty() && flat_secret.is_empty() {
        return changed; // nothing legacy to migrate (a first run)
    }

    if is_default_relay(&flat_url) {
        if !flat_secret.is_empty() {
            cfg.servers.push(ServerProfile {
                name: DEFAULT_SERVER_NAME.to_owned(),
                url: default_key,
                publish_secret: flat_secret,
            });
        }
        cfg.selected_server = DEFAULT_SERVER_NAME.to_owned();
    } else {
        cfg.servers.push(ServerProfile {
            name: "Migrated server".to_owned(),
            url: flat_url,
            publish_secret: flat_secret,
        });
        cfg.selected_server = "Migrated server".to_owned();
    }
    cfg.relay_url.clear();
    cfg.publish_secret.clear();
    true
}

/// Saves atomically: temp file in the same directory, then rename — a crash
/// mid-write cannot corrupt the config. Credentials are wrapped on the way
/// out (the flat pair AND every profile's secret, docs/38 D14); resolved
/// defaults are NEVER written (blank stays blank).
pub fn save(path: &Path, cfg: &Config, creds: &dyn Credentials) -> Result<(), String> {
    let mut stored = cfg.clone();
    stored.publish_secret = creds.wrap(&cfg.publish_secret);
    stored.last_resume_token = creds.wrap(&cfg.last_resume_token);
    for p in &mut stored.servers {
        p.publish_secret = creds.wrap(&p.publish_secret);
    }
    let json = serde_json::to_vec_pretty(&stored).map_err(|e| e.to_string())?;

    let dir = path.parent().ok_or("config path has no parent directory")?;
    std::fs::create_dir_all(dir).map_err(|e| e.to_string())?;
    let tmp = path.with_extension("json.tmp");
    std::fs::write(&tmp, &json).map_err(|e| e.to_string())?;
    std::fs::rename(&tmp, path).map_err(|e| e.to_string())?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn blank_means_the_default_resolved_at_use() {
        let cfg = Config::default();
        assert_eq!(cfg.resolve_relay_url(), defaults::RELAY_URL);
        assert_eq!(cfg.resolve_app_url(), defaults::APP_URL);
        assert_eq!(cfg.resolve_origin(), defaults::ORIGIN);
        assert_eq!(cfg.resolve_rung(), (1920, 1080, 60, 12_000_000));

        let cfg = Config {
            relay_url: "  https://other.example:4433 ".into(),
            ..Default::default()
        };
        assert_eq!(cfg.resolve_relay_url(), "https://other.example:4433");
    }

    #[test]
    fn telemetry_pairing_rule() {
        // Default relay (blank) ⇒ default collector.
        assert_eq!(
            resolve_telemetry_url("", ""),
            Some(defaults::TELEMETRY_URL.to_owned())
        );
        // The default relay spelled differently still pairs: the comparison
        // is on the parsed address, not the string.
        for spelled in [
            "https://api.gawk.ioio.fi:4433/",
            "HTTPS://API.GAWK.IOIO.FI:4433",
            " https://api.gawk.ioio.fi:4433 ",
        ] {
            assert_eq!(
                resolve_telemetry_url(spelled, ""),
                Some(defaults::TELEMETRY_URL.to_owned()),
                "{spelled}"
            );
        }
        // Any other relay ⇒ nothing, unless explicit.
        assert_eq!(
            resolve_telemetry_url("https://relay.example:4433", ""),
            None
        );
        assert_eq!(
            resolve_telemetry_url("https://relay.example:4433", "https://t.example/ingest"),
            Some("https://t.example/ingest".into())
        );
        // "off" always wins, any case.
        assert_eq!(resolve_telemetry_url("", "off"), None);
        assert_eq!(resolve_telemetry_url("", "OFF"), None);
        // A malformed relay URL can only fail to match — never the default.
        assert_eq!(resolve_telemetry_url("not a url", ""), None);
    }

    #[test]
    fn save_never_stores_resolved_defaults_and_load_round_trips() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-test-{}", std::process::id()));
        let path = dir.join("broadcast.json");
        let cfg = Config {
            last_broadcast_id: "K7XQ2M".into(),
            publish_secret: "s3cret".into(),
            ..Default::default()
        };
        save(&path, &cfg, &Plaintext).unwrap();

        let raw = std::fs::read_to_string(&path).unwrap();
        assert!(
            raw.contains("\"relayUrl\": \"\""),
            "blank stays blank: {raw}"
        );
        assert!(
            !raw.contains(defaults::RELAY_URL),
            "no resolved default in the file"
        );
        assert!(
            raw.contains("\"lastBroadcastId\": \"K7XQ2M\""),
            "camelCase keys: {raw}"
        );

        let (loaded, warn) = load(&path, &Plaintext);
        assert!(warn.is_none());
        assert_eq!(loaded, cfg);
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn a_corrupt_file_warns_and_keeps_defaults() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-corrupt-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("broadcast.json");
        std::fs::write(&path, b"{not json").unwrap();
        let (cfg, warn) = load(&path, &Plaintext);
        assert_eq!(cfg, Config::default());
        assert!(warn.unwrap().contains("corrupt"));
        std::fs::remove_dir_all(&dir).ok();
    }

    struct Reversing;
    impl Credentials for Reversing {
        fn wrap(&self, v: &str) -> String {
            if v.is_empty() {
                String::new()
            } else {
                format!("wrapped:{v}")
            }
        }
        fn unwrap(&self, s: &str) -> String {
            s.strip_prefix("wrapped:").unwrap_or(s).to_owned()
        }
    }

    #[test]
    fn credentials_are_wrapped_on_save_and_unwrapped_on_load() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-creds-{}", std::process::id()));
        let path = dir.join("broadcast.json");
        let cfg = Config {
            publish_secret: "s3cret".into(),
            last_resume_token: "deadbeef".into(),
            ..Default::default()
        };
        save(&path, &cfg, &Reversing).unwrap();
        let raw = std::fs::read_to_string(&path).unwrap();
        assert!(raw.contains("wrapped:s3cret"), "{raw}");

        // Unwrapped on load — and a hand-edited PLAINTEXT value is accepted
        // too (unwrap passes unknown formats through).
        let (loaded, _) = load(&path, &Reversing);
        assert_eq!(loaded.publish_secret, "s3cret");
        assert_eq!(loaded.last_resume_token, "deadbeef");
        std::fs::remove_dir_all(&dir).ok();
    }

    // --- R37 SP9: server profiles --------------------------------------------

    fn profile(name: &str, url: &str, secret: &str) -> ServerProfile {
        ServerProfile {
            name: name.into(),
            url: url.into(),
            publish_secret: secret.into(),
        }
    }

    #[test]
    fn every_profile_secret_is_wrapped_on_save_and_unwrapped_on_load() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-profiles-{}", std::process::id()));
        let path = dir.join("broadcast.json");
        let cfg = Config {
            servers: vec![
                profile("Server A", "https://relay.example:4433", "aaa"),
                profile("Server B", "https://other.example:4433", "bbb"),
            ],
            selected_server: "Server B".into(),
            ..Default::default()
        };
        save(&path, &cfg, &Reversing).unwrap();
        let raw = std::fs::read_to_string(&path).unwrap();
        assert!(raw.contains("wrapped:aaa"), "{raw}");
        assert!(raw.contains("wrapped:bbb"), "{raw}");
        assert!(!raw.contains("\"aaa\""), "unwrapped secret on disk: {raw}");
        // The Linux config file's key names, exactly (docs/38 D14).
        assert!(
            raw.contains("\"selectedServer\": \"Server B\""),
            "camelCase keys: {raw}"
        );
        assert!(raw.contains("\"publishSecret\""), "{raw}");

        let (loaded, warn) = load(&path, &Reversing);
        assert!(warn.is_none());
        assert_eq!(loaded, cfg);
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn migration_default_shaped_attaches_credentials_to_the_default_record() {
        // Blank URL and a spelled-out default URL both count as the default.
        for legacy_url in ["", "HTTPS://API.GAWK.IOIO.FI:4433/"] {
            let mut cfg = Config {
                relay_url: legacy_url.into(),
                publish_secret: "s3cret".into(),
                ..Default::default()
            };
            assert!(migrate(&mut cfg), "{legacy_url:?}");
            assert!(cfg.relay_url.is_empty(), "flat URL cleared");
            assert!(cfg.publish_secret.is_empty(), "flat secret cleared");
            assert_eq!(cfg.servers.len(), 1);
            let rec = &cfg.servers[0];
            assert_eq!(rec.name, DEFAULT_SERVER_NAME);
            assert_eq!(rec.publish_secret, "s3cret");
            assert_eq!(
                cfg.selected_server, DEFAULT_SERVER_NAME,
                "the default is selected, like the Linux Migrate"
            );
            // The migrated shape resolves like the flat one did.
            assert_eq!(cfg.resolve_relay_url(), defaults::RELAY_URL);
            assert_eq!(cfg.resolve_publish_secret(), "s3cret");
            // Idempotent: a second run changes nothing.
            let snapshot = cfg.clone();
            assert!(!migrate(&mut cfg));
            assert_eq!(cfg, snapshot);
        }
    }

    #[test]
    fn migration_custom_url_becomes_a_selected_profile() {
        let mut cfg = Config {
            relay_url: "https://relay.example:4433".into(),
            publish_secret: "s3cret".into(),
            ..Default::default()
        };
        assert!(migrate(&mut cfg));
        assert!(cfg.relay_url.is_empty());
        assert!(cfg.publish_secret.is_empty());
        assert_eq!(cfg.servers.len(), 1);
        let p = &cfg.servers[0];
        assert_eq!(p.name, "Migrated server");
        assert_eq!(p.url, "https://relay.example:4433");
        assert_eq!(p.publish_secret, "s3cret");
        assert_eq!(cfg.selected_server, "Migrated server");
        // The user who pointed their install at a custom relay keeps working
        // without noticing (§4.1.2).
        assert_eq!(cfg.resolve_relay_url(), "https://relay.example:4433");
        assert_eq!(cfg.resolve_publish_secret(), "s3cret");

        let snapshot = cfg.clone();
        assert!(!migrate(&mut cfg));
        assert_eq!(cfg, snapshot);
    }

    #[test]
    fn migration_with_nothing_legacy_is_a_no_op() {
        let mut cfg = Config {
            servers: vec![profile("Homelab", "https://relay.example:4433", "x")],
            selected_server: "Homelab".into(),
            ..Default::default()
        };
        let snapshot = cfg.clone();
        assert!(!migrate(&mut cfg));
        assert_eq!(cfg, snapshot);
    }

    // F9: the default's credential record is keyed to the URL it was saved
    // against; a release that moves the fleet discards it rather than
    // presenting the old relay's secret to the new host.
    #[test]
    fn a_default_credential_record_keyed_to_another_url_is_discarded() {
        let mut cfg = Config {
            servers: vec![profile(
                DEFAULT_SERVER_NAME,
                "https://old-fleet.example:4433",
                "stale",
            )],
            selected_server: DEFAULT_SERVER_NAME.into(),
            ..Default::default()
        };
        assert!(migrate(&mut cfg));
        assert!(cfg.servers.is_empty(), "stale record discarded");
        assert_eq!(cfg.resolve_publish_secret(), "");
    }

    #[test]
    fn selection_resolves_url_and_secret_and_unknown_falls_back_to_default() {
        let cfg = Config {
            servers: vec![
                profile("Homelab", "https://relay.example:4433", "custom-secret"),
                profile(DEFAULT_SERVER_NAME, defaults::RELAY_URL, "default-secret"),
            ],
            selected_server: "Homelab".into(),
            ..Default::default()
        };
        assert_eq!(cfg.resolve_relay_url(), "https://relay.example:4433");
        assert_eq!(cfg.resolve_publish_secret(), "custom-secret");

        // Default selected (blank name): the credential record's secret rides.
        let on_default = Config {
            selected_server: String::new(),
            ..cfg.clone()
        };
        assert_eq!(on_default.resolve_relay_url(), defaults::RELAY_URL);
        assert_eq!(on_default.resolve_publish_secret(), "default-secret");

        // Unknown selection degrades to the default, never a panic.
        let unknown = Config {
            selected_server: "gone".into(),
            ..cfg
        };
        assert_eq!(unknown.resolve_relay_url(), defaults::RELAY_URL);
        assert_eq!(unknown.resolve_publish_secret(), "default-secret");
    }

    #[test]
    fn added_profile_names_never_collide_and_default_secret_rotates() {
        let mut cfg = Config::default();
        assert_eq!(cfg.add_custom_server(), "New server");
        assert_eq!(cfg.add_custom_server(), "New server 2");
        assert!(cfg.profile_name_taken("New server"));
        assert!(
            cfg.profile_name_taken(DEFAULT_SERVER_NAME),
            "the reserved name is always taken"
        );

        // F4: the default's credential slot is editable — store, rotate,
        // clear — and the record it writes carries the F9 key.
        cfg.set_default_secret("first");
        assert_eq!(cfg.resolve_publish_secret(), "first");
        cfg.set_default_secret("second");
        assert_eq!(cfg.resolve_publish_secret(), "second");
        let rec = cfg
            .servers
            .iter()
            .find(|p| p.name == DEFAULT_SERVER_NAME)
            .unwrap();
        assert!(!rec.url.is_empty(), "keyed to the URL it was saved against");
        cfg.set_default_secret("");
        assert!(!cfg.servers.iter().any(|p| p.name == DEFAULT_SERVER_NAME));
        assert_eq!(cfg.resolve_publish_secret(), "");
    }

    // --- R37 phase E: 0x12 precedence + guard --------------------------------

    #[test]
    fn advertised_url_wins_over_the_configured_one() {
        // On a foreign relay with no explicit telemetry URL, the advertised
        // URL is the only way batches flow.
        assert_eq!(
            effective_telemetry_url(
                "https://relay.example:4433",
                "",
                Some("https://relay.example/api/telemetry/v1/ingest"),
            ),
            Some("https://relay.example/api/telemetry/v1/ingest".into())
        );
        // It also beats an explicitly configured URL (D15: the fleet that
        // gates collection and mints the token owns the destination).
        assert_eq!(
            effective_telemetry_url(
                "https://relay.example:4433",
                "https://configured.example/ingest",
                Some("https://relay.example/ingest"),
            ),
            Some("https://relay.example/ingest".into())
        );
        // And the default collector on the default relay.
        assert_eq!(
            effective_telemetry_url("", "", Some("https://relay.example/ingest")),
            Some("https://relay.example/ingest".into())
        );
    }

    #[test]
    fn off_beats_an_advertised_url() {
        // The advertised URL moves the destination; it must not override the
        // user's opt-out.
        assert_eq!(
            effective_telemetry_url("", "off", Some("https://relay.example/ingest")),
            None
        );
        assert_eq!(
            effective_telemetry_url(
                "https://relay.example:4433",
                "OFF",
                Some("https://relay.example/ingest"),
            ),
            None
        );
    }

    #[test]
    fn a_malformed_advertised_url_is_ignored_not_adopted() {
        for bad in ["http://relay.example/ingest", "not a url", "", "https://"] {
            // On the default relay the fallback still applies…
            assert_eq!(
                effective_telemetry_url("", "", Some(bad)),
                Some(defaults::TELEMETRY_URL.to_owned()),
                "{bad:?}"
            );
            // …and on a foreign relay the guard holds: nothing is sent.
            assert_eq!(
                effective_telemetry_url("https://relay.example:4433", "", Some(bad)),
                None,
                "{bad:?}"
            );
        }
    }

    // The §4.10 guard: a non-default relay whose fleet enabled collection
    // but advertised no URL gets NOTHING — those batches could only die at
    // the home deployment's token check.
    #[test]
    fn no_advertised_url_on_a_foreign_relay_reports_nothing() {
        assert_eq!(
            effective_telemetry_url("https://relay.example:4433", "", None),
            None
        );
        // An explicitly configured URL still works (the operator opted in).
        assert_eq!(
            effective_telemetry_url(
                "https://relay.example:4433",
                "https://configured.example/ingest",
                None,
            ),
            Some("https://configured.example/ingest".into())
        );
        // On the pinned default the guard never engages.
        assert_eq!(
            effective_telemetry_url("", "", None),
            Some(defaults::TELEMETRY_URL.to_owned())
        );
    }

    #[test]
    fn config_level_precedence_uses_the_selected_profile() {
        let cfg = Config {
            servers: vec![profile("Homelab", "https://relay.example:4433", "")],
            selected_server: "Homelab".into(),
            ..Default::default()
        };
        // Foreign relay, nothing advertised, nothing configured: the guard.
        assert_eq!(cfg.effective_telemetry_url(None), None);
        assert_eq!(cfg.resolve_telemetry_url(), None);
        // The advertised URL unlocks reporting for exactly this fleet.
        assert_eq!(
            cfg.effective_telemetry_url(Some("https://relay.example/ingest")),
            Some("https://relay.example/ingest".into())
        );
    }
}
