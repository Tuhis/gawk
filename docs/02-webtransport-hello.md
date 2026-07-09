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

**Decision: moved to native development instead of debugging WSL further.**

## Manual browser verification (A5) — pending

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

3. Record any browser-side gotchas here.

## Wire format golden vectors (for the TS mirror, chunk B4)

From `internal/wire/wire_test.go`:

- VideoChunk (keyframe, frameID `0x01020304`, chunkIndex 5, chunkCount 130,
  timestampUs `0x5D21DBA5F0`, payload `"abc"`):
  `0101010001020304000500820000005d21dba5f0616263`
- DecoderConfig (codec `avc1.42E02A`, extradata `01 42 E0 2A FF E1`):
  `0102000b617663312e3432453032410142e02affe1`
- DecoderConfig (codec `vp8`, no extradata): `01020003767038`
