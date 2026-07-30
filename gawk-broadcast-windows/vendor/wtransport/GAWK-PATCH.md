# Vendored wtransport 0.7.1 + gawk patches

This is `wtransport` 0.7.1 from crates.io (MIT OR Apache-2.0, © Biagio
Festa and contributors — license declared in Cargo.toml), vendored via
`[patch.crates-io]` in the workspace root, with three functional changes.
All were found by running the WB2 integration tests against the real
gawk-server (webtransport-go) and are candidates for upstreaming; every
site is marked `GAWK PATCH` in the source. Sibling patches live in
`../wtransport-proto/GAWK-PATCH.md`; the integration tests in
`crates/engine/tests/relay_integration.rs` are what keep all of them
honest (docs/38 D2/D18).

1. **`ConnectingError::SessionRejected` carries the HTTP status** (`u16`; 0
   when the request stream ended before a response). Upstream parses the
   rejection status and discards it; gawk needs it because 401/403/404/409
   (terminal for resume) vs 429/5xx (retryable) and the shells' curated
   start-error messages key off it — docs/38 D2 gate 2a. Sites:
   `src/error.rs` (the variant), `src/endpoint.rs` (two constructions).

2. **`Connection::closed()` prefers the driver's session-level close.**
   When the peer ends the SESSION with a CLOSE_WEBTRANSPORT_SESSION
   capsule, the driver records it and then locally closes the QUIC
   connection — so the quinn-level cause is `LocallyClosed` and the
   application close code (4000/4002/4004) was lost. Sites:
   `src/connection.rs` (`closed()`), `src/driver/mod.rs`
   (`terminal_error()` accessor).

3. **The CONNECT-stream capsule reader reassembles across DATA frames**
   (`src/driver/streams/connect.rs`, rewritten). Capsules are a byte
   stream carried in DATA frames (RFC 9297 §3.1) and webtransport-go
   routinely splits one capsule's header and value into two DATA frames;
   stock wtransport parsed each DATA frame as exactly one complete capsule
   and silently skipped everything else — losing every session close code
   the gawk relay sends. Unknown frames on the stream are now skipped per
   RFC 9114 §9 instead of feeding the capsule parser.

When bumping wtransport: re-apply (or hopefully drop) these; the
integration suite fails loudly if any regresses.
