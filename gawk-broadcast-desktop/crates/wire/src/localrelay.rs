//! R74 PoP relays (docs/76 §9): LocalRelayInfo (0x18, PoP → client) and
//! LocalRelayOffer (0x19, home → client), mirroring
//! gawk-server/wire/localrelay.go, whose layout comments are the spec.
//!
//! Both ride their own server-opened uni stream after the upgrade on /echo,
//! /publish, /subscribe and /room. Like RelayIdentity, TRAILING BYTES ARE
//! TOLERATED (appended fields are the extension mechanism) while the flags
//! byte stays strict; LocalRelayInfo's state byte is tolerant (unknown bits
//! ignored). The URL and origin rules are hand checks identical in all three
//! wire copies, so no copy leans on its platform's URL parser. Clients parse
//! these and never send them; the encoders are mirrored by rule.

use crate::error::WireError;
use crate::{
    MAX_RELAY_IDENTITY_NAME_LEN, MAX_ROOM_CODE_LEN, TYPE_LOCAL_RELAY_INFO, TYPE_LOCAL_RELAY_OFFER,
    VERSION,
};

/// LocalRelayInfo state bit: the PoP is paired with and linked to its home.
pub const LOCAL_RELAY_LINKED: u8 = 0x01;
/// LocalRelayInfo state bit: this publish session's broadcast is on the wall.
pub const LOCAL_RELAY_ON_WALL: u8 = 0x02;
/// The shortest LocalRelayInfo: four header bytes, three empty fields.
pub const LOCAL_RELAY_INFO_MIN_SIZE: usize = 7;
/// Bound on LocalRelayInfo's home app origin.
pub const MAX_LOCAL_RELAY_ORIGIN_LEN: usize = 128;
/// Bound on LocalRelayOffer's opaque PoP handle.
pub const MAX_LOCAL_RELAY_HANDLE_LEN: usize = 64;
/// Bound on one LocalRelayOffer candidate URL.
pub const MAX_LOCAL_RELAY_URL_LEN: usize = 256;
/// Bound on LocalRelayOffer's candidate count.
pub const MAX_LOCAL_RELAY_CANDIDATES: usize = 4;
/// Bound on pinned SHA-256 hashes per candidate (current and next cert).
pub const MAX_LOCAL_RELAY_HASHES: usize = 2;

const LOCAL_RELAY_OFFER_MIN_SIZE: usize = 3 + 1 + 1 + 1 + 4 + 1;

/// A bounds-checked cursor; every overrun is the message's own error.
struct Cursor<'a> {
    buf: &'a [u8],
    off: usize,
    err: WireError,
}

impl<'a> Cursor<'a> {
    fn take(&mut self, n: usize) -> Result<&'a [u8], WireError> {
        if self.off + n > self.buf.len() {
            return Err(self.err.clone());
        }
        let v = &self.buf[self.off..self.off + n];
        self.off += n;
        Ok(v)
    }

    fn u8(&mut self) -> Result<u8, WireError> {
        Ok(self.take(1)?[0])
    }

    fn u16(&mut self) -> Result<u16, WireError> {
        Ok(u16::from_be_bytes(self.take(2)?.try_into().unwrap()))
    }

    fn u32(&mut self) -> Result<u32, WireError> {
        Ok(u32::from_be_bytes(self.take(4)?.try_into().unwrap()))
    }

    /// A uint8-length-prefixed UTF-8 string bounded by max.
    fn str8(&mut self, max: usize) -> Result<&'a str, WireError> {
        let n = usize::from(self.u8()?);
        if n > max {
            return Err(self.err.clone());
        }
        std::str::from_utf8(self.take(n)?).map_err(|_| self.err.clone())
    }
}

fn check_prefix(msg: &[u8], want: u8, min: usize) -> Result<(), WireError> {
    if msg.len() < min {
        return Err(WireError::ShortDatagram {
            len: msg.len(),
            need: min,
        });
    }
    if msg[0] != VERSION {
        return Err(WireError::BadVersion(msg[0]));
    }
    if msg[1] != want {
        return Err(WireError::BadType { got: msg[1], want });
    }
    Ok(())
}

