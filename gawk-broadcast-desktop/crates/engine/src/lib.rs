//! Session lifecycle, send policy, resume supervisor, timesync, stats and
//! telemetry batching — the port of `gawk-server/internal/pubsim/engine`'s
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
#[cfg(feature = "self-update")]
pub mod install;
pub mod link;
pub mod lossnotice;
pub mod media;
pub mod probe;
pub mod relay;
pub mod resume;
pub mod room;
pub mod sender;
pub mod session;
pub mod stats;
pub mod telemetry;
pub mod timesync;
pub mod transport;
#[cfg(feature = "self-update")]
pub mod update;
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
        /// The `app` label every dial carries for the relay's usage metrics
        /// (R59, docs/61 D1): `desktop` for the three shells here, `ios` for
        /// the iOS app, which injects its own identity (docs/67 D5).
        pub app: &'static str,
    }

    pub const WINDOWS: Distribution = Distribution {
        name: "gawk-broadcast-windows",
        origin: "gawk-broadcast://windows",
        asset: "gawk-broadcast-windows-x86_64.exe",
        os: "Windows",
        app: "desktop",
    };

    pub const MACOS: Distribution = Distribution {
        name: "gawk-broadcast-macos",
        origin: "gawk-broadcast://macos",
        asset: "gawk-broadcast-macos-arm64.zip",
        os: "macOS",
        app: "desktop",
    };

    /// The Linux distribution (R56, docs/58 OD10). Unlike the other two it
    /// is never chosen by the target OS: the Linux shell injects it with
    /// [`set_this`], because the Windows shell is linted and tested on Linux
    /// hosts too (docs/38 D18) and must keep its identity there.
    pub const LINUX: Distribution = Distribution {
        name: "gawk-broadcast-linux",
        origin: "gawk-broadcast://linux",
        asset: "gawk-broadcast-linux-x86_64.tar.gz",
        os: "Linux",
        app: "desktop",
    };

    static THIS: std::sync::OnceLock<&'static Distribution> = std::sync::OnceLock::new();

    /// What [`this`] is when no shell injected an identity: macOS on macOS,
    /// Windows everywhere else — the pre-R56 rule, byte for byte.
    const fn target_default() -> &'static Distribution {
        if cfg!(target_os = "macos") {
            &MACOS
        } else {
            &WINDOWS
        }
    }

    /// The distribution this build is (docs/58 D2). The shell injects its
    /// identity; the target OS does not choose it. Unset, it is the pre-R56
    /// rule, so `app-windows`, `app-macos`, the engine's relay suite and
    /// every host test keep today's identity. A Cargo feature was rejected:
    /// `cargo test --workspace` unifies features across crates, so
    /// `app-linux` enabling one would flip `app-windows`'s host tests too.
    pub fn this() -> &'static Distribution {
        THIS.get().copied().unwrap_or(target_default())
    }

    /// Injects this binary's identity, first thing in `main`, before anything
    /// reads [`this`]. Idempotent for the same value; a second, different
    /// value panics, so two shells can never race each other.
    pub fn set_this(dist: &'static Distribution) {
        let set = *THIS.get_or_init(|| dist);
        assert!(
            set == dist,
            "distribution already set to {}, refusing {}",
            set.name,
            dist.name
        );
    }

    /// This build's Origin header — see [`Distribution::origin`].
    pub fn origin() -> &'static str {
        this().origin
    }

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

/// What a room link's `?rt=` carries: the credential that lets the room view
/// act for this broadcaster.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RoomGrant {
    /// A dynamic room's creator token, hex.
    Creator(String),
    /// A static room's attach key.
    Attach(String),
}

impl RoomGrant {
    /// The grant as the `rt` parameter's value, in gawk-app's format
    /// (`grantHandoff.ts` is its home): `c:<hex>` or `a:<key>`.
    pub fn to_rt(&self) -> String {
        match self {
            RoomGrant::Creator(hex) => format!("c:{hex}"),
            RoomGrant::Attach(key) => format!("a:{key}"),
        }
    }

