# Vendored wtransport-proto 0.7.1 + gawk patches

`wtransport-proto` 0.7.1 from crates.io (MIT OR Apache-2.0, © Biagio Festa
and contributors), vendored via `[patch.crates-io]`, with two functional
changes (sites marked `GAWK PATCH`; found against the real gawk-server —
see ../wtransport/GAWK-PATCH.md for the story):

1. **`Headers::generate_frame()` emits pseudo-headers first**
   (`src/headers.rs`). The backing store is a HashMap, so iteration order
   is arbitrary — but RFC 9114 §4.3.1 requires every pseudo-header field
   to precede regular fields, and quic-go's http3 server enforces it with
   an H3_MESSAGE_ERROR stream reset. Stock wtransport therefore could not
   send ANY additional request header (e.g. gawk's Origin) to a quic-go
   server without a coin-flip protocol violation.

2. **Unknown H3 frame types are treated as ignorable** (`src/frame.rs`,
   `FrameKind::parse` maps unknown ids to `Exercise`). RFC 9114 §9 says
   unknown frame types MUST be ignored; quic-go sends GOAWAY (0x07) on the
   control stream during shutdown, which stock wtransport turned into a
   violation-close of the whole connection.