fn url_bytes_ok(s: &str) -> bool {
    s.bytes().all(|b| (0x21..=0x7e).contains(&b))
}

/// A host[:port] authority: a non-empty host, either a bracketed IPv6
/// literal or a run with no ':' or brackets; a port, when present, is 1–5
/// digits valued 1–65535. Returns the host. Mirrors Go
/// splitLocalRelayAuthority.
fn split_local_relay_authority(auth: &str) -> Option<&str> {
    let (host, port) = if auth.starts_with('[') {
        let end = auth.find(']')?;
        if end < 2 || auth[1..end].contains('[') {
            return None;
        }
        let rest = &auth[end + 1..];
        let port = if rest.is_empty() {
            None
        } else {
            Some(rest.strip_prefix(':')?)
        };
        (&auth[..=end], port)
    } else {
        let (host, port) = match auth.find(':') {
            Some(i) => (&auth[..i], Some(&auth[i + 1..])),
            None => (auth, None),
        };
        if host.is_empty() || host.contains(['[', ']']) {
            return None;
        }
        (host, port)
    };
    if let Some(p) = port {
        if p.is_empty() || p.len() > 5 || !p.bytes().all(|b| b.is_ascii_digit()) {
            return None;
        }
        let n: u32 = p.parse().ok()?;
        if n == 0 || n > 65535 {
            return None;
        }
    }
    Some(host)
}

/// The home-origin rule: empty, or https://host[:port] with no userinfo,
/// path, query or fragment, or http://localhost[:port] /
/// http://127.0.0.1[:port].
fn valid_local_relay_origin(origin: &str) -> bool {
    if origin.is_empty() {
        return true;
    }
    if origin.len() > MAX_LOCAL_RELAY_ORIGIN_LEN || !url_bytes_ok(origin) {
        return false;
    }
    let (auth, loopback_only) = if let Some(a) = origin.strip_prefix("https://") {
        (a, false)
    } else if let Some(a) = origin.strip_prefix("http://") {
        (a, true)
    } else {
        return false;
    };
    if auth.contains(['/', '?', '#', '@']) {
        return false;
    }
    match split_local_relay_authority(auth) {
        None => false,
        Some(host) => !loopback_only || host == "localhost" || host == "127.0.0.1",
    }
}

/// The candidate-URL rule: printable ASCII, "https://", a non-empty
/// host[:port] authority with no userinfo, an optional path, no query or
/// fragment.
fn valid_local_relay_url(url: &str) -> bool {
    if url.is_empty() || url.len() > MAX_LOCAL_RELAY_URL_LEN || !url_bytes_ok(url) {
        return false;
    }
    let Some(rest) = url.strip_prefix("https://") else {
        return false;
    };
    if url.contains(['?', '#']) {
        return false;
    }
    let auth = rest.split('/').next().unwrap_or("");
    !auth.contains('@') && split_local_relay_authority(auth).is_some()
}

fn valid_local_relay_handle(handle: &str) -> bool {
    !handle.is_empty()
        && handle.len() <= MAX_LOCAL_RELAY_HANDLE_LEN
        && handle.bytes().all(|b| (0x20..=0x7e).contains(&b))
}

// --- LocalRelayInfo (0x18) ---------------------------------------------------

/// The parsed contents of a LocalRelayInfo message, borrowing the input.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct LocalRelayInfo<'a> {
    /// The PoP is linked to its home fleet.
    pub linked: bool,
    /// This publish session's broadcast is on the party wall.
    pub on_wall: bool,
    /// The PoP's display name. Attacker-influenced trust UI: render
    /// sanitized.
    pub name: &'a str,
    /// The home fleet's app origin; empty when unlinked.
    pub home_origin: &'a str,
    /// The party wall's room code; empty when there is none.
    pub wall_code: &'a str,
}