    /// Reads an `rt` value the way gawk-app's `parseGrant` does: `a:` (or
    /// `a.`) and a non-empty key, else a 32-digit hex creator token with an
    /// optional `c:`/`c.` prefix. Anything else is no grant.
    pub fn parse(raw: &str) -> Option<RoomGrant> {
        let v = raw.trim();
        if let Some(key) = v.strip_prefix("a:").or_else(|| v.strip_prefix("a.")) {
            return (!key.is_empty()).then(|| RoomGrant::Attach(key.to_owned()));
        }
        let hex = v
            .strip_prefix("c:")
            .or_else(|| v.strip_prefix("c."))
            .unwrap_or(v);
        (hex.len() == 32 && hex.chars().all(|c| c.is_ascii_hexdigit()))
            .then(|| RoomGrant::Creator(hex.to_ascii_lowercase()))
    }
}

/// What the room field held: the code, and the grant a pasted room link
/// carried in its `?rt=`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RoomInput {
    pub code: String,
    pub grant: Option<RoomGrant>,
}

/// Reads the room field: a bare code or slug, or a room link with its
/// grant (docs/60 D8), or a native `gawk://room/<code>` or
/// `gawk://broadcast?room=<code>` link (docs/68 D2), which never carries a
/// grant. `None` for an empty or unusable input.
pub fn parse_room_input(input: &str) -> Option<RoomInput> {
    if link::is_gawk_link(input) {
        let code = match link::parse(input).ok()?.link {
            link::Link::Room { code, .. } => code,
            link::Link::Broadcast { room, .. } => room?,
            link::Link::Watch { .. } => return None,
        };
        return Some(RoomInput { code, grant: None });
    }
    let code = parse_room_code(input)?;
    let grant = input
        .trim()
        .split_once("#/room/")
        .and_then(|(_, rest)| rest.split_once('?'))
        .and_then(|(_, query)| {
            let query = query.split('#').next().unwrap_or("");
            url::form_urlencoded::parse(query.as_bytes())
                .find(|(k, _)| k == "rt")
                .map(|(_, v)| v.into_owned())
        })
        .and_then(|rt| RoomGrant::parse(&rt));
    Some(RoomInput { code, grant })
}

