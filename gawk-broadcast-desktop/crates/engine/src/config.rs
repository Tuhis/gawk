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

/// One room in "Your rooms" (docs/60 D8): the rooms this app joined, most
/// recent first, with the ones the user saved kept.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct RecentRoom {
    /// The code or static slug, as last joined.
    pub code: String,
    /// Starred: never dropped by the cap, listed first.
    pub saved: bool,
    /// Unix seconds of the last join.
    pub last_joined: u64,
    /// The static room's attach key, when it needed one — a credential
    /// (DPAPI-wrapped on Windows, like `roomAttachSecret`).
    pub attach_secret: String,
    /// The server the attach key was stored for, as [`server_key`] (R66,
    /// docs/68 D5a): the key is only ever presented to that server.
    pub server: String,
}

/// How many rooms "Your rooms" keeps. Saved rooms and the room just joined
/// are never dropped to make room, so the list can exceed this only by those.
pub const MAX_RECENT_ROOMS: usize = 8;

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
    /// The server `lastBroadcastId` and `lastResumeToken` were minted on, as
    /// [`server_key`]: a resume presents them there only (review of #452).
    pub last_broadcast_server: String,
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
    /// R42 rooms (docs/44 §4.8): the room code or static slug to join and
    /// attach to on every start (blank = no room). Same key as the Linux
    /// broadcaster's profile field.
    pub room: String,
    /// A static room's attach key — a credential (DPAPI-wrapped on Windows,
    /// like the publish secret).
    pub room_attach_secret: String,
    /// The room nickname (blank = the relay assigns one); it also names
    /// the tile. A pre-2026-09-14 profile's `roomLabel` key is ignored on
    /// load (serde default) and gone after the next save.
    pub nickname: String,
    /// The creator token (hex) of the room in `room`, when a pasted link
    /// carried one — a credential (DPAPI-wrapped on Windows, like the attach
    /// key). `room` itself only ever holds the code.
    pub room_creator_token: String,
    /// The server `roomAttachSecret` and `roomCreatorToken` were stored
    /// for, as [`server_key`] (R66, docs/68 D5a). They are presented to that
    /// server only; blank with no credentials.
    pub room_server: String,
    /// "Your rooms" (docs/60 D8), see [`Config::remember_room`].
    pub recent_rooms: Vec<RecentRoom>,
    /// Set while a broadcast is live, cleared when it ends inside the app:
    /// still set at launch means the app died live (docs/60 D12).
    pub was_live: bool,
    /// The last source shared (Windows, docs/60 D5): `display:<label>` or
    /// `window:<title>`. Blank = none yet.
    pub last_source: String,
    /// Linux (R56, docs/58 D10): the four keys the Go app's file carries.
    /// Read and written on every OS, so a shared file round-trips; only the
    /// Linux shell acts on them. `encoder` pins one cascade element
    /// (`vulkanh264enc`, `nvh264enc`, `vah264enc`), skipping the cascade but
    /// still trialling it (OD12).
    pub encoder: String,
    /// Pins a PulseAudio device name; skips the audio cascade AND the
    /// whose-audio step, with a visible note (docs/39 D3).
    pub audio_device: String,
    /// The binary last chosen in the whose-audio step, preselected next
    /// time (docs/39 D5).
    pub audio_app: String,
    /// The system-audio cascade's cached winner, re-verified before use
    /// (`pipewire-monitor`, `pulse-default-monitor`).
    pub last_good_audio_source: String,
    /// The launch-time update check (R45, docs/47 D6): zero value = on, the
    /// `disableAudio` convention. `GAWK_NO_UPDATE_CHECK=1` also turns it off.
    pub disable_update_check: bool,
    /// When GitHub last answered the check, RFC 3339 UTC; it gates the
    /// automatic check to once per 15 minutes. A check that got no answer
    /// leaves it.
    pub last_update_check: String,
    /// The newer release that answer named (blank = none), and its release
    /// page: a relaunch inside the 15 minutes shows the notice from these
    /// rather than asking GitHub again. Dismissing the notice is not stored —
    /// it lasts until the app restarts.
    pub update_version: String,
    pub update_url: String,
    /// The window's size as the user last set it by hand, in logical px
    /// (R64, docs/66 D10). 0 = never set: the window opens at its default.
    /// Heights that window fit chose are never stored here.
    pub window_width: u32,
    pub window_height: u32,
}

impl Config {
    /// Records a room join on `server` (a [`server_key`]) in "Your rooms":
    /// moves it to the front with the time, keeps its saved star, and stores
    /// the attach key when one was used. An empty key keeps the one on file
    /// when it was stored for the same server; one stored for another server
    /// is dropped, since it may never reach this one (docs/68 D5a). Codes
    /// compare case-insensitively, as the relay joins them. Beyond
    /// [`MAX_RECENT_ROOMS`], the oldest unsaved rooms go.
    pub fn remember_room(&mut self, server: &str, code: &str, attach_secret: &str, now: u64) {
        let code = code.trim();
        if code.is_empty() {
            return;
        }
        let mut entry = match self
            .recent_rooms
            .iter()
            .position(|r| r.code.eq_ignore_ascii_case(code))
        {
            Some(i) => self.recent_rooms.remove(i),
            None => RecentRoom::default(),
        };
        entry.code = code.to_owned();
        entry.last_joined = now;
        if !attach_secret.is_empty() {
            entry.attach_secret = attach_secret.to_owned();
        } else if entry.server != server {
            entry.attach_secret.clear();
        }
        entry.server = server.to_owned();
        self.recent_rooms.insert(0, entry);
        // The room just joined (index 0) is never the one evicted.
        while self.recent_rooms.len() > MAX_RECENT_ROOMS {
            match self.recent_rooms[1..].iter().rposition(|r| !r.saved) {
                Some(i) => {
                    self.recent_rooms.remove(i + 1);
                }
                None => break,
            }
        }
    }

