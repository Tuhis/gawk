# Adversarial Review — R37 Relay Server Picker design (`docs/40`, PR #258)

**Date:** 2026-08-13
**Scope:** the design document only — `docs/40-relay-server-picker.md` plus its
ROADMAP/README/index edits, as introduced in PR #258. No code ships in the PR;
this review holds the *design* to the bar its own conventions set: every
grounding claim checked against the code it cites, every security argument
pushed on, every schema read as a hostile implementer would.

**Method.** Every "grounded" claim in §3 (file, line, behaviour) was verified
against the PR's base commit (`fb7d3a3`). The wire allocation, close-code and
CI-trigger claims were checked in `wire.go` and `.github/workflows/`. The
security section's load-bearing statements were traced to the code paths they
depend on. Findings are ordered by severity; each one states what breaks and
what would fix it.

---

## Verdict

**The design is fundamentally sound and unusually well grounded — but it should
not enter phase A as written.** The link grammar, per-server credential model
(D4), two-sided gating (D6/D13), and the in-band identity message are all
correct calls, and nearly every code citation checks out (see "What held"
below). Three findings block: the design's central security control (the
prominent override indicator) is only specified on a page the attack never
visits (F2); the one production surface SP3 must replace is misidentified — the
cited file is the frozen debug tree (F1); and the publish-secret prompt flow
the broadcast-route story leans on does not survive contact with multiple
relays (F3). All three are fixable with a doc revision; none invalidates the
architecture.

---

## Findings

### F1 (major, grounding) — §1/§3 misidentify the production settings surface; SP3 as written targets the frozen debug tree

§1 claims "the only way to point it elsewhere is a dev-gated settings field
(`ServerSettings.tsx`, hidden unless `isDevEnvironment()`)", and §3 has SP3
"replace it with the picker panel". Neither half is accurate:

- `ServerSettings.tsx` contains no gate at all — it renders unconditionally
  wherever it is mounted.
- Its only consumers are `BroadcastPage.tsx:169` and `ViewPage.tsx:175` —
  the **frozen `#/debug/*` tree** (`routing.ts` maps production `#/broadcast`
  and `#/view/{id}` to `BroadcasterScreen`/`ViewerScreen`, not these).
- The *actual* production relay override is inline in
  `BroadcasterScreen.tsx:128` and `ViewerScreen.tsx:227`
  (`const showDevSettings = isDevEnvironment();` + their own panel markup).
  Those two panels are what SP3 must replace.

An implementer following §3 literally would modify the frozen debug surface,
leave both production dev panels in place, and still pass SP3's acceptance
criteria as written (they only test the chip and panel, not the removal).
**Fix:** repoint §1/§3/SP3 at the `BroadcasterScreen`/`ViewerScreen` inline
panels; state explicitly that the debug tree's `ServerSettings.tsx` is frozen
and untouched; add "the two production dev panels are gone, both screens read
resolved values from the new store" to SP3's criteria.

### F2 (major, security) — the prominent-override indicator is only specified on the landing page, which link-borne sessions never visit

The security story for both crafted-link cases rests on one control. §5:
"the chip prominently names the non-default host"; "a malicious relay in a
broadcast link gets the broadcaster's stream only if the broadcaster proceeds
past a prominently non-default chip". But §4.3 defines the chip as "the only
new landing-page element" — and a user opening `#/view/ABC234?relay=…` or
`#/broadcast?relay=…` goes straight to the viewer/broadcaster screen without
ever seeing the landing page (`parseRoute` routes directly;
`ViewerScreen.tsx`'s own comments note viewers usually arrive from a share
link). The one gesture at an in-session surface — "the R26 armed surface, when
it exists" — names a milestone that is designed but **not started**, so at
phase-A ship time it does not exist.

As specified, the load-bearing warning renders exactly where the attack never
happens, and SP2/SP3's acceptance criteria test chip states only on the
landing page. **Fix:** specify a persistent, prominent override/non-default
host indicator **on the viewer and broadcaster screens themselves** (phase A,
not deferred to R26), and give it its own acceptance criteria: visible before
the user grants capture or enters a secret, not dismissible while the override
is active, tested on both routes.

### F3 (major, design gap) — the publish-secret prompt is deployment-scoped; against a foreign secured relay the flow dead-ends in an opaque failure

§4.2's broadcast-route defense says "on a `requirePublishSecret` deployment
the normal secret prompt appears". That prompt is driven by
`requiresPublishSecret()` (`config.ts:84`) — a property of the **UI
deployment's config** (with an `isDevEnvironment()` fallback) — while whether
a secret is needed is a property of the **target relay**
(`server.go:513`: wrong/missing secret → HTTP 401 at CONNECT, which a browser
surfaces as an opaque WebTransport failure). With multiple relays the two
disagree in exactly the case R37 creates:

