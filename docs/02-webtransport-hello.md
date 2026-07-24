# v0.2 — WebTransport hello-world (build step 2)

Server scaffold + TLS + echo endpoint. Implements chunks A1–A4 (and B1, the
wire format, done early since it has no dependencies) from
[`implementation-tasks.md`](implementation-tasks.md).

## What exists

- `gawk-server/` Go module: `config`, `tlsutil`, `wire`, `transport` packages,
  `gawk-server` and `gawk-devcert` binaries. See `gawk-server/README.md` for
  build/run commands.
- Automated E2E test (`internal/transport/server_test.go`): a Go
  `webtransport.Dialer` connects to `/echo` over real QUIC with an in-memory
  dev cert and round-trips a datagram. Runs in plain `go test`, no browser.

## Gotchas found (webtransport-go v0.11.1 / quic-go v0.60)

These cost real debugging time; all three failed at runtime, not compile time:

1. **ALPN must be set manually.** `webtransport.Server.ListenAndServe` passes
   `H3.TLSConfig` straight to `quic.ListenEarly` *without* applying
   `http3.ConfigureTLSConfig`, so the `h3` ALPN is missing and every handshake
   dies with `CRYPTO_ERROR 0x178: server did not select an ALPN protocol`.
   Fix: wrap the config yourself — `http3.ConfigureTLSConfig(&tls.Config{...})`.