    /// Stars or unstars a room in "Your rooms".
    pub fn set_room_saved(&mut self, code: &str, saved: bool) {
        if let Some(r) = self
            .recent_rooms
            .iter_mut()
            .find(|r| r.code.eq_ignore_ascii_case(code))
        {
            r.saved = saved;
        }
    }

    /// The attach key stored for a room in "Your rooms", if one was stored
    /// for `server` (a [`server_key`]); a key stored for another server is
    /// never returned (docs/68 D5a).
    pub fn room_attach_key(&self, server: &str, code: &str) -> Option<&str> {
        self.recent_rooms
            .iter()
            .find(|r| {
                r.code.eq_ignore_ascii_case(code)
                    && !r.attach_secret.is_empty()
                    && !server.is_empty()
                    && r.server == server
            })
            .map(|r| r.attach_secret.as_str())
    }

    /// The selected server as a [`server_key`]: what room credentials are
    /// stored against and checked against (docs/68 D5a).
    pub fn server_key(&self) -> String {
        server_key(&self.resolve_relay_url())
    }

    /// The pending room's attach key and creator token, but only when they
    /// were stored for the selected server; otherwise both blank, and the
    /// relay's own prompt asks (docs/68 D5a — the one place they leave).
    /// The broadcast ID and resume token a Resume reclaims with, but only
    /// when they were minted on the selected server; otherwise both blank
    /// (review of #452). A token presented to another relay would let its
    /// operator supersede the broadcast on its own server (close 4004).
    pub fn resume_identity(&self) -> (String, String) {
        let here = self.server_key();
        if here.is_empty() || self.last_broadcast_server != here {
            return (String::new(), String::new());
        }
        (
            self.last_broadcast_id.clone(),
            self.last_resume_token.clone(),
        )
    }

    pub fn pending_room_credentials(&self) -> (String, String) {
        let here = self.server_key();
        if here.is_empty() || self.room_server != here {
            return (String::new(), String::new());
        }
        (
            self.room_attach_secret.clone(),
            self.room_creator_token.clone(),
        )
    }

    /// The saved server a link's relay origin names (docs/68 D5): `None`
    /// for the default fleet (no origin, or the default's own), else
    /// `Some(Ok(name))` for a custom profile with that origin, and
    /// `Some(Err(()))` when no saved server has it.
    pub fn server_for_origin(&self, origin: Option<&str>) -> Option<Result<String, ()>> {
        let origin = origin?;
        if relay_origin(defaults::RELAY_URL).as_deref() == Some(origin) {
            return None;
        }
        Some(
            self.servers
                .iter()
                .find(|p| {
                    p.name != DEFAULT_SERVER_NAME && relay_origin(&p.url).as_deref() == Some(origin)
                })
                .map(|p| p.name.clone())
                .ok_or(()),
        )
    }

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

