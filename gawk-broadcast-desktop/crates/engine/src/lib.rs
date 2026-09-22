//! Session lifecycle, send policy, resume supervisor, timesync, stats and
//! telemetry batching — the port of `gawk-broadcast/internal/engine`'s
//! semantics (docs/38 D5): same idea, same names, third language. Anything
//! the relay or viewer can observe behaves identically to the Go engine.
//!
//! This crate deliberately has no GUI and no COM/WinRT dependencies: media
//! enters through the types in [`media`], the network through the
//! [`relay::RelaySession`] seam — "the send policy is defined by what
//! happens when sends fail", and only a seam lets tests script failures.
//! That is also what keeps a future CLI or pubsim-style shell possible
//! without redesign (docs/38 OD12).

pub mod clock;
pub mod config;
pub mod dispatch;
pub mod gate;
pub mod media;
pub mod relay;
pub mod resume;
pub mod room;
pub mod sender;
pub mod session;
pub mod stats;
pub mod telemetry;
pub mod timesync;
pub mod transport;
pub mod uplink;

/// Shipped defaults, all pointing at the production gawk deployment
/// (docs/38 D13). "Blank means the default, resolved at use, never at save"
/// — the config file never stores a resolved default, so a release that
/// moves the fleet address moves every user with it.
pub mod defaults {
    /// The production relay origin (WebTransport publish endpoint).
    pub const RELAY_URL: &str = "https://api.gawk.ioio.fi:4433";
    /// The production frontend origin; join links are `<APP_URL>/#/view/<ID>`.
    /// Unlike the Linux broadcaster this ships as a real default: join links
    /// must work out of the box (docs/38 G8).
    pub const APP_URL: &str = "https://gawk.ioio.fi";
    /// The reference telemetry ingest — on the frontend origin, NOT the
    /// relay. `off` disables reporting; the pairing rule (default collector
    /// only with the default relay) is enforced where the URL is resolved.
    pub const TELEMETRY_URL: &str = "https://gawk.ioio.fi/api/telemetry/v1/ingest";
    /// One downloadable build of this workspace (docs/54 D1, D14): the
    /// workspace releases as one component, but a user downloads — and the
    /// relay, the release job and the telemetry dashboard each see — one of
    /// these.
    #[derive(Debug, PartialEq, Eq)]
    pub struct Distribution {
        /// Telemetry `browser` and diagnostics `kind` (docs/38 D15), and the
        /// R46 manifest's `component`: `releases/<name>/latest.json`.
        pub name: &'static str,
        /// The self-identifying Origin header (docs/38 D19, docs/54 D16). A
        /// native client must send one, or an allowlisting relay matches it
        /// against nothing; the production relay's allowlist must include it.
        pub origin: &'static str,
        /// The fixed release asset name (docs/47 D5's per-platform table).
        pub asset: &'static str,
        /// Telemetry `os`.
        pub os: &'static str,
    }

    pub const WINDOWS: Distribution = Distribution {
        name: "gawk-broadcast-windows",
        origin: "gawk-broadcast://windows",
        asset: "gawk-broadcast-windows-x86_64.exe",
        os: "Windows",
    };

    pub const MACOS: Distribution = Distribution {
        name: "gawk-broadcast-macos",
        origin: "gawk-broadcast://macos",
        asset: "gawk-broadcast-macos-arm64.zip",
        os: "macOS",
    };

    /// The distribution this build is: macOS on macOS, Windows everywhere
    /// else. Not `cfg(windows)`, because the Windows shell is linted and
    /// tested on Linux hosts too (docs/38 D18) and must keep its identity
    /// there.
    pub const THIS: &Distribution = if cfg!(target_os = "macos") {
        &MACOS
    } else {
        &WINDOWS
    };

    /// This build's Origin header — see [`Distribution::origin`].
    pub const ORIGIN: &str = THIS.origin;

    /// The fixed rung (docs/38 D11): 1080p60, 500 ms GOP, 12 Mbps peak
    /// (peak-constrained VBR; typical motion averages ~75 % of it).
    /// Lowered from 16 after the first field broadcast (F-12): 16 Mbps of
    /// datagrams left home uplinks no headroom for the keyframe stream.
    pub const WIDTH: u32 = 1920;
    pub const HEIGHT: u32 = 1080;
    pub const FPS: u32 = 60;
    pub const BITRATE_BPS: u32 = 12_000_000;
    pub const GOP_MS: u32 = 500;
}

/// Builds the join link for a broadcast code the way both Linux shells do:
/// `<app-url>/#/view/<ID>` with a trailing slash trimmed first.
pub fn join_link(app_url: &str, broadcast_id: &str) -> String {
    format!("{}/#/view/{}", app_url.trim_end_matches('/'), broadcast_id)
}

