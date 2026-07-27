# Security Policy

`gawk` is a self-hosted, low-latency game-streaming stack (a Go relay, a web
app, a native Linux broadcaster, and an optional per-session diagnostics
service). It is maintained by a single person as a side project, on a
best-effort basis. This document explains how to report a vulnerability and
what you can reasonably expect in return.

## Threat model (read this first)

`gawk` is built for a **self-hosted deployment run by a known operator** — a
homelab or a single-tenant cluster — not as a hardened, multi-tenant service on
the open internet. Some robustness is deliberately traded for a leaner,
self-owned pipeline. In particular:

- **Broadcast IDs are join secrets, not access control.** Anyone who knows a
  6-character code can watch that broadcast. IDs are short (~31⁶) and are
  intended to be shared out-of-band. Treat them as unlisted-link secrecy, not
  authentication.
- **There are no user accounts.** Publishing can be gated with a pre-shared
  secret (`-publish-secret`) and, in a fleet, with resume-token keys
  (`resumeTokenKey`); viewing is intentionally open to anyone with the code.
- **We do not owe a hostile-network SLA.** The relay has hardening knobs
  (concurrent-broadcast / subscriber / per-IP-rate / egress-bandwidth caps —
  see [`docs/07-hardening.md`](docs/07-hardening.md)), but the design assumes a
  cooperative environment with bandwidth headroom, not an adversarial public
  endpoint.
- **The relay is a byte forwarder.** It parses wire headers only to observe
  (cache keyframe/config, prime late joiners) and does not decode media.

**In scope** — issues that break these assumptions or the protections that *are*
promised, for example:

- Remote crash, panic, or unbounded resource exhaustion in the relay reachable
  from a single connection or a small number of them (beyond the documented
  caps).
- Bypass of the publish secret, resume-token gate, or the "newest publisher
  wins" takeover fencing (e.g. hijacking or terminating another operator's
  broadcast without the token).
- Leaking raw broadcast IDs or other identifiers that `/statusz` is designed to
  obfuscate (it keys broadcasts by a per-process HMAC — raw IDs are joinable).
- Memory-safety / parsing bugs in the wire-format decoders reachable from
  untrusted network input.
- Exposure of the ops/metrics endpoint or `-publish-secret` to parties who
  should not have them (e.g. via the public LoadBalancer).
- Exposure of `gawk-telemetry`'s dashboard, read API, or MCP surface — these
  are designed to run on a listener that is never routed publicly, separate
  from the public ingest path.
- Supply-chain issues in the build/release path (CI, Helm charts, GHCR images).

**Out of scope** — expected behavior given the threat model, not vulnerabilities:

- Viewing a broadcast whose code you know, or guessing a code you were not given
  (mitigate by keeping codes unlisted; rotate by restarting the broadcast).
- Denial of service that only succeeds by exceeding the documented rate/
  bandwidth caps, or that requires network-position abuse (packet floods,
  L3/L4 volumetric attacks) against a self-hosted UDP endpoint.
- Absence of end-to-end media encryption beyond QUIC/TLS transport security.
- Anything requiring a malicious operator, physical access to the host, or a
  compromised broadcaster machine.
- Findings in third-party dependencies with no demonstrated impact on `gawk`
  (please still tell us, but severity is assessed by real impact).

If you are unsure whether something is in scope, report it anyway and let the
maintainer make the call.

## Supported versions

This is a solo project, so security fixes land on the **latest release of each
component only**. The four components version independently
([SemVer](https://semver.org/); see `.release-please-manifest.json`):

| Component         | Supported            |
|-------------------|----------------------|
| `gawk-server`     | Latest release only  |
| `gawk-app`        | Latest release only  |
| `gawk-broadcast`  | Latest release only  |
| `gawk-telemetry`  | Latest release only  |

Older tags do not receive backports. If you run a pinned version, plan to
upgrade to pick up a fix. If a fix ever needs to reach an older line, that will
be decided case by case at report time.

## Reporting a vulnerability

**Please do not open a public issue, pull request, or discussion for a
suspected vulnerability.** Report it privately through **GitHub private
vulnerability reporting** — the **Security → Report a vulnerability** button,
which opens a private [GitHub Security Advisory](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
draft visible only to you and the maintainer.

(This requires the repository to be public with private reporting enabled. If
it is not yet available, please wait for it rather than disclosing publicly.)

To help triage quickly, please include what you can:

- The affected component and version (relay / app / broadcaster / telemetry).
- A description of the issue and its impact under the threat model above.
- Reproduction steps or a proof of concept (a minimal repro, config, or the
  relevant wire bytes are ideal).
- Any relevant deployment details (single-pod vs. cluster mode, whether a
  publish secret / resume-token key is set).

You do not need working exploit code — a clear description of the flaw is
enough. Please give the maintainer a reasonable, good-faith window to fix the
issue before any public disclosure.

## What to expect

This project is maintained by one person in their spare time, so these are
good-faith targets, **not** contractual guarantees:

- **Acknowledgement:** within about **7 days**.
- **Initial assessment** (in scope? severity? reproducible?): within about
  **14 days**.
- **Fix or mitigation:** timeline depends on severity and complexity; you will
  be kept in the loop. Critical, easily-exploited relay issues are prioritized.
- **Disclosure:** coordinated. Once a fix is released, a
  [GitHub Security Advisory](https://docs.github.com/en/code-security/security-advisories)
  may be published. You are welcome to be credited by name/handle, or to remain
  anonymous — just say which you prefer.

## Safe harbor

Good-faith security research on **your own deployment** is welcome and will not
be pursued. Please do **not** test against infrastructure you do not own or
operate, do not access or modify other people's broadcasts or data, and do not
run denial-of-service or volumetric attacks against shared/public hosts. Stay
within the law and stop if you encounter data that is not yours.

## No bug bounty

There is no paid bounty program. Meaningful reports are genuinely appreciated
and credited (with your consent) in the advisory and release notes.