- UI with `requirePublishSecret: false` (or unset on a non-dev origin) +
  link/selection to a self-hosted relay that *does* require a secret → no
  prompt, CONNECT 401, and the broadcaster sees the same indistinguishable
  failure state as "unreachable". No prompt, no retry path, no way to enter
  the secret — the design never says what happens here.
- The inverse (UI requires, foreign relay open) produces a spurious prompt —
  harmless but wrong.

The entry schema already has per-server `publishSecret`, but nothing specifies
per-server prompt semantics, which entry's slot a prompted secret is written
to, or an on-401-equivalent re-prompt. **Fix:** make the prompt decision
per-resolved-server (e.g. deployment flag governs the pinned default; for any
non-default server, prompt when the entry's secret is empty — or treat
connect-failure-after-no-secret as "may need a secret" and offer entry), and
state explicitly where a prompted secret is stored (§4.1.1's slot for the
resolved entry, session-only for an unsaved override per D2/D3).

### F4 (moderate, internal contradiction) — the default entry's credential shadow contradicts §4.1.1, and no path exists to rotate the default relay's credentials

§4.1.1: `gawk.servers` holds "custom entries only; the pinned default is never
stored (D5)". §4.1.2: migrated legacy credentials are "stored under
`gawk.servers` as a credentials-only shadow record for `"default"`". Both
cannot be true, and the shadow record's shape (an entry with an empty `url`?
a distinct record type?) is unspecified though SP1's tests must exercise it.
Compounding it, SP3's criteria say "default entry not deletable/**editable**"
— so after migration there is no specified way for a user on a secured
deployment to update a rotated publish secret or a regenerated dev cert hash
for the default relay (today they edit the dev panel or get re-prompted; the
prompt's write target is also unspecified, see F3). **Fix:** amend §4.1.1 to
name the default's credential shadow as a legitimate resident of
`gawk.servers` (URL-less, id `"default"`), and split "editable" — identity
fields (label/URL) locked, credential fields editable, with an SP3 criterion.

### F5 (moderate, protocol) — the identity stream races the probe's lifetime, and the relay-side open has unspecified blocking semantics

§4.4's probe closes after ~5 datagram round-trips over ≲1 s; identity arrives
on a relay-opened uni stream "at echo-session start". Nothing sequences the
two: on a fast link the echoes can complete and the client close before the
identity stream is accepted/read, so the picker nondeterministically labels a
current relay "pre-R37" (URL-only). SP6's criteria test "identity absent" but
not "identity late". Relay-side, `OpenUniStream` against a peer that
advertises no uni-stream credit can fail or block — a hostile echo client
must not be able to wedge `handleEcho`, which today never opens streams.
**Fix:** specify that the probe holds the session open for a short identity
deadline (e.g. 500 ms after last echo) before concluding pre-R37, and that
the relay's identity write is best-effort and non-blocking (open failure ⇒
skip, never stall the echo loop); test both.

### F6 (moderate, security/UX) — the identity display name is attacker-influenced trust UI with no sanitization rules, and the example rendering omits the host

The §4.4 name field (≤64 UTF-8 bytes) is controlled by whoever runs the relay
— including a relay reached via a crafted link, i.e. precisely the party the
picker's trust decisions are about. §5 requires directory labels to "render as
text" but states no rules for the identity name: control characters, bidi
override codepoints, homoglyphs of the deployment's own name, or reassuring
strings ("Official relay ✓") all pass the schema. The doc's own example
rendering — "Juho's homelab · gawk-server 1.42.0" — shows **no host**.
A 64-byte cap can also split a UTF-8 sequence, and `versionLen`'s "ASCII" is
asserted but no parser rule rejects non-ASCII. **Fix:** specify sanitization
(strip C0/C1 and bidi controls; reject or replace invalid UTF-8; validate
version as printable ASCII) and require that any surface showing a name for a
non-default server always shows the normalized host alongside it.

### F7 (minor, consistency) — flags are listed as a managed-mode extension point but parsers must reject nonzero flags

§4.9 names "the reserved flags bits + trailing extension bytes" as extension
points; §4.4/SP4 require parsers to reject nonzero flags. Both are individually
defensible (the doc even argues the strict-flags side well) — but together they
mean setting any flag bit in the future breaks identity parsing on every
R37-era client, i.e. flags are *not* a compatible extension point. Also
unspecified: what a malformed identity message does to the probe result
(degrade to URL-label, or fail the probe?). **Fix:** drop flags from §4.9's
extension-point list (trailing bytes suffice), and state that any identity
parse failure degrades to "no identity" without failing an otherwise-good
probe.