    /// The pinned default's own publish secret, whichever server is selected
    /// — what its Edit page shows (R62, docs/64 D15): the credentials-only
    /// record, or the legacy flat field until migration has run.
    pub fn default_secret(&self) -> String {
        match self.servers.iter().find(|p| p.name == DEFAULT_SERVER_NAME) {
            Some(rec) => rec.publish_secret.clone(),
            None => self.publish_secret.clone(),
        }
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

    /// Adds a profile for `url` with no secret, named `name` or, when that
    /// is taken, `name 2`, `name 3`… — the "Add and switch" of a link that
    /// names an unknown server (docs/68 D5). Returns the name it got; it
    /// does not select it.
    pub fn add_server(&mut self, name: &str, url: &str) -> String {
        let base = if name.trim().is_empty() {
            "Server"
        } else {
            name.trim()
        };
        let mut unique = base.to_owned();
        let mut i = 2;
        while self.profile_name_taken(&unique) {
            unique = format!("{base} {i}");
            i += 1;
        }
        self.servers.push(ServerProfile {
            name: unique.clone(),
            url: url.to_owned(),
            publish_secret: String::new(),
        });
        unique
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
            defaults::origin().to_owned()
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

/// A relay value as an https origin and nothing else — the docs/40 §4.2
/// rule, restating gawk-app's `normalizeRelayOrigin`: https only, no
/// credentials, path `/` or none, no query or fragment, reduced to
/// `https://host[:port]` with the host lowercased and the default port
/// elided. `None` for anything else.
pub fn relay_origin(raw: &str) -> Option<String> {
    let u = url::Url::parse(raw.trim()).ok()?;
    if u.scheme() != "https"
        || !u.username().is_empty()
        || u.password().is_some()
        || (u.path() != "/" && !u.path().is_empty())
        || u.query().is_some()
        || u.fragment().is_some()
        || u.host_str().is_none_or(str::is_empty)
    {
        return None;
    }
    Some(u.origin().ascii_serialization())
}

/// The key room credentials are bound to (docs/68 D5a): the relay URL's
/// origin when it is one, else its comparison form — a URL the strict rule
/// refuses still binds to exactly itself. Blank stays blank and matches
/// nothing.
pub fn server_key(relay_url: &str) -> String {
    if relay_url.trim().is_empty() {
        return String::new();
    }
    relay_origin(relay_url).unwrap_or_else(|| normalize_relay_url(relay_url))
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

/// The config file location: `%APPDATA%\gawk\broadcast.json` on Windows,
/// `~/Library/Application Support/gawk/broadcast.json` on macOS, and the Go
/// Linux app's own file on Linux — one filename everywhere (no collision is
/// possible cross-OS, and shared vocabulary keeps docs legible).
pub fn default_path() -> Option<PathBuf> {
    #[cfg(windows)]
    {
        std::env::var_os("APPDATA").map(|d| PathBuf::from(d).join("gawk").join("broadcast.json"))
    }
    #[cfg(target_os = "macos")]
    {
        // docs/54 D12.
        std::env::var_os("HOME").map(|d| {
            PathBuf::from(d)
                .join("Library")
                .join("Application Support")
                .join("gawk")
                .join("broadcast.json")
        })
    }
    #[cfg(all(not(windows), not(target_os = "macos")))]
    {
        // Linux (R56, docs/58 D10): the Go app's own file, found the way
        // Go's os.UserConfigDir finds it, so the two apps share one config
        // through the overlap window and a user's settings carry over.
        xdg_path(
            std::env::var_os("XDG_CONFIG_HOME"),
            std::env::var_os("HOME"),
        )
    }
}

/// `$XDG_CONFIG_HOME/gawk/broadcast.json`, else `$HOME/.config/…`. A
/// relative `XDG_CONFIG_HOME` is invalid per the XDG spec and is never
/// resolved against the working directory: Go's `os.UserConfigDir` errors on
/// it (the Go app then runs without a config); this falls back to `HOME`.
#[cfg_attr(any(windows, target_os = "macos"), allow(dead_code))]
fn xdg_path(
    xdg_config_home: Option<std::ffi::OsString>,
    home: Option<std::ffi::OsString>,
) -> Option<PathBuf> {
    let base = match xdg_config_home.map(PathBuf::from) {
        Some(dir) if dir.is_absolute() => dir,
        _ => PathBuf::from(home?).join(".config"),
    };
    Some(base.join("gawk").join("broadcast.json"))
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
            cfg.room_attach_secret = creds.unwrap(&cfg.room_attach_secret);
            cfg.room_creator_token = creds.unwrap(&cfg.room_creator_token);
            for r in &mut cfg.recent_rooms {
                r.attach_secret = creds.unwrap(&r.attach_secret);
            }
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
    let profiles = migrate_profiles(cfg);
    stamp_room_servers(cfg) || profiles
}

/// R66 (docs/68 D5a): room credentials stored before they were bound to a
/// server are stamped with the server selected at load — the assumption
/// the app made until then, and right for everyone on one server.
fn stamp_room_servers(cfg: &mut Config) -> bool {
    let here = cfg.server_key();
    let mut changed = false;
    // The resume identity the same way (review of #452).
    if cfg.last_broadcast_server.is_empty() && !cfg.last_broadcast_id.is_empty() {
        cfg.last_broadcast_server = here.clone();
        changed = true;
    }
    if cfg.room_server.is_empty()
        && (!cfg.room_attach_secret.is_empty() || !cfg.room_creator_token.is_empty())
    {
        cfg.room_server = here.clone();
        changed = true;
    }
    for r in &mut cfg.recent_rooms {
        if r.server.is_empty() {
            r.server = here.clone();
            changed = true;
        }
    }
    changed
}

/// The server-profile half of [`migrate`].
fn migrate_profiles(cfg: &mut Config) -> bool {
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
    stored.room_attach_secret = creds.wrap(&cfg.room_attach_secret);
    stored.room_creator_token = creds.wrap(&cfg.room_creator_token);
    for r in &mut stored.recent_rooms {
        r.attach_secret = creds.wrap(&r.attach_secret);
    }
    for p in &mut stored.servers {
        p.publish_secret = creds.wrap(&p.publish_secret);
    }
    let json = serde_json::to_vec_pretty(&stored).map_err(|e| e.to_string())?;

    let dir = path.parent().ok_or("config path has no parent directory")?;
    std::fs::create_dir_all(dir).map_err(|e| e.to_string())?;
    let tmp = path.with_extension("json.tmp");
    write_owner_only(&tmp, &json).map_err(|e| e.to_string())?;
    std::fs::rename(&tmp, path).map_err(|e| e.to_string())?;
    Ok(())
}

/// Writes `bytes` to `path`, replacing it. On Unix the file is mode 0600
/// (docs/54 D12, the Linux rule): the credentials in it are plaintext
/// there, so nobody but the owner may read it. Windows protects them with
/// DPAPI instead and keeps the default ACL.
fn write_owner_only(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    #[cfg(unix)]
    {
        use std::io::Write;
        use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
        let mut f = std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(path)?;
        // `mode` only applies on creation; a tmp left by a crashed save
        // keeps its old bits unless they are set explicitly.
        f.set_permissions(std::fs::Permissions::from_mode(0o600))?;
        f.write_all(bytes)
    }
    #[cfg(not(unix))]
    {
        std::fs::write(path, bytes)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The default fleet's server key, which room credentials bind to.
    const S: &str = "https://api.gawk.ioio.fi:4433";

    // --- R66 (docs/68 D5, D5a) ---------------------------------------------

    #[test]
    fn relay_origin_is_the_web_rule() {
        for (raw, want) in [
            (
                "https://Relay.Example.com:4433",
                Some("https://relay.example.com:4433"),
            ),
            (
                "https://relay.example.com:4433/",
                Some("https://relay.example.com:4433"),
            ),
            (
                "https://relay.example.com:443",
                Some("https://relay.example.com"),
            ),
            (
                "  https://relay.example.com  ",
                Some("https://relay.example.com"),
            ),
            ("http://relay.example.com", None),
            ("https://u:p@relay.example.com", None),
            ("https://u@relay.example.com", None),
            ("https://relay.example.com/path", None),
            ("https://relay.example.com/?q=1", None),
            ("https://relay.example.com/#f", None),
            ("relay.example.com", None),
            ("", None),
        ] {
            assert_eq!(relay_origin(raw).as_deref(), want, "{raw}");
        }
        assert_eq!(server_key(""), "");
        assert_eq!(server_key("https://R.example/"), "https://r.example");
        // Refused by the strict rule, it still binds to exactly itself.
        assert_eq!(server_key("https://r.example/p"), "https://r.example/p");
    }

    #[test]
    fn a_room_key_is_returned_only_for_the_server_it_was_stored_for() {
        let mut cfg = Config::default();
        cfg.remember_room(S, "lan-party", "k3y", 1);
        assert_eq!(cfg.room_attach_key(S, "LAN-PARTY"), Some("k3y"));
        assert_eq!(
            cfg.room_attach_key("https://evil.example", "lan-party"),
            None
        );
        assert_eq!(cfg.room_attach_key("", "lan-party"), None);

        // Joined again on another server with no key: the old key, which
        // belongs to the first server, goes.
        cfg.remember_room("https://evil.example", "lan-party", "", 2);
        assert_eq!(cfg.recent_rooms.len(), 1);
        assert_eq!(cfg.room_attach_key(S, "lan-party"), None);
        assert_eq!(
            cfg.room_attach_key("https://evil.example", "lan-party"),
            None
        );
        assert_eq!(cfg.recent_rooms[0].attach_secret, "");

        // The same server with no key keeps the key on file.
        cfg.remember_room("https://evil.example", "lan-party", "other", 3);
        cfg.remember_room("https://evil.example", "lan-party", "", 4);
        assert_eq!(
            cfg.room_attach_key("https://evil.example", "lan-party"),
            Some("other")
        );
    }

    #[test]
    fn pending_room_credentials_follow_the_selected_server() {
        let mut cfg = Config {
            room: "lan-party".into(),
            room_attach_secret: "k3y".into(),
            room_creator_token: "c0ffee".into(),
            room_server: S.into(),
            servers: vec![ServerProfile {
                name: "Friend".into(),
                url: "https://relay.friend.example".into(),
                publish_secret: String::new(),
            }],
            ..Default::default()
        };
        assert_eq!(
            cfg.pending_room_credentials(),
            ("k3y".to_string(), "c0ffee".to_string())
        );
        cfg.selected_server = "Friend".into();
        assert_eq!(
            cfg.pending_room_credentials(),
            (String::new(), String::new())
        );
        cfg.selected_server = DEFAULT_SERVER_NAME.into();
        cfg.room_server.clear();
        assert_eq!(
            cfg.pending_room_credentials(),
            (String::new(), String::new()),
            "unbound credentials go nowhere"
        );
    }

    #[test]
    fn a_legacy_config_stamps_room_credentials_with_the_selected_server() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-d5a-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("broadcast.json");
        std::fs::write(
            &path,
            br#"{"room": "lan-party", "roomAttachSecret": "k3y",
                 "servers": [{"name": "Friend", "url": "https://Relay.Friend.example:4433/"}],
                 "selectedServer": "Friend",
                 "recentRooms": [{"code": "lan-party", "attachSecret": "k3y", "lastJoined": 5}]}"#,
        )
        .unwrap();
        let (mut cfg, warn) = load(&path, &Plaintext);
        assert!(warn.is_none());
        assert!(cfg.recent_rooms[0].server.is_empty());
        assert!(migrate(&mut cfg), "stamping is a change to save");
        let friend = "https://relay.friend.example:4433";
        assert_eq!(cfg.room_server, friend);
        assert_eq!(cfg.recent_rooms[0].server, friend);
        assert_eq!(cfg.room_attach_key(friend, "lan-party"), Some("k3y"));
        assert_eq!(cfg.room_attach_key(S, "lan-party"), None);
        // Idempotent.
        assert!(!migrate(&mut cfg));

        save(&path, &cfg, &Plaintext).unwrap();
        let raw = std::fs::read_to_string(&path).unwrap();
        assert!(
            raw.contains("\"roomServer\": \"https://relay.friend.example:4433\""),
            "{raw}"
        );
        assert!(
            raw.contains("\"server\": \"https://relay.friend.example:4433\""),
            "{raw}"
        );
        std::fs::remove_dir_all(&dir).ok();
    }

    // Review of #452: the reclaim identity (ID and resume token) goes only
    // to the server it was minted on. A token presented elsewhere lets that
    // operator supersede the broadcast on its own server.
    #[test]
    fn the_resume_identity_goes_only_to_the_server_it_was_minted_on() {
        let mut cfg = Config {
            last_broadcast_id: "K7XQ2M".into(),
            last_resume_token: "aa11".into(),
            last_broadcast_server: S.into(),
            ..friend_cfg()
        };
        assert_eq!(
            cfg.resume_identity(),
            ("K7XQ2M".to_string(), "aa11".to_string())
        );
        cfg.selected_server = "Friend".into();
        assert_eq!(cfg.resume_identity(), (String::new(), String::new()));
        cfg.selected_server.clear();
        cfg.last_broadcast_server.clear();
        assert_eq!(
            cfg.resume_identity(),
            (String::new(), String::new()),
            "an unbound identity goes nowhere"
        );
    }

    #[test]
    fn a_legacy_resume_identity_is_stamped_with_the_selected_server() {
        let mut cfg = Config {
            last_broadcast_id: "K7XQ2M".into(),
            last_resume_token: "aa11".into(),
            selected_server: "Friend".into(),
            ..friend_cfg()
        };
        assert!(migrate(&mut cfg));
        assert_eq!(cfg.last_broadcast_server, "https://relay.friend.example");
        assert_eq!(cfg.resume_identity().0, "K7XQ2M");
        assert!(!migrate(&mut cfg));
        // Nothing to bind, nothing stamped.
        let mut fresh = Config::default();
        assert!(!migrate(&mut fresh));
        assert!(fresh.last_broadcast_server.is_empty());
    }

    fn friend_cfg() -> Config {
        Config {
            servers: vec![ServerProfile {
                name: "Friend".into(),
                url: "https://relay.friend.example".into(),
                publish_secret: String::new(),
            }],
            ..Default::default()
        }
    }

    #[test]
    fn a_pending_room_without_credentials_is_not_stamped() {
        let mut cfg = Config {
            room: "lan-party".into(),
            ..Default::default()
        };
        assert!(!migrate(&mut cfg));
        assert!(cfg.room_server.is_empty());
    }

    #[test]
    fn a_link_origin_names_the_default_a_saved_server_or_none() {
        let mut cfg = Config::default();
        cfg.servers.push(ServerProfile {
            name: "Friend".into(),
            url: "https://Relay.Friend.example:4433/".into(),
            publish_secret: "s".into(),
        });
        // The default's credentials-only record is never a match.
        cfg.set_default_secret("d");
        assert_eq!(cfg.server_for_origin(None), None);
        assert_eq!(cfg.server_for_origin(Some(S)), None);
        assert_eq!(
            cfg.server_for_origin(Some("https://relay.friend.example:4433")),
            Some(Ok("Friend".to_string()))
        );
        assert_eq!(
            cfg.server_for_origin(Some("https://relay.friend.example")),
            Some(Err(()))
        );
    }

    #[test]
    fn add_server_names_it_uniquely_and_does_not_select_it() {
        let mut cfg = Config::default();
        let a = cfg.add_server("relay.friend.example", "https://relay.friend.example");
        let b = cfg.add_server("relay.friend.example", "https://relay.friend.example:4433");
        let c = cfg.add_server("default", "https://x.example");
        assert_eq!(a, "relay.friend.example");
        assert_eq!(b, "relay.friend.example 2");
        assert_eq!(c, "default 2");
        assert_eq!(cfg.servers[1].url, "https://relay.friend.example:4433");
        assert!(cfg.servers.iter().all(|p| p.publish_secret.is_empty()));
        assert!(cfg.selected_server.is_empty());
    }

    /// docs/54 D12 (the Linux rule, unchanged): on Unix the credentials sit
    /// in the file as plaintext, so the file is the owner's alone.
    #[cfg(unix)]
    #[test]
    fn saved_config_is_owner_only_on_unix() {
        use std::os::unix::fs::PermissionsExt;
        let dir = std::env::temp_dir().join(format!("gawk-cfg-mode-{}", std::process::id()));
        let path = dir.join("broadcast.json");
        let cfg = Config {
            last_resume_token: "aa11".into(),
            ..Config::default()
        };
        save(&path, &cfg, &Plaintext).unwrap();
        let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "mode {mode:o}");
        // A rewrite keeps it (the atomic rename replaces the file).
        save(&path, &cfg, &Plaintext).unwrap();
        let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "mode {mode:o} after a rewrite");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// docs/54 D12: where macOS keeps it.
    #[cfg(target_os = "macos")]
    #[test]
    fn the_macos_config_lives_in_application_support() {
        let p = default_path().expect("HOME is set");
        assert!(
            p.ends_with("Library/Application Support/gawk/broadcast.json"),
            "{}",
            p.display()
        );
    }

    #[test]
    fn blank_means_the_default_resolved_at_use() {
        let cfg = Config::default();
        assert_eq!(cfg.resolve_relay_url(), defaults::RELAY_URL);
        assert_eq!(cfg.resolve_app_url(), defaults::APP_URL);
        assert_eq!(cfg.resolve_origin(), defaults::origin());
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

    // --- R42 rooms (docs/44 §4.8) --------------------------------------------

    #[test]
    fn room_fields_round_trip_with_the_attach_key_wrapped() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-room-{}", std::process::id()));
        let path = dir.join("broadcast.json");
        let cfg = Config {
            room: "lan-party".into(),
            room_attach_secret: "k3y".into(),
            nickname: "Juho".into(),
            ..Default::default()
        };
        save(&path, &cfg, &Reversing).unwrap();
        let raw = std::fs::read_to_string(&path).unwrap();
        // The Linux profile's key names; the attach key is a credential.
        assert!(raw.contains("\"room\": \"lan-party\""), "{raw}");
        assert!(
            raw.contains("\"roomAttachSecret\": \"wrapped:k3y\""),
            "{raw}"
        );
        assert!(raw.contains("\"nickname\": \"Juho\""), "{raw}");
        // The nickname names the tile (2026-09-14): no separate label key.
        assert!(!raw.contains("roomLabel"), "{raw}");
        assert!(
            !raw.contains("\"k3y\""),
            "attach key on disk in the clear: {raw}"
        );

        let (loaded, warn) = load(&path, &Reversing);
        assert!(warn.is_none());
        assert_eq!(loaded, cfg);
        // Migration leaves the room fields alone.
        let mut migrated = loaded.clone();
        migrate(&mut migrated);
        assert_eq!(migrated.room, "lan-party");
        assert_eq!(migrated.room_attach_secret, "k3y");
        std::fs::remove_dir_all(&dir).ok();
    }

    // docs/60 D8, D12, D5: the redesign's keys round-trip, and a room's
    // attach key is a credential like roomAttachSecret.
    #[test]
    fn redesign_keys_round_trip_with_room_keys_wrapped() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-recent-{}", std::process::id()));
        let path = dir.join("broadcast.json");
        let mut cfg = Config {
            was_live: true,
            last_source: "display:Display 1".into(),
            ..Default::default()
        };
        cfg.remember_room(S, "lan-party", "k3y", 100);
        cfg.remember_room(S, "K7XQ2M", "", 200);
        cfg.set_room_saved("lan-party", true);
        save(&path, &cfg, &Reversing).unwrap();
        let raw = std::fs::read_to_string(&path).unwrap();
        assert!(raw.contains("\"recentRooms\""), "{raw}");
        assert!(raw.contains("\"attachSecret\": \"wrapped:k3y\""), "{raw}");
        assert!(!raw.contains("\"k3y\""), "attach key in the clear: {raw}");
        assert!(raw.contains("\"wasLive\": true"), "{raw}");
        assert!(
            raw.contains("\"lastSource\": \"display:Display 1\""),
            "{raw}"
        );
        let (loaded, warn) = load(&path, &Reversing);
        assert!(warn.is_none());
        assert_eq!(loaded, cfg);
        assert_eq!(loaded.room_attach_key(S, "LAN-PARTY"), Some("k3y"));
        std::fs::remove_dir_all(&dir).ok();
    }

    // docs/47 D6: the update check's keys round-trip under their camelCase
    // names, and a file without them means "check on, never checked,
    // nothing found".
    #[test]
    fn update_check_keys_round_trip_and_default_to_on() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-update-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("broadcast.json");
        std::fs::write(&path, br#"{"nickname": "Juho"}"#).unwrap();
        let (old, warn) = load(&path, &Reversing);
        assert!(warn.is_none());
        assert!(!old.disable_update_check);
        assert!(old.last_update_check.is_empty());
        assert!(old.update_version.is_empty());
        assert!(old.update_url.is_empty());

        let cfg = Config {
            disable_update_check: true,
            last_update_check: "2026-09-30T12:00:00Z".into(),
            update_version: "2.1.0".into(),
            update_url: "https://github.com/Tuhis/gawk/releases/tag/gawk-broadcast-desktop/v2.1.0"
                .into(),
            ..Default::default()
        };
        save(&path, &cfg, &Reversing).unwrap();
        let raw = std::fs::read_to_string(&path).unwrap();
        assert!(raw.contains("\"disableUpdateCheck\": true"), "{raw}");
        assert!(
            raw.contains("\"lastUpdateCheck\": \"2026-09-30T12:00:00Z\""),
            "{raw}"
        );
        assert!(raw.contains("\"updateVersion\": \"2.1.0\""), "{raw}");
        assert!(
            raw.contains("\"updateUrl\": \"https://github.com/"),
            "{raw}"
        );
        let (loaded, _) = load(&path, &Reversing);
        assert_eq!(loaded, cfg);
        std::fs::remove_dir_all(&dir).ok();
    }

    // docs/66 D10: the user's window size round-trips under its camelCase
    // names, and a file without it means "never set" (0), not 0 × 0.
    #[test]
    fn window_size_keys_round_trip_and_default_to_unset() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-window-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("broadcast.json");
        std::fs::write(&path, br#"{"nickname": "Juho"}"#).unwrap();
        let (old, warn) = load(&path, &Plaintext);
        assert!(warn.is_none());
        assert_eq!((old.window_width, old.window_height), (0, 0));

        let cfg = Config {
            window_width: 560,
            window_height: 720,
            ..Default::default()
        };
        save(&path, &cfg, &Plaintext).unwrap();
        let raw = std::fs::read_to_string(&path).unwrap();
        assert!(raw.contains("\"windowWidth\": 560"), "{raw}");
        assert!(raw.contains("\"windowHeight\": 720"), "{raw}");
        let (loaded, _) = load(&path, &Plaintext);
        assert_eq!(loaded, cfg);
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn a_file_without_the_redesign_keys_loads_with_defaults() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-norecent-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("broadcast.json");
        std::fs::write(&path, br#"{"room": "lan-party", "nickname": "Juho"}"#).unwrap();
        let (loaded, warn) = load(&path, &Plaintext);
        assert!(warn.is_none());
        assert!(loaded.recent_rooms.is_empty());
        assert!(!loaded.was_live);
        assert_eq!(loaded.last_source, "");
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn recent_rooms_move_to_the_front_and_cap_without_dropping_saved_ones() {
        let mut cfg = Config::default();
        cfg.remember_room(S, "saved-one", "k", 1);
        cfg.set_room_saved("saved-one", true);
        for i in 0..10u64 {
            cfg.remember_room(S, &format!("room-{i}"), "", 10 + i);
        }
        assert_eq!(cfg.recent_rooms.len(), MAX_RECENT_ROOMS);
        assert_eq!(cfg.recent_rooms[0].code, "room-9");
        assert!(
            cfg.recent_rooms.iter().any(|r| r.code == "saved-one"),
            "the cap never drops a saved room"
        );
        assert!(!cfg.recent_rooms.iter().any(|r| r.code == "room-0"));

        // A re-join (any case) moves it to the front, keeps its star and
        // its key when no new key is given.
        cfg.remember_room(S, "SAVED-ONE", "", 99);
        let front = &cfg.recent_rooms[0];
        assert_eq!(
            (front.code.as_str(), front.saved, front.last_joined),
            ("SAVED-ONE", true, 99)
        );
        assert_eq!(front.attach_secret, "k");
        assert_eq!(
            cfg.recent_rooms
                .iter()
                .filter(|r| r.code.eq_ignore_ascii_case("saved-one"))
                .count(),
            1
        );
        // A blank code is not a room.
        let before = cfg.recent_rooms.clone();
        cfg.remember_room(S, "  ", "", 1);
        assert_eq!(cfg.recent_rooms, before);
    }

    // Review of #381: with every other room saved, the cap evicted the room
    // just joined (and the key typed for it), since it was the only unsaved
    // entry.
    #[test]
    fn the_cap_never_evicts_the_room_just_joined() {
        let mut cfg = Config::default();
        for i in 0..MAX_RECENT_ROOMS as u64 {
            let code = format!("saved-{i}");
            cfg.remember_room(S, &code, "", i);
            cfg.set_room_saved(&code, true);
        }
        cfg.remember_room(S, "new", "k", 100);
        assert_eq!(cfg.recent_rooms[0].code, "new");
        assert_eq!(cfg.room_attach_key(S, "new"), Some("k"));
        assert_eq!(
            cfg.recent_rooms.iter().filter(|r| r.saved).count(),
            MAX_RECENT_ROOMS,
            "no saved room dropped either"
        );
        // The next join may evict the older unsaved one, never the new one.
        cfg.remember_room(S, "newer", "", 101);
        assert_eq!(cfg.recent_rooms[0].code, "newer");
        assert!(!cfg.recent_rooms.iter().any(|r| r.code == "new"));
    }

    #[test]
    fn a_profile_with_the_old_room_label_key_loads_and_drops_it() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-oldlabel-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("broadcast.json");
        std::fs::write(
            &path,
            br#"{"room": "lan-party", "roomLabel": "Juho's PC", "nickname": "Juho"}"#,
        )
        .unwrap();
        let (cfg, warn) = load(&path, &Plaintext);
        assert!(warn.is_none());
        assert_eq!(
            (cfg.room.as_str(), cfg.nickname.as_str()),
            ("lan-party", "Juho")
        );
        save(&path, &cfg, &Plaintext).unwrap();
        let raw = std::fs::read_to_string(&path).unwrap();
        assert!(!raw.contains("roomLabel"), "{raw}");
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn a_pre_rooms_config_loads_with_no_room() {
        let dir = std::env::temp_dir().join(format!("gawk-cfg-noroom-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("broadcast.json");
        std::fs::write(&path, br#"{"lastBroadcastId": "K7XQ2M"}"#).unwrap();
        let (cfg, warn) = load(&path, &Plaintext);
        assert!(warn.is_none());
        assert_eq!(cfg.last_broadcast_id, "K7XQ2M");
        assert!(cfg.room.is_empty() && cfg.room_attach_secret.is_empty());
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

    // R62 (docs/64 D15): Edit on the default's row shows the DEFAULT's
    // secret even while a custom server is selected — never the selected
    // server's, which is what `resolve_publish_secret` answers.
    #[test]
    fn the_default_secret_is_the_defaults_whichever_server_is_selected() {
        let mut cfg = Config::default();
        assert_eq!(cfg.default_secret(), "");
        cfg.set_default_secret("official");
        let name = cfg.add_custom_server();
        cfg.servers
            .iter_mut()
            .find(|p| p.name == name)
            .unwrap()
            .publish_secret = "homelab".into();
        cfg.selected_server = name;
        assert_eq!(cfg.resolve_publish_secret(), "homelab");
        assert_eq!(cfg.default_secret(), "official");

        // Before migration the flat field is the default's.
        let legacy = Config {
            publish_secret: "flat".into(),
            ..Config::default()
        };
        assert_eq!(legacy.default_secret(), "flat");
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

    /// docs/58 D10: the Linux path honours XDG_CONFIG_HOME, as the Go app
    /// always did, and falls back to ~/.config.
    #[test]
    fn the_linux_path_is_the_go_apps_file() {
        use std::ffi::OsString;
        let home = Some(OsString::from("/home/u"));
        assert_eq!(
            xdg_path(Some("/x/cfg".into()), home.clone()),
            Some(PathBuf::from("/x/cfg/gawk/broadcast.json"))
        );
        assert_eq!(
            xdg_path(None, home.clone()),
            Some(PathBuf::from("/home/u/.config/gawk/broadcast.json"))
        );
        // Empty and relative values are not a directory to write into.
        assert_eq!(
            xdg_path(Some("".into()), home.clone()),
            Some(PathBuf::from("/home/u/.config/gawk/broadcast.json"))
        );
        assert_eq!(
            xdg_path(Some("rel/dir".into()), home),
            Some(PathBuf::from("/home/u/.config/gawk/broadcast.json"))
        );
        assert_eq!(xdg_path(None, None), None);
    }

    /// docs/58 D10's carry-over test: a file the Go app wrote
    /// (tests/fixtures/go-broadcast.json, produced by the Go
    /// `config.Config.Save` with every key populated) loads here with the
    /// same effective relay, secret per server, room, nickname, resume id
    /// and token, rung, encoder cache and audio preselection.
    #[test]
    fn a_go_written_config_loads_with_identical_effective_values() {
        let dir = std::env::temp_dir().join(format!("gawk-go-cfg-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("broadcast.json");
        std::fs::write(&path, include_str!("../tests/fixtures/go-broadcast.json")).unwrap();

        let (mut cfg, warn) = load(&path, &Plaintext);
        assert!(warn.is_none(), "{warn:?}");
        // Already migrated in Go (servers present): the only change is
        // binding the room's attach key to the selected server (docs/68 D5a).
        let before = cfg.clone();
        assert!(migrate(&mut cfg));
        assert_eq!(cfg.servers, before.servers);
        assert_eq!(cfg.selected_server, before.selected_server);
        assert_eq!(cfg.room_server, "https://relay.home.example:4433");
        assert!(!migrate(&mut cfg));

        assert_eq!(cfg.resolve_relay_url(), "https://relay.home.example:4433");
        assert_eq!(cfg.resolve_publish_secret(), "homelab-secret");
        assert_eq!(cfg.resolve_app_url(), "https://gawk.example.org");
        assert_eq!(cfg.resolve_telemetry_url(), None, "\"off\" carries over");
        // The default's credentials-only record survives F9 (same fleet).
        let mut on_default = cfg.clone();
        on_default.selected_server = DEFAULT_SERVER_NAME.into();
        assert_eq!(on_default.resolve_relay_url(), defaults::RELAY_URL);
        assert_eq!(on_default.resolve_publish_secret(), "default-secret");

        assert_eq!(cfg.room, "FRIDAY");
        assert_eq!(cfg.room_attach_secret, "attach-key");
        assert_eq!(cfg.nickname, "tuhis");
        assert_eq!(cfg.last_broadcast_id, "K7XQ2M");
        assert_eq!(cfg.last_resume_token, "00112233445566778899aabbccddeeff");
        // A saved bitrate is kept (D5: only the UNSET default drops to 12).
        assert_eq!(cfg.resolve_rung(), (2560, 1440, 120, 16_000_000));
        assert_eq!(cfg.last_good_encoder, "nvh264enc");
        assert_eq!(cfg.encoder, "vah264enc");
        assert_eq!(cfg.last_good_audio_source, "pipewire-monitor");
        assert_eq!(
            cfg.audio_device,
            "alsa_output.usb-headset.analog-stereo.monitor"
        );
        assert_eq!(cfg.audio_app, "hl2_linux");
        assert!(!cfg.disable_audio);

        // And it round-trips: the Linux keys survive a save by this app.
        save(&path, &cfg, &Plaintext).unwrap();
        let (again, _) = load(&path, &Plaintext);
        assert_eq!(again, cfg);
        let raw = std::fs::read_to_string(&path).unwrap();
        for key in ["encoder", "audioDevice", "audioApp", "lastGoodAudioSource"] {
            assert!(raw.contains(&format!("\"{key}\"")), "{key} missing: {raw}");
        }
        std::fs::remove_dir_all(&dir).ok();
    }
}