impl<'a> LocalRelayInfo<'a> {
    /// Appends this message to `dst`.
    pub fn encode(&self, dst: &mut Vec<u8>) -> Result<(), WireError> {
        append_local_relay_info(dst, self)
    }

    /// Parses a LocalRelayInfo message.
    pub fn parse(msg: &'a [u8]) -> Result<Self, WireError> {
        parse_local_relay_info(msg)
    }
}

/// Appends a LocalRelayInfo message. A `&str` is UTF-8 by construction, so
/// only lengths and the origin rule can fail.
pub fn append_local_relay_info(dst: &mut Vec<u8>, m: &LocalRelayInfo<'_>) -> Result<(), WireError> {
    if m.name.len() > MAX_RELAY_IDENTITY_NAME_LEN
        || m.wall_code.len() > MAX_ROOM_CODE_LEN
        || !valid_local_relay_origin(m.home_origin)
    {
        return Err(WireError::BadLocalRelayInfo);
    }
    let mut state = 0;
    if m.linked {
        state |= LOCAL_RELAY_LINKED;
    }
    if m.on_wall {
        state |= LOCAL_RELAY_ON_WALL;
    }
    dst.extend_from_slice(&[VERSION, TYPE_LOCAL_RELAY_INFO, 0, state]);
    for s in [m.name, m.home_origin, m.wall_code] {
        dst.push(s.len() as u8);
        dst.extend_from_slice(s.as_bytes());
    }
    Ok(())
}

/// Parses a LocalRelayInfo message. Trailing bytes and unknown state bits
/// are ignored; the flags byte is strict.
pub fn parse_local_relay_info(msg: &[u8]) -> Result<LocalRelayInfo<'_>, WireError> {
    check_prefix(msg, TYPE_LOCAL_RELAY_INFO, LOCAL_RELAY_INFO_MIN_SIZE)?;
    let mut r = Cursor {
        buf: msg,
        off: 2,
        err: WireError::BadLocalRelayInfo,
    };
    if r.u8()? != 0 {
        return Err(WireError::BadLocalRelayInfo);
    }
    let state = r.u8()?;
    let name = r.str8(MAX_RELAY_IDENTITY_NAME_LEN)?;
    let home_origin = r.str8(MAX_LOCAL_RELAY_ORIGIN_LEN)?;
    let wall_code = r.str8(MAX_ROOM_CODE_LEN)?;
    // Bytes past the wall code are the extension space — ignored.
    if !valid_local_relay_origin(home_origin) {
        return Err(WireError::BadLocalRelayInfo);
    }
    Ok(LocalRelayInfo {
        linked: state & LOCAL_RELAY_LINKED != 0,
        on_wall: state & LOCAL_RELAY_ON_WALL != 0,
        name,
        home_origin,
        wall_code,
    })
}

// --- LocalRelayOffer (0x19) --------------------------------------------------

/// One URL a client may probe, with the SHA-256 certificate hashes to pin.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LocalRelayCandidate<'a> {
    pub url: &'a str,
    pub hashes: Vec<&'a [u8; 32]>,
}

/// The parsed contents of a LocalRelayOffer message, borrowing the input.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LocalRelayOffer<'a> {
    /// The opaque PoP handle (the Pop CR's metadata.uid); opt-outs key on it.
    pub handle: &'a str,
    /// The PoP's display name.
    pub name: &'a str,
    /// Seconds the offer may be acted on, ≥ 1.
    pub validity: u32,
    /// In the sender's preference order.
    pub candidates: Vec<LocalRelayCandidate<'a>>,
}

impl<'a> LocalRelayOffer<'a> {
    /// Appends this message to `dst`.
    pub fn encode(&self, dst: &mut Vec<u8>) -> Result<(), WireError> {
        append_local_relay_offer(dst, self)
    }

