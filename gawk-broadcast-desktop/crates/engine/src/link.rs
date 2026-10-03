//! `gawk://` links (R66, docs/68 D1, D2): one grammar for every native app.
//!
//! ```text
//! gawk://broadcast[/][?room=<code>][&nick=<nickname>][&relay=<https-origin>]
//! gawk://watch/<BROADCAST-ID>[?relay=<https-origin>]
//! gawk://room/<code>[?nick=<nickname>][&relay=<https-origin>]
//! ```
//!
//! A link is untrusted input from any web page (D13), so this module only
//! reads it: what the app does with it is the shell's business, and nothing
//! here can start, capture or carry a secret. The golden vectors in
//! `tests/link-vectors.json` pin the parser and both builders; gawk-app's
//! builder (R67) and the iOS app (R65) restate them, never import them.

use crate::config;
use crate::defaults;
use crate::room::truncate_utf8;
use serde::{Deserialize, Serialize};

/// A link longer than this is rejected before anything reads it (D1).
pub const MAX_LINK_LEN: usize = 2048;

/// Shortest static room slug, as gawk-app's `MIN_ROOM_SLUG_LEN`.
pub const MIN_ROOM_SLUG_LEN: usize = 3;

/// What a link asks for. `relay` is the normalized origin of a server
/// other than the default fleet; `None` means the default fleet, never
/// "whatever this app has selected" (D1).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "lowercase")]
pub enum Link {
    Broadcast {
        room: Option<String>,
        nick: Option<String>,
        relay: Option<String>,
    },
    Watch {
        id: String,
        relay: Option<String>,
    },
    Room {
        code: String,
        nick: Option<String>,
        relay: Option<String>,
    },
}

/// Why a parameter was left out. The link still applies without it.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum DropReason {
    /// A credential-shaped parameter: secrets never travel in links.
    Secret,
    /// A parameter this grammar doesn't have, or not on this path.
    Unknown,
    /// A known parameter with an unusable value, or a repeat of one.
    Invalid,
}

/// One parameter that was dropped, by name only: its value is never kept,
/// since it may be a secret.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Dropped {
    pub param: String,
    pub why: DropReason,
}

/// A parsed link, and what was left out of it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Parsed {
    pub link: Link,
    pub dropped: Vec<Dropped>,
}

/// Why a whole link was rejected.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LinkError {
    /// Over [`MAX_LINK_LEN`] bytes.
    TooLong,
    /// Not `gawk://…`.
    NotGawk,
    /// A path this grammar doesn't have.
    UnknownPath,
    /// `watch/` without a valid broadcast code.
    BadId,
    /// `room/` without a valid room code.
    BadCode,
}

impl LinkError {
    /// The stable name the vectors use.
    pub fn code(self) -> &'static str {
        match self {
            LinkError::TooLong => "too-long",
            LinkError::NotGawk => "not-gawk",
            LinkError::UnknownPath => "unknown-path",
            LinkError::BadId => "bad-id",
            LinkError::BadCode => "bad-code",
        }
    }
}

impl std::fmt::Display for LinkError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            LinkError::TooLong => "the link is too long",
            LinkError::NotGawk => "it isn't a gawk:// link",
            LinkError::UnknownPath => "this app doesn't know what it asks for",
            LinkError::BadId => "its broadcast code isn't valid",
            LinkError::BadCode => "its room code isn't valid",
        })
    }
}

impl std::error::Error for LinkError {}

/// Reads a `gawk://` link (D1). An invalid path or ID rejects the whole
/// link; an unusable, unknown or secret parameter is only dropped. Never
/// panics, whatever the input.
pub fn parse(raw: &str) -> Result<Parsed, LinkError> {
    if raw.len() > MAX_LINK_LEN {
        return Err(LinkError::TooLong);
    }
    let s = raw.trim();
    let rest = strip_prefix_ignore_case(s, "gawk://").ok_or(LinkError::NotGawk)?;
    // A fragment means nothing in this grammar.
    let rest = rest.split('#').next().unwrap_or("");
    let (path, query) = match rest.split_once('?') {
        Some((p, q)) => (p, q),
        None => (rest, ""),
    };
    let path = path.strip_suffix('/').unwrap_or(path);
    let (kind, arg) = match path.split_once('/') {
        Some((k, a)) => (k, Some(a)),
        None => (path, None),
    };
    let kind = kind.to_ascii_lowercase();
    let allowed: &[&str] = match (kind.as_str(), arg) {
        ("broadcast", None) => &["room", "nick", "relay"],
        ("watch", Some(_)) => &["relay"],
        ("room", Some(_)) => &["nick", "relay"],
        ("watch", None) => return Err(LinkError::BadId),
        ("room", None) => return Err(LinkError::BadCode),
        _ => return Err(LinkError::UnknownPath),
    };

    let mut dropped = Vec::new();
    let params = read_query(query, allowed, &mut dropped);
    let relay = params.relay;
    let nick = params.nick;
    let link = match kind.as_str() {
        "broadcast" => Link::Broadcast {
            room: params.room,
            nick,
            relay,
        },
        "watch" => {
            let id = arg
                .and_then(|a| gawk_wire::normalize_broadcast_id(a.as_bytes()))
                .ok_or(LinkError::BadId)?;
            Link::Watch { id, relay }
        }
        _ => {
            let code = arg
                .filter(|a| is_valid_room_code(a))
                .ok_or(LinkError::BadCode)?;
            Link::Room {
                code: code.to_owned(),
                nick,
                relay,
            }
        }
    };
    Ok(Parsed { link, dropped })
}

