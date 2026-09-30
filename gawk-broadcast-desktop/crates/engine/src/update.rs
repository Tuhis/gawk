//! The launch-time update check (R45, docs/47): one plain GET of this
//! distribution's R46 release manifest, validated the way the project site
//! reads it, compared on the `X.Y.Z` release part only.
//!
//! Shell-free like the telemetry reporter beside it: the current release is
//! passed in as a `&str` (the app's `version::RELEASE`), and the result is a
//! value the shell turns into the line under the version badge. Nothing from
//! the manifest is executed or rendered as markup — it yields one version to
//! compare and one URL, prefix-checked against the project's own releases
//! path before anything may open it (docs/47 §5).

use crate::defaults::Distribution;
use serde::Deserialize;
use std::time::Duration;

/// Where every published manifest lives: the orphan `badges` branch, served
/// by raw GitHub (docs/46 D1 — no API, no per-IP quota). A fork edits this,
/// like the relay default beside it (docs/47 D1). Never a relay-advertised
/// value: a relay must not choose which binary a broadcaster runs.
const MANIFEST_BASE: &str = "https://raw.githubusercontent.com/Tuhis/gawk/badges/releases/";

/// The fixed User-Agent (docs/47 D2): no version in it, or the request would
/// carry exactly the fact the check must not reveal.
pub const USER_AGENT: &str = "gawk-broadcast-update-check";

/// docs/47 D2: one attempt, bounded, never retried.
const TIMEOUT: Duration = Duration::from_secs(10);

/// The manifest is under 2 KB; anything past this is not one.
const MAX_BODY: u64 = 64 * 1024;

/// docs/47 D3: at most one check a day per machine.
const INTERVAL_SECS: u64 = 24 * 60 * 60;

/// The environment opt-out every shell honours (docs/47 D6). Read with
/// `var_os`, so it needs no console on the windowed Windows EXE.
pub const ENV_OPT_OUT: &str = "GAWK_NO_UPDATE_CHECK";

const RELEASE_URL_PREFIX: &str = "https://github.com/Tuhis/gawk/releases/tag/";
const ASSET_URL_PREFIX: &str = "https://github.com/Tuhis/gawk/releases/download/";

/// This distribution's manifest: `releases/<name>/latest.json`.
pub fn manifest_url(dist: &Distribution) -> String {
    format!("{MANIFEST_BASE}{}/latest.json", dist.name)
}

/// A newer release than the running one, fit to show.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Update {
    /// Bare `X.Y.Z`.
    pub version: String,
    /// The release page — already checked to be under the project's own
    /// releases path.
    pub release_url: String,
}

/// The fields of the docs/46 D5 shape this check reads. Unknown keys
/// (`assets`, `published_at`, `tag`, anything added later) are ignored.
#[derive(Debug, Deserialize)]
struct Manifest {
    schema: u32,
    component: String,
    version: String,
    release_url: String,
    asset: Asset,
}

#[derive(Debug, Deserialize)]
struct Asset {
    name: String,
    sha256: String,
    url: String,
}

/// Parses a bare `X.Y.Z` — digits only, no sign, no pre-release, no build
/// metadata (the manifest writer rejects both, `manifest.py`).
fn parse_release(v: &str) -> Option<(u64, u64, u64)> {
    let mut parts = v.split('.');
    let mut next = || -> Option<u64> {
        let p = parts.next()?;
        if p.is_empty() || !p.bytes().all(|b| b.is_ascii_digit()) {
            return None;
        }
        p.parse().ok()
    };
    let r = (next()?, next()?, next()?);
    parts.next().is_none().then_some(r)
}

/// Whether `latest` is strictly newer than `current` (docs/47 D4). Only the
/// release parts compare: a `+g<sha>` build of a release is that release, so
/// a dev build is never nagged about itself. An unparseable side is "no".
pub fn newer(current: &str, latest: &str) -> bool {
    let current = current.split('+').next().unwrap_or("");
    match (parse_release(current), parse_release(latest)) {
        (Some(c), Some(l)) => l > c,
        _ => false,
    }
}