2. **WebTransport SETTINGS must be enabled manually.** The server does not
   self-configure; without an explicit `webtransport.ConfigureHTTP3Server(s.H3)`
   call, clients reject with `server didn't enable WebTransport`. (The
   library's own tests call it; nothing in the docs mentions it.)

3. **`EnableStreamResetPartialDelivery` is required on the *client*
   `quic.Config`** (the server's `serve()` force-sets it on its own config).
   Without it the Dialer fails with `webtransport: stream reset partial
   delivery required`.

Also:

- **UDP receive buffer warning** on Linux: quic-go wants ~7 MiB, default is
  208 KiB. Harmless for the echo test; for real streaming set
  `sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000` on the host
  (worth baking into the k8s node / homelab config in Milestone D).
- The go directive was bumped to 1.25 by webtransport-go v0.11.1; the Go
  toolchain auto-downloads it (GOTOOLCHAIN=auto), no manual install needed.
- Client-side `tls.Config` for the Go Dialer needs `NextProtos: ["h3"]` too.

## CLI testing (no browser)

Two options:

- `go test ./internal/transport/` — automated E2E: in-process server, real
  QUIC echo round-trip.
- `cmd/gawk-echo` — probes a *running* server, e.g. from another terminal or
  LAN machine:

  ```sh
  go run ./cmd/gawk-server -dev-cert   # terminal 1; logs cert_hash_hex
  go run ./cmd/gawk-echo -cert-hash <cert_hash_hex>   # terminal 2
  ```

  It verifies the server cert by SHA-256 hash (same mechanism as the
  browser's `serverCertificateHashes`; omit `-cert-hash` to skip
  verification) and prints per-datagram RTT. Loopback measured ~250µs.

Note: CLI testing does not replace A5 — the browser check exists precisely to
validate Chrome's cert acceptance rules, which no Go client exercises.

## WSL2 blocks browser verification (2026-07-10)

A5 could not be completed with the server inside WSL2 (NAT mode) and Chrome
on Windows. Findings, so nobody re-debugs this:

- `https://localhost:4433` from Chrome hangs forever: WSL2's localhost
  forwarding is **TCP-only**; QUIC/UDP datagrams silently vanish.
- Using the WSL NAT IP (`hostname -I`) instead, Chrome fails with
  `QUIC_NETWORK_IDLE_TIMEOUT ... num_undecryptable_packets: 0` — it sends
  Initials and receives nothing back.
- The path itself is fine: raw UDP from Windows → WSL IP works (tested small
  and 1256-byte packets), and a **Windows-native build of gawk-echo**
  (`GOOS=windows go build ./cmd/gawk-echo`) completes the full
  QUIC + WebTransport handshake against the same WSL server. Only Chrome
  fails. No system proxy, no Chrome policies, Hyper-V firewall all defaults —
  root cause inside Chrome/WSL2-NAT interaction, not identified.
- Untried: WSL2 mirrored networking mode (`networkingMode=mirrored` in
  `.wslconfig`) may fix it, since localhost then carries UDP natively.

**Resolution (2026-07-10): ran Chrome *inside* WSL2 too, alongside the server.**
With both endpoints in the same Linux network namespace, the localhost
UDP-forwarding problem disappears and QUIC connects. This unblocked A5 —
see below.

## Manual browser verification (A5) — done (2026-07-10)

Verified with both `gawk-server` and Chrome running inside WSL2 (see the
resolution note above). Steps:

1. `go run ./cmd/gawk-server -dev-cert` (in `gawk-server/`) — the startup log
   prints the SPKI fingerprint, cert hash, and ready-to-paste Chrome flags.
2. Launch Chrome with those flags, or skip flags entirely and use
   `serverCertificateHashes` in devtools on any `https://` page:

   ```js
   const hash = Uint8Array.from("<cert_hash_hex from log>".match(/../g), b => parseInt(b, 16));
   const wt = new WebTransport("https://localhost:4433/echo", {
     serverCertificateHashes: [{ algorithm: "sha-256", value: hash }],
   });
   await wt.ready;
   const w = wt.datagrams.writable.getWriter();
   await w.write(new TextEncoder().encode("ping"));
   const r = wt.datagrams.readable.getReader();
   console.log(new TextDecoder().decode((await r.read()).value)); // "ping"
   ```

### Browser-side gotchas found

- **The dev cert's total validity span must be ≤ 14 days, backdate included.**
  Chromium's `serverCertificateHashes` verifier rejects any cert whose
  `notAfter − notBefore` exceeds 14 days with
  `ERR_QUIC_PROTOCOL_ERROR.QUIC_TLS_CERTIFICATE_UNKNOWN` /
  `CERTIFICATE_VERIFY_FAILED` (`certificate unknown`). `GenerateDevCert`
  originally added its 1-hour clock-skew backdate *on top of* the full 14-day
  validity (span = 14d + 1h), so the server's `-dev-cert` path (which requests
  `MaxDevCertValidity`) always produced a cert Chrome refused. Fixed by
  anchoring `NotAfter` to `NotBefore` so the skew hour lives inside the budget.
  The Go `gawk-echo` probe never caught this — it verifies only the hash match,
  not Chromium's 14-day span rule, so it accepted the bad cert.
- **The dev cert is ephemeral** — regenerated on every server start. Re-copy
  the fresh `cert_hash_hex` from the startup log after each restart; a stale
  hash fails the same way (`certificate unknown`).

## Wire format golden vectors (for the TS mirror, chunk B4)

From `gawk-server/wire/wire_test.go`:

- VideoChunk (keyframe, frameID `0x01020304`, chunkIndex 5, chunkCount 130,
  timestampUs `0x5D21DBA5F0`, payload `"abc"`):
  `0101010001020304000500820000005d21dba5f0616263`
- DecoderConfig (codec `avc1.42E02A`, extradata `01 42 E0 2A FF E1`):
  `0102000b617663312e3432453032410142e02affe1`
- DecoderConfig (codec `vp8`, no extradata): `01020003767038`
- StreamFrame header (R8, [docs/12](12-worker-and-reliable-keyframes.md);
  keyframe, frameID `0x01020304`, timestampUs `0x5D21DBA5F0`, configLen 6,
  payloadLen 3): `01040100010203040000005d21dba5f00000000600000003`
- TimeSync reply (R5 Q2, [docs/15](15-viewer-live-edge.md); clientTimeUs
  `0x0102030405060708`, serverTimeUs `0x090A0B0C0D0E0F10`):
  `01050102030405060708090a0b0c0d0e0f10`; request (clientTimeUs 1 000 000,
  serverTimeUs 0): `010500000000000f42400000000000000000`
- ClockMapping (R5 Q2; offsetUs +1 500 000): `0106000000000016e360`;
  offsetUs −1 000 000 (int64 two's complement): `0106fffffffffff0bdc0`

**Validation limit (added in R2, [docs/07](07-hardening.md))**: `chunkCount`
is capped at **3000** (`wire.MaxChunkCount` / `MAX_CHUNK_COUNT`, ~3.5 MB per
keyframe; the R2 draft used 1000, later raised) — the relay counts anything above it as a bad datagram to bound
keyframe-reassembly memory, and the TS side rejects it in both encode and
parse so an over-limit frame fails loudly at the broadcaster instead of
being silently dropped server-side.