/// Builds the room view link the "Open room view" button launches (docs/44
/// §4.8): `<app-url>/#/room/<CODE>?rt=<grant>`, where the grant is the
/// creator token (hex) of a room this session minted or a static room's
/// attach key — the SPA moves it into session storage and rewrites the hash
/// before rendering, the same one-shot pattern as `?relay=`. No grant ⇒ a
/// plain participant link.
pub fn room_link(app_url: &str, code: &str, grant: &str) -> String {
    let base = format!("{}/#/room/{}", app_url.trim_end_matches('/'), code);
    if grant.is_empty() {
        return base;
    }
    let grant: String = url::form_urlencoded::byte_serialize(grant.as_bytes()).collect();
    format!("{base}?rt={grant}")
}

/// Reads a room code out of what a user pasted into the Room card: a bare
/// code or slug, or a room link (`…/#/room/CODE?rt=…`). Whitespace is
/// trimmed; case is left to the relay (codes join case-insensitively).
/// `None` for an empty or unusable input.
pub fn parse_room_code(input: &str) -> Option<String> {
    let s = input.trim();
    if s.is_empty() {
        return None;
    }
    let code = match s.find("#/room/") {
        Some(i) => &s[i + "#/room/".len()..],
        None => s,
    };
    let code = code.split(['?', '/', '#']).next().unwrap_or("").trim();
    if code.is_empty()
        || code.len() > gawk_wire::MAX_ROOM_CODE_LEN
        || !code.chars().all(|c| c.is_ascii_alphanumeric() || c == '-')
    {
        return None;
    }
    Some(code.to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn room_link_carries_the_grant_as_rt() {
        assert_eq!(
            room_link("https://gawk.ioio.fi/", "K7XQ2M", ""),
            "https://gawk.ioio.fi/#/room/K7XQ2M"
        );
        assert_eq!(
            room_link("https://gawk.ioio.fi", "K7XQ2M", "deadbeef"),
            "https://gawk.ioio.fi/#/room/K7XQ2M?rt=deadbeef"
        );
        // A static room's attach key can be anything; it is form-encoded.
        assert_eq!(
            room_link("https://gawk.ioio.fi", "lan-party", "k 3&y"),
            "https://gawk.ioio.fi/#/room/lan-party?rt=k+3%26y"
        );
    }

    #[test]
    fn room_code_parses_from_a_code_or_a_link() {
        assert_eq!(parse_room_code(" k7xq2m ").as_deref(), Some("k7xq2m"));
        assert_eq!(parse_room_code("lan-party").as_deref(), Some("lan-party"));
        assert_eq!(
            parse_room_code("https://gawk.ioio.fi/#/room/K7XQ2M?rt=deadbeef").as_deref(),
            Some("K7XQ2M")
        );
        assert_eq!(
            parse_room_code("https://gawk.ioio.fi/#/room/lan-party").as_deref(),
            Some("lan-party")
        );
        assert_eq!(parse_room_code(""), None);
        assert_eq!(parse_room_code("https://gawk.ioio.fi/#/view/K7XQ2M"), None);
        assert_eq!(parse_room_code("not a code"), None);
        assert_eq!(parse_room_code(&"x".repeat(40)), None);
    }

    #[test]
    fn defaults_point_at_production() {
        assert_eq!(defaults::RELAY_URL, "https://api.gawk.ioio.fi:4433");
        assert_eq!(defaults::APP_URL, "https://gawk.ioio.fi");
        assert_eq!(
            defaults::TELEMETRY_URL,
            "https://gawk.ioio.fi/api/telemetry/v1/ingest"
        );
    }

    /// docs/54 D14/D16/D17 and docs/47 D5: each distribution's identity is
    /// pinned here, because every one of these strings is matched verbatim
    /// by something outside this workspace — the relay's origin allowlist,
    /// the release job's asset name and manifest path, the telemetry
    /// dashboard's `kind` filter.
    #[test]
    fn distributions_are_pinned() {
        assert_eq!(
            defaults::WINDOWS,
            defaults::Distribution {
                name: "gawk-broadcast-windows",
                origin: "gawk-broadcast://windows",
                asset: "gawk-broadcast-windows-x86_64.exe",
                os: "Windows",
            }
        );
        assert_eq!(
            defaults::MACOS,
            defaults::Distribution {
                name: "gawk-broadcast-macos",
                origin: "gawk-broadcast://macos",
                asset: "gawk-broadcast-macos-arm64.zip",
                os: "macOS",
            }
        );
    }

    /// A macOS build is the macOS distribution; every other target is the
    /// Windows one — including the Linux hosts the Windows shell is tested
    /// and linted on (docs/38 D18), which is why this is not `cfg(windows)`.
    #[test]
    fn this_build_is_the_distribution_of_its_target() {
        let want = if cfg!(target_os = "macos") {
            &defaults::MACOS
        } else {
            &defaults::WINDOWS
        };
        assert_eq!(defaults::THIS, want);
        assert_eq!(defaults::ORIGIN, want.origin);
    }

    #[test]
    fn join_link_matches_the_linux_shells() {
        assert_eq!(
            join_link("https://gawk.ioio.fi", "K7XQ2M"),
            "https://gawk.ioio.fi/#/view/K7XQ2M"
        );
        assert_eq!(
            join_link("https://gawk.ioio.fi/", "K7XQ2M"),
            "https://gawk.ioio.fi/#/view/K7XQ2M"
        );
    }
}