/// Validates a manifest body for `dist` and returns the release it names
/// when that is newer than `current`. Any shape failure is "no update"
/// (docs/47 D5), exactly like an equal or older version.
pub fn evaluate(body: &str, current: &str, dist: &Distribution) -> Option<Update> {
    let m: Manifest = serde_json::from_str(body).ok()?;
    let valid = m.schema == 1
        && m.component == dist.name
        && parse_release(&m.version).is_some()
        && safe_release_url(&m.release_url)
        && m.asset.url.starts_with(ASSET_URL_PREFIX)
        && m.asset.name == dist.asset
        && m.asset.sha256.len() == 64
        && m.asset
            .sha256
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b));
    (valid && newer(current, &m.version)).then_some(Update {
        version: m.version,
        release_url: m.release_url,
    })
}

/// The release page is the one manifest value the app opens, and on Windows
/// it is opened through `cmd /c start`, where `&`, `|`, `^` and `%` are shell
/// syntax that Rust's argument quoting does not escape. So past the prefix
/// only the characters a release tag path uses are allowed — a prefix check
/// alone would let a manifest run a command on click (PR #414 review).
fn safe_release_url(url: &str) -> bool {
    url.strip_prefix(RELEASE_URL_PREFIX).is_some_and(|rest| {
        !rest.is_empty()
            && rest
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"._/-".contains(&b))
    })
}

/// What one check came to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Outcome {
    /// GitHub answered — with a newer release or with nothing to say (equal,
    /// older, a 404, a manifest that failed validation). The day's check is
    /// spent.
    Answered(Option<Update>),
    /// No answer at all (offline, DNS, TLS, timeout): the check is retried at
    /// the next launch, so the timestamp does not move (docs/47 D3).
    Unreachable(String),
}

/// Fetches `url` and evaluates it. Blocking — call it off the UI thread.
/// The request is a bare GET: no query, no conditional headers, no cookies,
/// the fixed [`USER_AGENT`] (docs/47 D2). TLS verifies against the bundled
/// roots always; no relay setting reaches this client (docs/47 D10).
pub fn check(url: &str, current: &str, dist: &Distribution) -> Outcome {
    let agent: ureq::Agent = ureq::Agent::config_builder()
        .timeout_global(Some(TIMEOUT))
        .http_status_as_error(false)
        .user_agent(USER_AGENT)
        .build()
        .into();
    let mut resp = match agent.get(url).call() {
        Ok(r) => r,
        Err(e) => return Outcome::Unreachable(e.to_string()),
    };
    if resp.status() != 200 {
        return Outcome::Answered(None);
    }
    match resp
        .body_mut()
        .with_config()
        .limit(MAX_BODY)
        .read_to_string()
    {
        Ok(body) => Outcome::Answered(evaluate(&body, current, dist)),
        Err(e) => Outcome::Unreachable(e.to_string()),
    }
}

/// Whether a check is due: never checked, a stamp that does not parse, a
/// stamp in the future (a clock that moved back), or 24 h since the last.
pub fn due(last_check: &str, now_unix: u64) -> bool {
    match parse_rfc3339(last_check) {
        Some(last) if last <= now_unix => now_unix - last >= INTERVAL_SECS,
        _ => true,
    }
}

/// Whether the environment turned the check off: any non-empty value but
/// `0`, the way the Go CLI reads its boolean envs.
pub fn env_opted_out() -> bool {
    std::env::var_os(ENV_OPT_OUT).is_some_and(|v| !v.is_empty() && v != "0")
}

/// Whether the notice for `update` should show, given the version the user
/// last dismissed: dismissal is per version (docs/47 D7).
pub fn visible(update: &Update, dismissed: &str) -> bool {
    update.version != dismissed.trim()
}

/// Unix seconds as `YYYY-MM-DDTHH:MM:SSZ` — the `lastUpdateCheck` format.
pub fn format_rfc3339(secs: u64) -> String {
    let days = (secs / 86_400) as i64;
    let (h, m, s) = ((secs % 86_400) / 3600, (secs % 3600) / 60, secs % 60);
    // Civil-from-days (Howard Hinnant's algorithm).
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let mo = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = yoe + era * 400 + i64::from(mo <= 2);
    format!("{y:04}-{mo:02}-{d:02}T{h:02}:{m:02}:{s:02}Z")
}