    /// Parses a LocalRelayOffer message.
    pub fn parse(msg: &'a [u8]) -> Result<Self, WireError> {
        parse_local_relay_offer(msg)
    }
}

/// Appends a LocalRelayOffer message.
pub fn append_local_relay_offer(
    dst: &mut Vec<u8>,
    m: &LocalRelayOffer<'_>,
) -> Result<(), WireError> {
    let bad = Err(WireError::BadLocalRelayOffer);
    if !valid_local_relay_handle(m.handle)
        || m.name.len() > MAX_RELAY_IDENTITY_NAME_LEN
        || m.validity == 0
        || m.candidates.is_empty()
        || m.candidates.len() > MAX_LOCAL_RELAY_CANDIDATES
    {
        return bad;
    }
    for c in &m.candidates {
        if !valid_local_relay_url(c.url) || c.hashes.len() > MAX_LOCAL_RELAY_HASHES {
            return bad;
        }
    }
    dst.extend_from_slice(&[VERSION, TYPE_LOCAL_RELAY_OFFER, 0, m.handle.len() as u8]);
    dst.extend_from_slice(m.handle.as_bytes());
    dst.push(m.name.len() as u8);
    dst.extend_from_slice(m.name.as_bytes());
    dst.extend_from_slice(&m.validity.to_be_bytes());
    dst.push(m.candidates.len() as u8);
    for c in &m.candidates {
        dst.extend_from_slice(&(c.url.len() as u16).to_be_bytes());
        dst.extend_from_slice(c.url.as_bytes());
        dst.push(c.hashes.len() as u8);
        for h in &c.hashes {
            dst.extend_from_slice(&h[..]);
        }
    }
    Ok(())
}

/// Parses a LocalRelayOffer message. Trailing bytes are ignored; the flags
/// byte is strict and every declared field must fit and validate.
pub fn parse_local_relay_offer(msg: &[u8]) -> Result<LocalRelayOffer<'_>, WireError> {
    check_prefix(msg, TYPE_LOCAL_RELAY_OFFER, LOCAL_RELAY_OFFER_MIN_SIZE)?;
    let bad = WireError::BadLocalRelayOffer;
    let mut r = Cursor {
        buf: msg,
        off: 2,
        err: bad.clone(),
    };
    if r.u8()? != 0 {
        return Err(bad);
    }
    let handle_len = usize::from(r.u8()?);
    if handle_len > MAX_LOCAL_RELAY_HANDLE_LEN {
        return Err(bad);
    }
    let handle = std::str::from_utf8(r.take(handle_len)?).map_err(|_| bad.clone())?;
    if !valid_local_relay_handle(handle) {
        return Err(bad);
    }
    let name = r.str8(MAX_RELAY_IDENTITY_NAME_LEN)?;
    let validity = r.u32()?;
    if validity == 0 {
        return Err(bad);
    }
    let count = usize::from(r.u8()?);
    if count == 0 || count > MAX_LOCAL_RELAY_CANDIDATES {
        return Err(bad);
    }
    let mut candidates = Vec::with_capacity(count);
    for _ in 0..count {
        let url_len = usize::from(r.u16()?);
        if url_len == 0 || url_len > MAX_LOCAL_RELAY_URL_LEN {
            return Err(bad);
        }
        let url = std::str::from_utf8(r.take(url_len)?).map_err(|_| bad.clone())?;
        if !valid_local_relay_url(url) {
            return Err(bad);
        }
        let hash_count = usize::from(r.u8()?);
        if hash_count > MAX_LOCAL_RELAY_HASHES {
            return Err(bad);
        }
        let mut hashes = Vec::with_capacity(hash_count);
        for _ in 0..hash_count {
            hashes.push(<&[u8; 32]>::try_from(r.take(32)?).unwrap());
        }
        candidates.push(LocalRelayCandidate { url, hashes });
    }
    // Bytes past the last candidate are the extension space — ignored.
    Ok(LocalRelayOffer {
        handle,
        name,
        validity,
        candidates,
    })
}
