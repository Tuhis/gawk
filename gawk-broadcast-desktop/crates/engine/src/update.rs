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
//!
//! R47 (docs/48) adds the download: [`stage`] fetches the release's
//! `SHA256SUMS` and its minisign signature, verifies the signature against
//! the one compiled-in [`RELEASE_KEY`] and reads the release version from
//! the signed trusted comment, then downloads the asset and checks it
//! against its signed `SHA256SUMS` line. Only then does anything else get
//! to touch the file (the swap is [`crate::install`]).

use crate::defaults::Distribution;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
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

/// The automatic check runs at most this often per machine (owner decision
/// 2026-09-30, replacing docs/47 D3's day): a relaunch inside the window
/// shows the cached result instead of asking again.
const INTERVAL_SECS: u64 = 15 * 60;

/// The environment opt-out every shell honours (docs/47 D6). Read with
/// `var_os`, so it needs no console on the windowed Windows EXE.
pub const ENV_OPT_OUT: &str = "GAWK_NO_UPDATE_CHECK";

const RELEASE_URL_PREFIX: &str = "https://github.com/Tuhis/gawk/releases/tag/";
const ASSET_URL_PREFIX: &str = "https://github.com/Tuhis/gawk/releases/download/";

/// The release signing key (docs/48 D1, D2): one minisign public key,
/// compiled in, key ID `EFB62FBC86D13877`. A signature by any other key is
/// refused, and the app falls back to the R45 notice. Rotating it is a
/// bridge release (docs/48 §6); `tools/releases/keys/gawk-release.pub` is
/// the copy CI verifies with, and the two must change together.
pub const RELEASE_KEY: &str = "RWR3ONGGvC+27y0Zc84TSry1R0nqIo3abz92W34jn0KnRyMUroY2vydy";

/// The release-please component every desktop release is cut as. The
/// signature's trusted comment is exactly `"<this> <X.Y.Z>"`, so the
/// version being installed is one the release key vouched for, not one the
/// unsigned manifest claims (docs/48 D3).
pub const RELEASE_COMPONENT: &str = "gawk-broadcast-desktop";

/// The two release files that make the download verifiable (docs/48 D1).
pub const SUMS_NAME: &str = "SHA256SUMS";
pub const SIG_NAME: &str = "SHA256SUMS.minisig";

/// `SHA256SUMS` and its signature are well under a kilobyte.
const MAX_SIDECAR: u64 = 64 * 1024;

/// No desktop asset comes near this (the EXE is ~30 MB); a manifest that
/// claims more is not ours.
const MAX_ASSET: u64 = 512 * 1024 * 1024;

/// A whole download, not one request: a slow home link still fits.
const DOWNLOAD_TIMEOUT: Duration = Duration::from_secs(15 * 60);

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
    /// What an in-place install downloads (R47): present only when the
    /// manifest lists the asset, `SHA256SUMS` and its signature, each under
    /// the project's download path. A remembered update (see [`cached`])
    /// has none. Boxed: it is most of the struct, and [`Outcome`] carries
    /// an `Update` beside a `String`.
    pub files: Option<Box<ReleaseFiles>>,
    /// The Linux `.deb` of this release, by its exact name (docs/48 D9,
    /// docs/63 D2), when the manifest lists it.
    pub deb: Option<String>,
}

/// One downloadable release file, from the manifest's `assets` map.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Remote {
    pub name: String,
    pub url: String,
    pub size: u64,
}

/// The files a verified install needs (docs/48 D3).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReleaseFiles {
    pub asset: Remote,
    pub sums: Remote,
    pub sig: Remote,
}

/// The fields of the docs/46 D5 shape this check reads. Unknown keys
/// (`published_at`, `tag`, anything added later) are ignored.
#[derive(Debug, Deserialize)]
struct Manifest {
    schema: u32,
    component: String,
    version: String,
    release_url: String,
    asset: Asset,
    /// Every file on the release (docs/46 D5). Read loosely, as a JSON
    /// value: missing or malformed, the notice still shows and only the
    /// in-place install is off.
    #[serde(default)]
    assets: serde_json::Value,
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
    if !(valid && newer(current, &m.version)) {
        return None;
    }
    let remote = |name: &str| -> Option<Remote> {
        let listed = m.assets.get(name)?;
        let url = listed.get("url")?.as_str()?;
        let size = listed.get("size")?.as_u64()?;
        (safe_asset_url(url, name) && size <= MAX_ASSET).then(|| Remote {
            name: name.to_string(),
            url: url.to_string(),
            size,
        })
    };
    let files = (|| {
        Some(Box::new(ReleaseFiles {
            asset: remote(&m.asset.name)?,
            sums: remote(SUMS_NAME)?,
            sig: remote(SIG_NAME)?,
        }))
    })();
    let deb_name = format!("gawk-broadcast_{}_amd64.deb", m.version);
    let deb = (dist.name == crate::defaults::LINUX.name)
        .then(|| remote(&deb_name))
        .flatten()
        .map(|r| r.name);
    Some(Update {
        version: m.version,
        release_url: m.release_url,
        files,
        deb,
    })
}

