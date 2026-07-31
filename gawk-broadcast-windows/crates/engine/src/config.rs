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
}

impl Config {
    /// The relay URL to dial: blank means the default fleet.
    pub fn resolve_relay_url(&self) -> String {
        resolve_relay_url(&self.relay_url)
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
    /// pairing rule; `"off"` always wins).
    pub fn resolve_telemetry_url(&self) -> Option<String> {
        resolve_telemetry_url(&self.relay_url, &self.telemetry_url)
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

/// Saves atomically: temp file in the same directory, then rename — a crash
/// mid-write cannot corrupt the config. Credentials are wrapped on the way
/// out; resolved defaults are NEVER written (blank stays blank).
pub fn save(path: &Path, cfg: &Config, creds: &dyn Credentials) -> Result<(), String> {
    let mut stored = cfg.clone();
    stored.publish_secret = creds.wrap(&cfg.publish_secret);
    stored.last_resume_token = creds.wrap(&cfg.last_resume_token);
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

    #[test]
    fn credentials_are_wrapped_on_save_and_unwrapped_on_load() {
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
}