/// Builds the room view link the "Open room view" button launches (docs/44
/// §4.8): `<app-url>/#/room/<CODE>?rt=<grant>`. The SPA moves the grant into
/// session storage and rewrites the hash before rendering, the same one-shot
/// pattern as `?relay=`. No grant ⇒ a plain participant link.
pub fn room_link(app_url: &str, code: &str, grant: Option<&RoomGrant>) -> String {
    let base = format!("{}/#/room/{}", app_url.trim_end_matches('/'), code);
    let Some(grant) = grant else {
        return base;
    };
    let rt: String = url::form_urlencoded::byte_serialize(grant.to_rt().as_bytes()).collect();
    format!("{base}?rt={rt}")
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

    // The `rt` value is gawk-app's grant format (grantHandoff.ts): `c:<hex>`
    // for a creator token, `a:<key>` for an attach key. A bare value is read
    // only as a creator token, so a bare attach key never reached the room
    // view (docs/60 D14).
    #[test]
    fn room_link_carries_the_grant_in_the_spa_format() {
        assert_eq!(
            room_link("https://gawk.ioio.fi/", "K7XQ2M", None),
            "https://gawk.ioio.fi/#/room/K7XQ2M"
        );
        let creator = RoomGrant::Creator("de".repeat(16));
        assert_eq!(
            room_link("https://gawk.ioio.fi", "K7XQ2M", Some(&creator)),
            format!(
                "https://gawk.ioio.fi/#/room/K7XQ2M?rt=c%3A{}",
                "de".repeat(16)
            )
        );
        // A static room's attach key can be anything; it is form-encoded.
        let attach = RoomGrant::Attach("k 3&y".into());
        assert_eq!(
            room_link("https://gawk.ioio.fi", "lan-party", Some(&attach)),
            "https://gawk.ioio.fi/#/room/lan-party?rt=a%3Ak+3%26y"
        );
    }

    #[test]
    fn grants_parse_like_the_spa() {
        let hex = "AB".repeat(16);
        assert_eq!(
            RoomGrant::parse(&hex),
            Some(RoomGrant::Creator("ab".repeat(16)))
        );
        assert_eq!(
            RoomGrant::parse(&format!("c:{hex}")),
            Some(RoomGrant::Creator("ab".repeat(16)))
        );
        assert_eq!(
            RoomGrant::parse("a:k3y"),
            Some(RoomGrant::Attach("k3y".into()))
        );
        assert_eq!(
            RoomGrant::parse("a.k3y"),
            Some(RoomGrant::Attach("k3y".into()))
        );
        assert_eq!(RoomGrant::parse("a:"), None);
        assert_eq!(RoomGrant::parse("k3y"), None, "a bare key is no grant");
        assert_eq!(RoomGrant::parse(""), None);
        // Round trip through the link builder's format.
        let g = RoomGrant::Attach("k 3&y".into());
        assert_eq!(RoomGrant::parse(&g.to_rt()), Some(g));
    }

    #[test]
    fn room_input_reads_the_grant_of_a_pasted_link() {
        assert_eq!(
            parse_room_input("lan-party"),
            Some(RoomInput {
                code: "lan-party".into(),
                grant: None
            })
        );
        assert_eq!(
            parse_room_input("https://gawk.ioio.fi/#/room/lan-party?rt=a%3Ak+3%26y"),
            Some(RoomInput {
                code: "lan-party".into(),
                grant: Some(RoomGrant::Attach("k 3&y".into()))
            })
        );
        let hex = "0f".repeat(16);
        assert_eq!(
            parse_room_input(&format!("https://gawk.ioio.fi/#/room/K7XQ2M?rt=c:{hex}")),
            Some(RoomInput {
                code: "K7XQ2M".into(),
                grant: Some(RoomGrant::Creator(hex))
            })
        );
        // A junk grant is ignored; the code still joins.
        assert_eq!(
            parse_room_input("https://gawk.ioio.fi/#/room/K7XQ2M?rt=nope"),
            Some(RoomInput {
                code: "K7XQ2M".into(),
                grant: None
            })
        );
        assert_eq!(parse_room_input("not a code"), None);
    }

    // docs/68 D2: a pasted native link works like a pasted web link, and
    // never brings a grant with it.
    #[test]
    fn room_input_reads_a_pasted_gawk_link() {
        let lan = Some(RoomInput {
            code: "lan-party".into(),
            grant: None,
        });
        assert_eq!(parse_room_input(" gawk://room/lan-party?nick=Juho "), lan);
        assert_eq!(
            parse_room_input("GAWK://broadcast?room=lan-party&rt=a%3Ak3y"),
            lan
        );
        assert_eq!(parse_room_input("gawk://broadcast?nick=Juho"), None);
        assert_eq!(parse_room_input("gawk://watch/ABC234"), None);
        assert_eq!(parse_room_input("gawk://room/ab"), None);
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
                app: "desktop",
            }
        );
        assert_eq!(
            defaults::MACOS,
            defaults::Distribution {
                name: "gawk-broadcast-macos",
                origin: "gawk-broadcast://macos",
                asset: "gawk-broadcast-macos-arm64.zip",
                os: "macOS",
                app: "desktop",
            }
        );
        assert_eq!(
            defaults::LINUX,
            defaults::Distribution {
                name: "gawk-broadcast-linux",
                origin: "gawk-broadcast://linux",
                asset: "gawk-broadcast-linux-x86_64.tar.gz",
                os: "Linux",
                app: "desktop",
            }
        );
    }

    /// Unset, a macOS build is the macOS distribution and every other
    /// target is the Windows one — including the Linux hosts the Windows
    /// shell is tested and linted on (docs/38 D18), which is why this is not
    /// `cfg(windows)`. The injected Linux identity is tested in its own test
    /// binary (tests/identity_linux.rs), so the global never leaks here.
    #[test]
    fn this_build_is_the_distribution_of_its_target() {
        let want = if cfg!(target_os = "macos") {
            &defaults::MACOS
        } else {
            &defaults::WINDOWS
        };
        assert_eq!(defaults::this(), want);
        assert_eq!(defaults::origin(), want.origin);
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
