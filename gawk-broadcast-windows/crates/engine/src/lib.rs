//! Session lifecycle, send policy, resume supervisor, timesync, stats and
//! telemetry batching — the port of `gawk-broadcast/internal/engine`'s
//! semantics (docs/38 D5). WB0 scaffold: only the production defaults are
//! pinned so far; the session machinery lands in WB2.
//!
//! This crate deliberately has no GUI and no COM/WinRT dependencies: media
//! enters through source traits, the network through a `RelaySession` seam —
//! which is what keeps a future CLI or pubsim-style shell possible without
//! redesign (docs/38 OD12).

/// Shipped defaults, all pointing at the production gawk deployment
/// (docs/38 D13). "Blank means the default, resolved at use, never at save" —
/// the config file never stores a resolved default, so a release that moves
/// the fleet address moves every user with it.
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
    /// This client's self-identifying Origin header (docs/38 D19). A native
    /// client must send one, or an allowlisting relay matches it against
    /// nothing; the production relay's allowlist must include this value.
    pub const ORIGIN: &str = "gawk-broadcast://windows";

    /// The fixed rung (docs/38 D11): 1080p60, 500 ms GOP, 16 Mbps peak
    /// (peak-constrained VBR; typical motion averages ~75 % of it).
    pub const WIDTH: u32 = 1920;
    pub const HEIGHT: u32 = 1080;
    pub const FPS: u32 = 60;
    pub const BITRATE_BPS: u32 = 16_000_000;
    pub const GOP_MS: u32 = 500;
}

/// Builds the join link for a broadcast code the way both Linux shells do:
/// `<app-url>/#/view/<ID>` with a trailing slash trimmed first.
pub fn join_link(app_url: &str, broadcast_id: &str) -> String {
    format!("{}/#/view/{}", app_url.trim_end_matches('/'), broadcast_id)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_point_at_production() {
        assert_eq!(defaults::RELAY_URL, "https://api.gawk.ioio.fi:4433");
        assert_eq!(defaults::APP_URL, "https://gawk.ioio.fi");
        assert_eq!(
            defaults::TELEMETRY_URL,
            "https://gawk.ioio.fi/api/telemetry/v1/ingest"
        );
        assert_eq!(defaults::ORIGIN, "gawk-broadcast://windows");
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