/// A release file's URL: under the project's download path, ending in the
/// file's own name, and nothing but the characters a tag path and a file
/// name use.
fn safe_asset_url(url: &str, name: &str) -> bool {
    url.strip_prefix(ASSET_URL_PREFIX).is_some_and(|rest| {
        rest.ends_with(&format!("/{name}"))
            && rest
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"._/-+".contains(&b))
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
/// stamp in the future (a clock that moved back), or 15 minutes since the
/// last answered one.
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

/// The update the last answered check found, as the config remembers it
/// (`updateVersion`, `updateUrl`), when it is still newer than the running
/// build — an upgrade makes it stale, and so does a hand-edited file. The URL
/// is held to the same rule as a fresh manifest's, because it is opened.
pub fn cached(version: &str, url: &str, current: &str) -> Option<Update> {
    (newer(current, version) && safe_release_url(url)).then(|| Update {
        version: version.to_string(),
        release_url: url.to_string(),
        files: None,
        deb: None,
    })
}

/// Checks `sig` over `sums` with the public key `key` and returns the
/// release version the signature's trusted comment names. Refuses a
/// signature by any other key, a comment that is not exactly
/// `"gawk-broadcast-desktop X.Y.Z"`, and a version that is not newer than
/// `current` (docs/48 D3: a manifest pointing at an older signed release is
/// a no-op, never a rollback).
pub fn verify_release(sums: &[u8], sig: &str, key: &str, current: &str) -> Result<String, String> {
    let pk = minisign_verify::PublicKey::from_base64(key)
        .map_err(|e| format!("the compiled-in key does not parse: {e}"))?;
    let sig = minisign_verify::Signature::decode(sig)
        .map_err(|e| format!("the signature does not parse: {e}"))?;
    pk.verify(sums, &sig, false)
        .map_err(|e| format!("{SUMS_NAME} is not signed by the release key: {e}"))?;
    let version = sig
        .trusted_comment()
        .strip_prefix(RELEASE_COMPONENT)
        .and_then(|rest| rest.strip_prefix(' '))
        .filter(|v| parse_release(v).is_some())
        .ok_or_else(|| {
            format!(
                "the signed comment {:?} does not name a {RELEASE_COMPONENT} release",
                sig.trusted_comment()
            )
        })?;
    if !newer(current, version) {
        return Err(format!(
            "the signed release {version} is not newer than {current}"
        ));
    }
    Ok(version.to_string())
}

/// The SHA-256 `SHA256SUMS` lists for `name`, in the `sha256sum ./*` shape
/// the attach job writes (`<hex>  ./<name>`). `None` when the name is
/// missing or listed twice.
pub fn sums_entry(sums: &str, name: &str) -> Option<String> {
    let mut found = None;
    for line in sums.lines() {
        let Some((hex, file)) = line.split_once(' ') else {
            continue;
        };
        let file = file.trim_start_matches([' ', '*']);
        let file = file.strip_prefix("./").unwrap_or(file);
        if file != name {
            continue;
        }
        if found.is_some() || !is_sha256_hex(hex) {
            return None;
        }
        found = Some(hex.to_string());
    }
    found
}

fn is_sha256_hex(s: &str) -> bool {
    s.len() == 64
        && s.bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

/// A release asset on disk, verified: the signature over `SHA256SUMS`, the
/// version in it, and the file's hash against its signed line.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Staged {
    pub version: String,
    pub file: PathBuf,
}

/// Downloads and verifies `update`'s asset into `dir` (docs/48 D3):
/// signature first, then the version it names, then the asset against its
/// signed hash. Nothing outside `dir` is touched, and a file that fails is
/// deleted. Blocking — call it off the UI thread.
pub fn stage(update: &Update, current: &str, dir: &Path) -> Result<Staged, String> {
    stage_with(update, current, RELEASE_KEY, dir, &fetch, &download)
}

/// GETs a small file whole: `(url, byte limit)`.
pub type FetchFn = dyn Fn(&str, u64) -> Result<Vec<u8>, String>;
/// Streams a file to a path and returns its SHA-256: `(url, path, limit)`.
pub type DownloadFn = dyn Fn(&str, &Path, u64) -> Result<String, String>;

/// [`stage`] with the key and the network passed in, for the tests.
pub fn stage_with(
    update: &Update,
    current: &str,
    key: &str,
    dir: &Path,
    fetch: &FetchFn,
    download: &DownloadFn,
) -> Result<Staged, String> {
    let files = update
        .files
        .as_ref()
        .ok_or("the manifest does not list a signed release")?;
    let sig = fetch(&files.sig.url, MAX_SIDECAR)?;
    let sig = String::from_utf8(sig).map_err(|_| "the signature is not text".to_string())?;
    let sums = fetch(&files.sums.url, MAX_SIDECAR)?;
    let version = verify_release(&sums, &sig, key, current)?;
    if version != update.version {
        return Err(format!(
            "the manifest says {} but the signed release is {version}",
            update.version
        ));
    }
    let sums = String::from_utf8(sums).map_err(|_| format!("{SUMS_NAME} is not text"))?;
    let want = sums_entry(&sums, &files.asset.name)
        .ok_or_else(|| format!("{SUMS_NAME} does not list {}", files.asset.name))?;

    std::fs::create_dir_all(dir).map_err(|e| format!("{}: {e}", dir.display()))?;
    let file = dir.join(&files.asset.name);
    let part = dir.join(format!("{}.part", files.asset.name));
    let got = download(&files.asset.url, &part, files.asset.size.min(MAX_ASSET));
    let got = match got {
        Ok(h) => h,
        Err(e) => {
            let _ = std::fs::remove_file(&part);
            return Err(e);
        }
    };
    if got != want {
        let _ = std::fs::remove_file(&part);
        return Err(format!(
            "{} does not match its signed hash",
            files.asset.name
        ));
    }
    std::fs::rename(&part, &file).map_err(|e| format!("{}: {e}", file.display()))?;
    Ok(Staged { version, file })
}

/// The download client: the check's fixed User-Agent and nothing else
/// (docs/48 SU2, docs/47 D2), redirects followed (GitHub serves release
/// assets from its object store), a whole-transfer timeout.
fn download_agent() -> ureq::Agent {
    ureq::Agent::config_builder()
        .timeout_global(Some(DOWNLOAD_TIMEOUT))
        .user_agent(USER_AGENT)
        .build()
        .into()
}

/// GETs a small file whole, refusing more than `limit` bytes.
fn fetch(url: &str, limit: u64) -> Result<Vec<u8>, String> {
    let mut resp = download_agent()
        .get(url)
        .call()
        .map_err(|e| e.to_string())?;
    resp.body_mut()
        .with_config()
        .limit(limit)
        .read_to_vec()
        .map_err(|e| e.to_string())
}

/// Streams `url` into `path` and returns its SHA-256, refusing more than
/// `limit` bytes.
fn download(url: &str, path: &Path, limit: u64) -> Result<String, String> {
    let mut resp = download_agent()
        .get(url)
        .call()
        .map_err(|e| e.to_string())?;
    let mut body = resp.body_mut().with_config().limit(limit).reader();
    let mut out = std::fs::File::create(path).map_err(|e| format!("{}: {e}", path.display()))?;
    let mut hasher = Sha256::new();
    let mut buf = vec![0u8; 64 * 1024];
    loop {
        let n = body.read(&mut buf).map_err(|e| e.to_string())?;
        if n == 0 {
            break;
        }
        hasher.update(&buf[..n]);
        out.write_all(&buf[..n])
            .map_err(|e| format!("{}: {e}", path.display()))?;
    }
    out.sync_all()
        .map_err(|e| format!("{}: {e}", path.display()))?;
    Ok(hex(&hasher.finalize()))
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
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
                // The fixture lists SHA256SUMS but no signature.
                files: None,
                deb: None,
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
    fn a_check_is_due_every_fifteen_minutes() {
        let now = 1_790_000_000; // 2026-09-21
        assert!(due("", now), "never checked");
        assert!(due("yesterday", now), "an unreadable stamp");
        let minute_ago = format_rfc3339(now - 60);
        assert!(!due(&minute_ago, now));
        let almost = format_rfc3339(now - 15 * 60 + 1);
        assert!(!due(&almost, now));
        let quarter_ago = format_rfc3339(now - 15 * 60);
        assert!(due(&quarter_ago, now));
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
    fn a_cached_update_shows_only_while_newer_and_safe() {
        let url = "https://github.com/Tuhis/gawk/releases/tag/gawk-broadcast-desktop/v2.1.0";
        assert_eq!(
            cached("2.1.0", url, "2.0.0"),
            Some(Update {
                version: "2.1.0".into(),
                release_url: url.into(),
                files: None,
                deb: None,
            })
        );
        assert_eq!(cached("2.1.0", url, "2.1.0"), None, "already upgraded");
        assert_eq!(cached("", "", "2.0.0"), None, "nothing cached");
        assert_eq!(
            cached("2.1.0", &format!("{url}&calc.exe"), "2.0.0"),
            None,
            "a hand-edited URL is held to the manifest's rule"
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

    // ---- R47 (docs/48 SU2): the signed download ------------------------

    /// Vectors made by the real minisign (`testdata/r47/regen.sh`) with a
    /// throwaway test key: `SHA256SUMS` over `asset.bin` named as the EXE.
    const T_KEY: &str = include_str!("../testdata/r47/test-key.pub");
    const T_SUMS: &[u8] = include_bytes!("../testdata/r47/SHA256SUMS");
    const T_ASSET: &[u8] = include_bytes!("../testdata/r47/asset.bin");
    const T_SIG: &str = include_str!("../testdata/r47/sig-2.1.0.minisig");
    const T_SIG_OLD: &str = include_str!("../testdata/r47/sig-1.9.0.minisig");
    const T_SIG_FOREIGN: &str = include_str!("../testdata/r47/sig-foreign.minisig");
    const T_SIG_OTHER_KEY: &str = include_str!("../testdata/r47/sig-other-key.minisig");

    fn key() -> &'static str {
        T_KEY.trim()
    }

    #[test]
    fn the_compiled_in_key_parses_and_is_the_release_key() {
        let pk = minisign_verify::PublicKey::from_base64(RELEASE_KEY);
        assert!(pk.is_ok(), "{pk:?}");
        let on_disk = include_str!("../../../../tools/releases/keys/gawk-release.pub");
        assert_eq!(
            on_disk.lines().nth(1),
            Some(RELEASE_KEY),
            "the key CI verifies with and the key the app trusts must be one key"
        );
    }

    #[test]
    fn a_release_signature_yields_its_signed_version() {
        assert_eq!(
            verify_release(T_SUMS, T_SIG, key(), "2.0.0"),
            Ok("2.1.0".into())
        );
        assert_eq!(
            verify_release(T_SUMS, T_SIG, key(), "2.0.0+g1a2b3c4"),
            Ok("2.1.0".into())
        );
    }

    #[test]
    fn signatures_that_must_not_install_are_refused() {
        let mut tampered = T_SUMS.to_vec();
        tampered[0] ^= 1;
        let cases: [(&str, &[u8], &str, &str); 6] = [
            ("tampered SHA256SUMS", &tampered, T_SIG, "2.0.0"),
            ("another key", T_SUMS, T_SIG_OTHER_KEY, "2.0.0"),
            ("not a desktop release", T_SUMS, T_SIG_FOREIGN, "2.0.0"),
            ("older than running", T_SUMS, T_SIG_OLD, "2.0.0"),
            ("the running release itself", T_SUMS, T_SIG, "2.1.0"),
            (
                "garbage signature",
                T_SUMS,
                "untrusted comment: x\nnope\n",
                "2.0.0",
            ),
        ];
        for (what, sums, sig, current) in cases {
            assert!(
                verify_release(sums, sig, key(), current).is_err(),
                "{what} must be refused"
            );
        }
        // And the release key does not accept the test key's signature.
        assert!(verify_release(T_SUMS, T_SIG, RELEASE_KEY, "2.0.0").is_err());
    }

    #[test]
    fn sums_lines_are_read_in_the_attach_jobs_shape() {
        let sums = std::str::from_utf8(T_SUMS).unwrap();
        let want = hex(&Sha256::digest(T_ASSET));
        assert_eq!(
            sums_entry(sums, "gawk-broadcast-windows-x86_64.exe"),
            Some(want.clone())
        );
        assert_eq!(sums_entry(sums, "missing.exe"), None);
        assert_eq!(
            sums_entry(&format!("{want} *a.exe\n"), "a.exe"),
            Some(want.clone())
        );
        assert_eq!(
            sums_entry(&format!("{want}  a.exe\n"), "a.exe"),
            Some(want.clone())
        );
        assert_eq!(
            sums_entry(&format!("{want}  ./a.exe\n{want}  ./a.exe\n"), "a.exe"),
            None,
            "listed twice is ambiguous"
        );
        assert_eq!(sums_entry("xyz  ./a.exe\n", "a.exe"), None, "not a hash");
    }

    /// A manifest listing the R47 files, as the attach job publishes it.
    fn signed_manifest(dist: &defaults::Distribution, version: &str) -> String {
        let base = format!(
            "https://github.com/Tuhis/gawk/releases/download/gawk-broadcast-desktop/v{version}"
        );
        let asset = dist.asset;
        let deb = format!("gawk-broadcast_{version}_amd64.deb");
        format!(
            r#"{{
  "asset": {{"name": "{asset}", "sha256": "{h}", "size": 16, "url": "{base}/{asset}"}},
  "assets": {{
    "{asset}": {{"sha256": "{h}", "size": 16, "url": "{base}/{asset}"}},
    "SHA256SUMS": {{"sha256": "{h}", "size": 185, "url": "{base}/SHA256SUMS"}},
    "SHA256SUMS.minisig": {{"sha256": "{h}", "size": 290, "url": "{base}/SHA256SUMS.minisig"}},
    "{deb}": {{"sha256": "{h}", "size": 9, "url": "{base}/{deb}"}}
  }},
  "component": "{name}",
  "release_url": "https://github.com/Tuhis/gawk/releases/tag/gawk-broadcast-desktop/v{version}",
  "schema": 1,
  "version": "{version}"
}}"#,
            h = "e".repeat(64),
            name = dist.name,
        )
    }

    #[test]
    fn a_signed_manifest_lists_the_files_to_install() {
        let u = evaluate(
            &signed_manifest(&defaults::WINDOWS, "2.1.0"),
            "2.0.0",
            &defaults::WINDOWS,
        )
        .unwrap();
        let f = u.files.expect("files listed");
        assert_eq!(f.asset.name, "gawk-broadcast-windows-x86_64.exe");
        assert_eq!(f.asset.size, 16);
        assert!(f.sums.url.ends_with("/v2.1.0/SHA256SUMS"));
        assert!(f.sig.url.ends_with("/v2.1.0/SHA256SUMS.minisig"));
        assert_eq!(u.deb, None, "only Linux reads the .deb");

        let l = evaluate(
            &signed_manifest(&defaults::LINUX, "2.1.0"),
            "2.0.0",
            &defaults::LINUX,
        )
        .unwrap();
        assert_eq!(l.deb.as_deref(), Some("gawk-broadcast_2.1.0_amd64.deb"));
        assert!(l.files.is_some());
    }

    /// The notice is R45's and must survive anything wrong with R47's part
    /// of the manifest; only the install goes away.
    #[test]
    fn a_bad_file_list_keeps_the_notice_and_drops_the_install() {
        let d = &defaults::WINDOWS;
        let good = signed_manifest(d, "2.1.0");
        let cases = [
            ("no signature", good.replace("\"SHA256SUMS.minisig\"", "\"x.minisig\"")),
            (
                "foreign signature url",
                good.replace(
                    "https://github.com/Tuhis/gawk/releases/download/gawk-broadcast-desktop/v2.1.0/SHA256SUMS.minisig",
                    "https://evil.example/SHA256SUMS.minisig",
                ),
            ),
            (
                "url naming another file",
                good.replace("/v2.1.0/SHA256SUMS\"", "/v2.1.0/other\""),
            ),
            (
                "oversized asset",
                good.replace(
                    r#""size": 16, "url""#,
                    r#""size": 99999999999, "url""#,
                ),
            ),
            ("assets not an object", {
                let a = good.find("\"assets\"").unwrap();
                let b = good.find("\"component\"").unwrap();
                format!("{}\"assets\": [],\n  {}", &good[..a], &good[b..])
            }),
        ];
        for (what, body) in cases {
            assert_ne!(body, good, "{what}: the mutation must change the body");
            let u = evaluate(&body, "2.0.0", d).unwrap_or_else(|| panic!("{what}: notice lost"));
            assert_eq!(u.files, None, "{what}");
        }
    }

    fn update_for(version: &str) -> Update {
        evaluate(
            &signed_manifest(&defaults::WINDOWS, version),
            "2.0.0",
            &defaults::WINDOWS,
        )
        .unwrap()
    }

    /// Serves the vectors by URL suffix; the asset body is `asset`.
    fn net(sig: &'static str, asset: &'static [u8]) -> (Box<FetchFn>, Box<DownloadFn>) {
        let fetch = move |url: &str, _limit: u64| -> Result<Vec<u8>, String> {
            if url.ends_with("/SHA256SUMS.minisig") {
                Ok(sig.as_bytes().to_vec())
            } else if url.ends_with("/SHA256SUMS") {
                Ok(T_SUMS.to_vec())
            } else {
                Err(format!("unexpected fetch {url}"))
            }
        };
        let download = move |url: &str, path: &Path, _limit: u64| -> Result<String, String> {
            assert!(url.ends_with("/gawk-broadcast-windows-x86_64.exe"), "{url}");
            std::fs::write(path, asset).map_err(|e| e.to_string())?;
            Ok(hex(&Sha256::digest(asset)))
        };
        (Box::new(fetch), Box::new(download))
    }

    #[test]
    fn a_verified_release_is_staged() {
        let dir = tempfile::tempdir().unwrap();
        let staging = dir.path().join(".gawk-update");
        let (fetch, download) = net(T_SIG, T_ASSET);
        let got = stage_with(
            &update_for("2.1.0"),
            "2.0.0",
            key(),
            &staging,
            &fetch,
            &download,
        )
        .unwrap();
        assert_eq!(got.version, "2.1.0");
        assert_eq!(got.file, staging.join("gawk-broadcast-windows-x86_64.exe"));
        assert_eq!(std::fs::read(&got.file).unwrap(), T_ASSET);
        assert!(
            !staging
                .join("gawk-broadcast-windows-x86_64.exe.part")
                .exists()
        );
    }

    /// docs/48 SU2: each refusal happens before anything could be renamed,
    /// and leaves no asset behind.
    #[test]
    fn a_release_that_fails_verification_leaves_nothing_staged() {
        let dir = tempfile::tempdir().unwrap();
        let staging = dir.path().join(".gawk-update");
        let staged = staging.join("gawk-broadcast-windows-x86_64.exe");
        let part = staging.join("gawk-broadcast-windows-x86_64.exe.part");
        let cases: [(&str, &'static str, &'static [u8], &str); 4] = [
            ("tampered asset", T_SIG, b"gawk test asset, evil\n", "2.1.0"),
            (
                "signature by another key",
                T_SIG_OTHER_KEY,
                T_ASSET,
                "2.1.0",
            ),
            ("older signed release", T_SIG_OLD, T_ASSET, "2.1.0"),
            // The manifest claims 2.2.0; the key vouched for 2.1.0.
            ("manifest version differs", T_SIG, T_ASSET, "2.2.0"),
        ];
        for (what, sig, asset, manifest_version) in cases {
            let (fetch, download) = net(sig, asset);
            let got = stage_with(
                &update_for(manifest_version),
                "2.0.0",
                key(),
                &staging,
                &fetch,
                &download,
            );
            assert!(got.is_err(), "{what}: {got:?}");
            assert!(!staged.exists() && !part.exists(), "{what}: left a file");
        }
        let no_files = Update {
            files: None,
            ..update_for("2.1.0")
        };
        let (fetch, download) = net(T_SIG, T_ASSET);
        assert!(stage_with(&no_files, "2.0.0", key(), &staging, &fetch, &download).is_err());
    }

    /// The download goes out like the check (docs/47 D2): the fixed UA, no
    /// conditional headers, and it streams to the file with its hash.
    #[test]
    fn the_download_carries_nothing_identifying() {
        let (url, rx) = serve(200, "payload".into());
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("f");
        let got = download(&url, &path, 1024).unwrap();
        assert_eq!(got, hex(&Sha256::digest(b"payload")));
        assert_eq!(std::fs::read(&path).unwrap(), b"payload");
        let head = rx.recv_timeout(Duration::from_secs(5)).unwrap();
        for line in head.lines().skip(1).filter(|l| !l.is_empty()) {
            let (k, v) = line.split_once(':').unwrap();
            let k = k.to_ascii_lowercase();
            assert!(!k.starts_with("if-") && k != "cookie", "{line}");
            if k == "user-agent" {
                assert_eq!(v.trim(), USER_AGENT);
            }
        }
    }

    #[test]
    fn a_download_over_its_size_is_refused() {
        let (url, _rx) = serve(200, "x".repeat(2048));
        let dir = tempfile::tempdir().unwrap();
        assert!(download(&url, &dir.path().join("f"), 1024).is_err());
    }
}