/// What the query held, each value already validated.
#[derive(Default)]
struct Params {
    room: Option<String>,
    nick: Option<String>,
    relay: Option<String>,
}

fn read_query(query: &str, allowed: &[&str], dropped: &mut Vec<Dropped>) -> Params {
    let mut out = Params::default();
    let mut seen: Vec<String> = Vec::new();
    for pair in query.split('&').filter(|p| !p.is_empty()) {
        let (k, v) = pair.split_once('=').unwrap_or((pair, ""));
        let mut drop = |param: &str, why| {
            dropped.push(Dropped {
                param: param.to_owned(),
                why,
            })
        };
        // A key that doesn't decode is still named by what was written.
        let Some(key) = decode(k) else {
            drop(k, DropReason::Unknown);
            continue;
        };
        if is_secret_shaped(&key) {
            drop(&key, DropReason::Secret);
            continue;
        }
        if !allowed.contains(&key.as_str()) {
            drop(&key, DropReason::Unknown);
            continue;
        }
        if seen.contains(&key) {
            drop(&key, DropReason::Invalid);
            continue;
        }
        seen.push(key.clone());
        let Some(value) = decode(v) else {
            drop(&key, DropReason::Invalid);
            continue;
        };
        // Blank counts as absent, and says nothing.
        if value.trim().is_empty() {
            continue;
        }
        let ok = match key.as_str() {
            "room" => is_valid_room_code(&value).then(|| out.room = Some(value)),
            "nick" => {
                let clean = sanitize_nickname(&value);
                (!clean.is_empty()).then(|| out.nick = Some(clean))
            }
            _ => config::relay_origin(&value).map(|origin| {
                // The default fleet is no `relay=` at all: canonical either way.
                if Some(&origin) != default_origin().as_ref() {
                    out.relay = Some(origin);
                }
            }),
        };
        if ok.is_none() {
            drop(&key, DropReason::Invalid);
        }
    }
    out
}

fn default_origin() -> Option<String> {
    config::relay_origin(defaults::RELAY_URL)
}

/// Credential-shaped names (D1): the room grant `rt`, and anything that
/// reads like a secret, token, key or password, in any case.
fn is_secret_shaped(key: &str) -> bool {
    let k = key.to_ascii_lowercase();
    k == "rt"
        || ["secret", "token", "key", "pass", "auth", "grant", "cred"]
            .iter()
            .any(|w| k.contains(w))
}

/// Percent-decodes a query component as UTF-8, `+` as a space (what the
/// browser's `URLSearchParams` does). `None` for a bad escape or bytes that
/// aren't UTF-8 — never a lossy guess.
fn decode(s: &str) -> Option<String> {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        match b[i] {
            b'+' => out.push(b' '),
            b'%' => {
                let hex = b.get(i + 1..i + 3)?;
                let hex = std::str::from_utf8(hex).ok()?;
                out.push(u8::from_str_radix(hex, 16).ok()?);
                i += 2;
            }
            c => out.push(c),
        }
        i += 1;
    }
    String::from_utf8(out).ok()
}

/// gawk-app's `isValidRoomCode`: a 3–32 character `[A-Za-z0-9-]` slug, or
/// a broadcast ID (six alphabet characters, which the slug rule already
/// admits). Case is kept.
pub fn is_valid_room_code(code: &str) -> bool {
    (MIN_ROOM_SLUG_LEN..=gawk_wire::MAX_ROOM_CODE_LEN).contains(&code.len())
        && code.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')
}

/// JavaScript's `\s` and `trim()` set: Unicode White_Space without U+0085,
/// plus U+FEFF.
fn is_js_space(c: char) -> bool {
    (c.is_whitespace() && c != '\u{85}') || c == '\u{feff}'
}