### F8 (minor, spec gap) — normalized-origin equality is required for credential attachment, but nothing normalizes entry URLs on save

§4.1.1 attaches a saved entry's credentials to a `?relay=` override only on
"normalized-origin equality", and §4.2 normalizes the link value — but manual
adds, §4.1.2 migration, and native profiles have no normalize-on-save rule. A
user whose saved entry reads `https://Relay.Example.com:4433/` gets silently
empty credentials on a link that names the same server. **Fix:** normalize to
origin on every write path (and in migration), then equality is trivial; add
to SP1's criteria. (Note the elision rule: only `:443` is elidable — `4433`
never is.)

### F9 (minor, D4 tension) — the default's credential shadow is keyed by position, not URL

D5 recomputes the pinned default's URL at load so chart-side `relayUrl`
changes reach users — but the F4 shadow credentials follow the id
`"default"`, not the URL they were saved against. An operator who moves the
deployment to a new relay host will have every migrated user silently present
the *old* relay's secret (and dev cert hash) to the *new* host. Same-operator,
so low harm — but it is exactly the "stored secret for server A presented to
server B" pattern D4 exists to prevent. **Fix:** record the URL the default's
credentials were saved against; discard them when the computed default no
longer matches.

### F10 (minor, privacy) — whether directory offers are auto-probed on panel open is ambiguous

§4.3 probes saved entries on panel open; §4.5 says directory rows have "the
probe available". If offers are auto-probed, opening the picker makes the
browser connect to every third-party server the operator listed — disclosing
the user's IP to servers they never chose — and quietly contradicts §4.4's
"zero traffic without user action" posture (SP6 even asserts it). **Fix:** one
sentence: directory offers are probed on demand only; saved entries on open.

### F11 (minor, spec gap) — cross-tab semantics of the now-mutable server list are unspecified

Today's three keys are read once at store creation and only self-written. R37
makes `gawk.servers`/`gawk.selectedServer` an actively edited dataset; two
tabs (e.g. a broadcast in one, the picker open in another) can now interleave
writes, and unknown-field preservation (§4.1.1) makes read-modify-write races
mildly lossy. Low stakes, but the doc should state the rule (e.g. re-read on
panel open; last-writer-wins; no `storage`-event reactivity promised).

---

## What held

Adversarial credit where due — every one of these was checked and is correct:

- **§3 grounding**: `transportStore.ts:17` (`defaultServerUrl` + precedence
  comment), `routing.ts` (a `?relay=` suffix today fails
  `isValidBroadcastId` → redirect home; QL1's split seam is real, `docs/31`
  D1/D2/D7 say what §4.2 claims they say), `server.go:1297` `handleEcho`
  (drain-aware via `rejectedDraining`, rate-limited via `rateLimited`,
  origin-checked by the global `CheckOrigin` with the loopback probe bypass).
- **Wire**: `0x10` (`TypeStripeState`) is the last allocated type; `0x11` is
  free; the message layout matches house framing (Version byte 0, type byte
  1); the "clients parse and never send" rule correctly cites the
  ViewerCount/TelemetryHello precedent; the `TypeRelayCapabilities` comment
  really does document the append-breaks-old-readers trap §4.4's
  trailing-bytes deviation answers.
- **`SP` prefix**: unclaimed (existing two-letter prefixes: NA, LI, TM, TU,
  WB, AS, QL, …); no collision.
- **No browser-reachable TCP listener**: confirmed — `/healthz`/`/statusz`
  ride the ops (TCP, `MetricsAddr`) listener plus the H3 mux; the §6
  rejection of an HTTPS ping endpoint is sound.
- **CI claim**: `broadcast-windows.yml` triggers on `gawk-server/wire/**`
  (lines 114/120), so SP4's one-PR gate across all four mirrors works.
- **Chart default**: `allowedOrigins: []` in the gawk-server values — "open
  when empty" is the real default posture, as §4.6 states.
- **DPAPI**: `docs/38` D14 is as cited (wrap on save, tolerate plaintext).
- **Doc-count edits**: 39→40 applied consistently in `README.md` (both
  tables + prose) and `docs/README.md`.

## Disposition

Revise `docs/40` for F1–F4 before SP1 starts (F1/F2/F3 change phase-A scope
and acceptance criteria; F4 changes SP1's schema). F5–F7 are one-paragraph
amendments to §4.4/§4.9 that should land in the same revision since SP4/SP5/
SP6 criteria reference them. F8–F11 are single-sentence clarifications.
Nothing here argues against the architecture: with the indicator moved to the
session routes and the secret flow made per-server, the D1–D13 decision set
stands.