/// Reads exactly the shape [`format_rfc3339`] writes, UTC only. Anything
/// else is `None`, which [`due`] reads as "check now".
fn parse_rfc3339(s: &str) -> Option<u64> {
    let b = s.as_bytes();
    if b.len() != 20
        || b[4] != b'-'
        || b[7] != b'-'
        || b[10] != b'T'
        || b[13] != b':'
        || b[16] != b':'
        || b[19] != b'Z'
    {
        return None;
    }
    let num = |r: std::ops::Range<usize>| -> Option<i64> {
        let p = &s[r];
        p.bytes()
            .all(|c| c.is_ascii_digit())
            .then(|| p.parse().ok())?
    };
    let (y, mo, d) = (num(0..4)?, num(5..7)?, num(8..10)?);
    let (h, mi, sec) = (num(11..13)?, num(14..16)?, num(17..19)?);
    if !(1..=12).contains(&mo) || !(1..=31).contains(&d) || h > 23 || mi > 59 || sec > 59 {
        return None;
    }
    // Days-from-civil (Hinnant), the inverse of format_rfc3339.
    let y = if mo <= 2 { y - 1 } else { y };
    let era = y.div_euclid(400);
    let yoe = y - era * 400;
    let mp = (mo + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146_097 + doe - 719_468;
    u64::try_from(days * 86_400 + h * 3600 + mi * 60 + sec).ok()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::defaults;
    use std::io::{Read, Write};
    use std::sync::mpsc;

    /// The docs/46 D5 example, restated (the `wirecheck` convention: the
    /// consumer pins its own copy of the contract, so a writer change is loud
    /// here) — the live Windows manifest of v2.0.0, trimmed to two assets.
    fn manifest(component: &str, version: &str, asset: &str) -> String {
        format!(
            r#"{{
  "asset": {{
    "name": "{asset}",
    "sha256": "e188bafc30e9b3204ffd6798a5a8c7b892642b3d01d65309dfb6ccc7167312af",
    "size": 31772160,
    "url": "https://github.com/Tuhis/gawk/releases/download/gawk-broadcast-desktop/v{version}/{asset}"
  }},
  "assets": {{
    "SHA256SUMS": {{
      "sha256": "e6287d7f661c905e954893e221b92a14a46ae81a6318f5b43cc7754fd1afd8b8",
      "size": 641,
      "url": "https://github.com/Tuhis/gawk/releases/download/gawk-broadcast-desktop/v{version}/SHA256SUMS"
    }}
  }},
  "component": "{component}",
  "published_at": "2026-09-29T21:31:14Z",
  "release_url": "https://github.com/Tuhis/gawk/releases/tag/gawk-broadcast-desktop/v{version}",
  "schema": 1,
  "tag": "gawk-broadcast-desktop/v{version}",
  "version": "{version}"
}}"#
        )
    }

    fn win(version: &str) -> String {
        manifest(defaults::WINDOWS.name, version, defaults::WINDOWS.asset)
    }

    #[test]
    fn manifest_urls_are_pinned_per_distribution() {
        assert_eq!(
            manifest_url(&defaults::WINDOWS),
            "https://raw.githubusercontent.com/Tuhis/gawk/badges/releases/gawk-broadcast-windows/latest.json"
        );
        assert_eq!(
            manifest_url(&defaults::MACOS),
            "https://raw.githubusercontent.com/Tuhis/gawk/badges/releases/gawk-broadcast-macos/latest.json"
        );
        assert_eq!(
            manifest_url(&defaults::LINUX),
            "https://raw.githubusercontent.com/Tuhis/gawk/badges/releases/gawk-broadcast-linux/latest.json"
        );
    }

    #[test]
    fn a_newer_release_is_an_update() {
        assert_eq!(
            evaluate(&win("2.1.0"), "2.0.0", &defaults::WINDOWS),
            Some(Update {
                version: "2.1.0".into(),
                release_url:
                    "https://github.com/Tuhis/gawk/releases/tag/gawk-broadcast-desktop/v2.1.0"
                        .into(),
            })
        );
    }

    #[test]
    fn equal_older_and_a_dev_build_of_the_same_release_say_nothing() {
        let d = &defaults::WINDOWS;
        assert_eq!(evaluate(&win("2.0.0"), "2.0.0", d), None);
        // A retraction (docs/47 D11) is an older manifest, never a prompt.
        assert_eq!(evaluate(&win("1.9.9"), "2.0.0", d), None);
        assert_eq!(evaluate(&win("2.0.0"), "2.0.0+g1a2b3c4", d), None);
        // …but a dev build of an older release still hears about the new one.
        assert!(evaluate(&win("2.0.1"), "2.0.0+g1a2b3c4", d).is_some());
    }

    #[test]
    fn versions_compare_numerically_not_as_strings() {
        assert!(newer("2.9.0", "2.10.0"));
        assert!(newer("1.99.99", "2.0.0"));
        assert!(!newer("2.10.0", "2.9.0"));
        assert!(!newer("", "2.0.0"), "an unparseable current is no");
        assert!(!newer("2.0.0", "2.1"), "an unparseable latest is no");
    }

    /// docs/47 D5: every failure is "no update", never an error surfaced.
    #[test]
    fn a_manifest_failing_validation_is_no_update() {
        let d = &defaults::WINDOWS;
        let good = win("9.0.0");
        assert!(evaluate(&good, "2.0.0", d).is_some(), "the baseline passes");
        let cases = [
            ("bad schema", good.replace(r#""schema": 1"#, r#""schema": 2"#)),
            (
                "wrong component",
                good.replace(
                    r#""component": "gawk-broadcast-windows""#,
                    r#""component": "gawk-broadcast-linux""#,
                ),
            ),
            (
                "pre-release version",
                good.replace(r#""version": "9.0.0""#, r#""version": "9.0.0-rc.1""#),
            ),
            (
                "build metadata",
                good.replace(r#""version": "9.0.0""#, r#""version": "9.0.0+g1""#),
            ),
            (
                "foreign release_url",
                good.replace(
                    "https://github.com/Tuhis/gawk/releases/tag/",
                    "https://evil.example/releases/tag/",
                ),
            ),
            (
                "foreign asset url",
                good.replace(
                    "https://github.com/Tuhis/gawk/releases/download/gawk-broadcast-desktop/v9.0.0/gawk-broadcast-windows",
                    "https://evil.example/gawk-broadcast-windows",
                ),
            ),
            (
                "wrong asset name",
                good.replace(
                    r#""name": "gawk-broadcast-windows-x86_64.exe""#,
                    r#""name": "gawk-broadcast-linux-x86_64.tar.gz""#,
                ),
            ),
            (
                "short sha256",
                good.replace("e188bafc30e9b3204ffd", "e188bafc30e9b3204ff"),
            ),
            (
                "uppercase sha256",
                good.replace("e188bafc30e9b3204ffd", "E188BAFC30E9B3204FFD"),
            ),
            // The release URL reaches `cmd /c start` on Windows, where these
            // are shell syntax rather than URL characters (PR #414 review).
            (
                "cmd separator in release_url",
                good.replace("/tag/gawk-broadcast-desktop/v9.0.0", "/tag/v9&calc.exe"),
            ),
            (
                "cmd pipe in release_url",
                good.replace("/tag/gawk-broadcast-desktop/v9.0.0", "/tag/v9|calc"),
            ),
            (
                "cmd variable in release_url",
                good.replace("/tag/gawk-broadcast-desktop/v9.0.0", "/tag/%COMSPEC%"),
            ),
            (
                "cmd escape in release_url",
                good.replace("/tag/gawk-broadcast-desktop/v9.0.0", "/tag/v9^x"),
            ),
            (
                "space in release_url",
                good.replace("/tag/gawk-broadcast-desktop/v9.0.0", "/tag/v9 x"),
            ),
            (
                "bare prefix release_url",
                good.replace("/tag/gawk-broadcast-desktop/v9.0.0", "/tag/"),
            ),
            ("missing asset", good.replace(r#""asset": {"#, r#""other": {"#)),
            ("not JSON", "<html>rate limited</html>".into()),
        ];
        for (what, body) in cases {
            assert_ne!(body, good, "{what}: the mutation must change the body");
            assert_eq!(evaluate(&body, "2.0.0", d), None, "{what}");
        }
    }

    #[test]
    fn each_distribution_accepts_only_its_own_manifest() {
        let linux = manifest(defaults::LINUX.name, "9.0.0", defaults::LINUX.asset);
        assert!(evaluate(&linux, "2.0.0", &defaults::LINUX).is_some());
        assert_eq!(evaluate(&linux, "2.0.0", &defaults::WINDOWS), None);
        assert_eq!(evaluate(&linux, "2.0.0", &defaults::MACOS), None);
    }

    #[test]
    fn a_check_is_due_daily() {
        let now = 1_790_000_000; // 2026-09-21
        assert!(due("", now), "never checked");
        assert!(due("yesterday", now), "an unreadable stamp");
        let hour_ago = format_rfc3339(now - 3600);
        assert!(!due(&hour_ago, now));
        let almost = format_rfc3339(now - INTERVAL_SECS + 1);
        assert!(!due(&almost, now));
        let day_ago = format_rfc3339(now - INTERVAL_SECS);
        assert!(due(&day_ago, now));
        let future = format_rfc3339(now + 3600);
        assert!(due(&future, now), "a clock that moved back must not stall");
    }

    #[test]
    fn stamps_round_trip() {
        for secs in [0, 951_782_400, 1_790_000_000, 4_102_444_799] {
            assert_eq!(parse_rfc3339(&format_rfc3339(secs)), Some(secs), "{secs}");
        }
        assert_eq!(format_rfc3339(0), "1970-01-01T00:00:00Z");
        // 2000-02-29 (a leap day in a century leap year).
        assert_eq!(format_rfc3339(951_782_400), "2000-02-29T00:00:00Z");
        assert_eq!(parse_rfc3339("2026-09-30T12:00:00+03:00"), None);
        assert_eq!(parse_rfc3339("2026-13-01T00:00:00Z"), None);
    }

    #[test]
    fn dismissal_is_per_version() {
        let u = Update {
            version: "2.1.0".into(),
            release_url: String::new(),
        };
        assert!(visible(&u, ""));
        assert!(!visible(&u, "2.1.0"));
        assert!(
            visible(&u, "2.0.1"),
            "a newer release than the dismissed one shows"
        );
    }

    /// One-shot HTTP server: answers one request with `status` + `body` and
    /// sends the raw request head to the channel.
    fn serve(status: u16, body: String) -> (String, mpsc::Receiver<String>) {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!(
            "http://{}/releases/gawk-broadcast-windows/latest.json",
            listener.local_addr().unwrap()
        );
        let (tx, rx) = mpsc::channel();
        std::thread::spawn(move || {
            let Ok((mut conn, _)) = listener.accept() else {
                return;
            };
            let mut buf = Vec::new();
            let mut tmp = [0u8; 4096];
            while !buf.windows(4).any(|w| w == b"\r\n\r\n") {
                match conn.read(&mut tmp) {
                    Ok(0) | Err(_) => return,
                    Ok(k) => buf.extend_from_slice(&tmp[..k]),
                }
            }
            let _ = tx.send(String::from_utf8_lossy(&buf).into_owned());
            let _ = conn.write_all(
                format!(
                    "HTTP/1.1 {status} X\r\ncontent-type: text/plain\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
                    body.len()
                )
                .as_bytes(),
            );
        });
        (url, rx)
    }

    /// docs/47 D2: a bare GET — no query string, no conditional headers, no
    /// cookies, and a User-Agent that is exactly the fixed string.
    #[test]
    fn the_request_carries_nothing_identifying() {
        let (url, rx) = serve(200, win("9.0.0"));
        let got = check(&url, "2.0.0", &defaults::WINDOWS);
        assert!(matches!(got, Outcome::Answered(Some(ref u)) if u.version == "9.0.0"));

        let head = rx.recv_timeout(Duration::from_secs(5)).unwrap();
        let mut lines = head.lines();
        assert_eq!(
            lines.next().unwrap(),
            "GET /releases/gawk-broadcast-windows/latest.json HTTP/1.1"
        );
        let mut ua = None;
        for line in lines.filter(|l| !l.is_empty()) {
            let (k, v) = line.split_once(':').unwrap();
            let k = k.to_ascii_lowercase();
            assert!(!k.starts_with("if-"), "conditional header sent: {line}");
            assert_ne!(k, "cookie", "cookie sent: {line}");
            assert!(
                !v.contains("2.0.0"),
                "the running version leaked into a header: {line}"
            );
            if k == "user-agent" {
                ua = Some(v.trim().to_string());
            }
        }
        assert_eq!(ua.as_deref(), Some(USER_AGENT));
    }

    #[test]
    fn a_404_is_an_answer_and_no_connection_is_not() {
        let (url, _rx) = serve(404, "404: Not Found".into());
        assert_eq!(
            check(&url, "2.0.0", &defaults::WINDOWS),
            Outcome::Answered(None)
        );
        // Nothing listens on port 1: the stamp must not move.
        assert!(matches!(
            check(
                "http://127.0.0.1:1/latest.json",
                "2.0.0",
                &defaults::WINDOWS
            ),
            Outcome::Unreachable(_)
        ));
    }

    #[test]
    fn a_manifest_that_fails_validation_over_the_wire_is_an_answer() {
        let (url, _rx) = serve(200, "<html>not a manifest</html>".into());
        assert_eq!(
            check(&url, "2.0.0", &defaults::WINDOWS),
            Outcome::Answered(None)
        );
    }
}