/// gawk-app's `sanitizeNickname`: whitespace runs collapsed to one space,
/// trimmed, cut to `MAX_ROOM_NICKNAME_LEN` bytes on a character boundary,
/// trimmed again.
pub fn sanitize_nickname(raw: &str) -> String {
    let mut collapsed = String::with_capacity(raw.len());
    let mut in_space = false;
    for c in raw.trim_matches(is_js_space).chars() {
        if is_js_space(c) {
            if !in_space {
                collapsed.push(' ');
            }
            in_space = true;
        } else {
            collapsed.push(c);
            in_space = false;
        }
    }
    truncate_utf8(&collapsed, gawk_wire::MAX_ROOM_NICKNAME_LEN)
        .trim_matches(is_js_space)
        .to_owned()
}

fn strip_prefix_ignore_case<'a>(s: &'a str, prefix: &str) -> Option<&'a str> {
    let head = s.get(..prefix.len())?;
    head.eq_ignore_ascii_case(prefix)
        .then(|| &s[prefix.len()..])
}

/// JavaScript's `encodeURIComponent`, so gawk-app's builder can match
/// [`Link::to_gawk`] byte for byte (docs/69 D3).
fn encode_component(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for &b in s.as_bytes() {
        if b.is_ascii_alphanumeric() || b"-_.!~*'()".contains(&b) {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

/// `?room=…&nick=…&relay=…` in that order, the absent ones left out; empty
/// when all are.
fn query(pairs: &[(&str, &Option<String>)]) -> String {
    let parts: Vec<String> = pairs
        .iter()
        .filter_map(|(k, v)| v.as_ref().map(|v| format!("{k}={}", encode_component(v))))
        .collect();
    if parts.is_empty() {
        String::new()
    } else {
        format!("?{}", parts.join("&"))
    }
}

impl Link {
    /// The canonical `gawk://` form: lowercase path, parameters in the
    /// grammar's order, `relay` only for a server other than the default.
    pub fn to_gawk(&self) -> String {
        format!("gawk://{}", self.path_and_query())
    }

    /// The same intent as a gawk-app page on `app_url`: `#/broadcast?…`,
    /// `#/view/<ID>?…` or `#/room/<code>?…`.
    pub fn to_https(&self, app_url: &str) -> String {
        let base = app_url.trim_end_matches('/');
        match self {
            Link::Watch { id, relay } => {
                format!("{base}/#/view/{id}{}", query(&[("relay", relay)]))
            }
            _ => format!("{base}/#/{}", self.path_and_query()),
        }
    }

    fn path_and_query(&self) -> String {
        match self {
            Link::Broadcast { room, nick, relay } => format!(
                "broadcast{}",
                query(&[("room", room), ("nick", nick), ("relay", relay)])
            ),
            Link::Watch { id, relay } => format!("watch/{id}{}", query(&[("relay", relay)])),
            Link::Room { code, nick, relay } => {
                format!("room/{code}{}", query(&[("nick", nick), ("relay", relay)]))
            }
        }
    }

    /// The kind alone — what a log line may say about a link (D13: no raw
    /// broadcast IDs in logs).
    pub fn kind(&self) -> &'static str {
        match self {
            Link::Broadcast { .. } => "broadcast",
            Link::Watch { .. } => "watch",
            Link::Room { .. } => "room",
        }
    }
}

/// Finds the one `gawk:` argument in a command line (D7): Windows passes
/// the link as `"%1"`, the Linux desktop entry as `%u`. Everything else is
/// ignored. `None` when there is none; `Some(Err(()))` when there are two
/// or more, which rejects the launch's link.
pub fn link_argument<S: AsRef<str>>(args: &[S]) -> Option<Result<String, ()>> {
    let mut found = args.iter().map(AsRef::as_ref).filter(|a| is_gawk_link(a));
    let first = found.next()?;
    if found.next().is_some() {
        return Some(Err(()));
    }
    Some(Ok(first.to_owned()))
}

/// True when `s` has the `gawk:` scheme, in any case.
pub fn is_gawk_link(s: &str) -> bool {
    strip_prefix_ignore_case(s.trim(), "gawk:").is_some()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// One golden vector (`tests/link-vectors.json`).
    #[derive(Deserialize)]
    struct Vector {
        name: String,
        input: String,
        #[serde(default)]
        link: Option<Link>,
        #[serde(default)]
        error: Option<String>,
        #[serde(default)]
        dropped: Vec<Dropped>,
        #[serde(default)]
        gawk: Option<String>,
        #[serde(default)]
        https: Option<String>,
    }

    #[derive(Deserialize)]
    #[serde(rename_all = "camelCase")]
    struct Vectors {
        app_url: String,
        vectors: Vec<Vector>,
    }

    fn vectors() -> Vectors {
        serde_json::from_str(include_str!("../tests/link-vectors.json")).expect("vectors parse")
    }

    #[test]
    fn the_golden_vectors_pass() {
        let v = vectors();
        assert!(v.vectors.len() > 30);
        for t in &v.vectors {
            match parse(&t.input) {
                Ok(p) => {
                    assert!(t.error.is_none(), "{}: parsed, want {:?}", t.name, t.error);
                    assert_eq!(Some(&p.link), t.link.as_ref(), "{}", t.name);
                    assert_eq!(p.dropped, t.dropped, "{}", t.name);
                    let gawk = p.link.to_gawk();
                    assert_eq!(Some(&gawk), t.gawk.as_ref(), "{}", t.name);
                    assert_eq!(
                        Some(&p.link.to_https(&v.app_url)),
                        t.https.as_ref(),
                        "{}",
                        t.name
                    );
                    // The canonical form is a fixed point.
                    let again = parse(&gawk).unwrap_or_else(|e| panic!("{}: {e}", t.name));
                    assert_eq!(again.link, p.link, "{}: canonical re-parse", t.name);
                    assert!(again.dropped.is_empty(), "{}", t.name);
                }
                Err(e) => {
                    assert_eq!(Some(e.code()), t.error.as_deref(), "{}", t.name);
                    assert!(t.link.is_none(), "{}", t.name);
                }
            }
        }
    }

    #[test]
    fn the_vectors_name_every_rule_both_ways() {
        let names: Vec<String> = vectors().vectors.into_iter().map(|v| v.name).collect();
        // Every D1 rule has an accepting and a rejecting vector; their names
        // carry the rule so a rule losing either side is a visible gap.
        for rule in [
            "case", "alphabet", "slug", "nick", "relay", "secret", "unknown", "length",
        ] {
            for side in ["accept", "reject"] {
                let tag = format!("{rule}/{side}");
                assert!(
                    names.iter().any(|n| n.starts_with(&tag)),
                    "no vector named {tag}…"
                );
            }
        }
    }

    /// G7: truncated and mutated vectors never panic, and whatever parses
    /// round-trips through the canonical form.
    #[test]
    fn garbage_never_panics() {
        let inputs: Vec<String> = vectors().vectors.into_iter().map(|v| v.input).collect();
        let subs: [&str; 12] = [
            "%", "%E2%82", "\u{0}", "?", "&", "/", "#", "=", "é", "%FF", "\u{feff}", "+",
        ];
        for s in &inputs {
            let chars: Vec<(usize, char)> = s.char_indices().collect();
            for &(i, _) in &chars {
                for cut in [&s[..i], &s[i..]] {
                    check(cut);
                }
                for sub in subs {
                    let mut m = s.clone();
                    m.insert_str(i, sub);
                    check(&m);
                    let mut r = s.clone();
                    let end = chars
                        .iter()
                        .find(|(j, _)| *j > i)
                        .map_or(s.len(), |(j, _)| *j);
                    r.replace_range(i..end, sub);
                    check(&r);
                }
            }
        }
        check(&format!(
            "gawk://broadcast?nick={}",
            "x".repeat(MAX_LINK_LEN)
        ));
        check(&"%".repeat(MAX_LINK_LEN));
    }

    fn check(input: &str) {
        if let Ok(p) = parse(input) {
            let again = parse(&p.link.to_gawk()).expect("canonical form parses");
            assert_eq!(again.link, p.link, "{input:?}");
        }
    }

    #[test]
    fn the_link_argument_is_the_one_gawk_argument() {
        let none: [&str; 2] = ["gawk-broadcast", "--relaunched-from"];
        assert_eq!(link_argument(&none), None);
        assert_eq!(
            link_argument(&["app", "GAWK://broadcast?room=abc"]),
            Some(Ok("GAWK://broadcast?room=abc".to_string()))
        );
        assert_eq!(
            link_argument(&["app", "gawk://broadcast", "gawk://room/abc"]),
            Some(Err(()))
        );
        assert!(!is_gawk_link("gawk-broadcast://windows"));
        assert!(is_gawk_link(" gawk:x"));
    }

    #[test]
    fn nicknames_sanitize_like_the_web() {
        assert_eq!(sanitize_nickname("  a \t\n b  "), "a b");
        assert_eq!(sanitize_nickname("\u{feff}x\u{a0}y"), "x y");
        // 32 bytes, cut on a character boundary, then trimmed again.
        let long = format!("{}é", "x".repeat(31));
        assert_eq!(sanitize_nickname(&long), "x".repeat(31));
        let spaced = format!("{} y", "x".repeat(31));
        assert_eq!(sanitize_nickname(&spaced), "x".repeat(31));
        assert_eq!(sanitize_nickname("   "), "");
    }
}
