# R37 — Streamlined relay server picker (docs/40)

**Status**: designed 2026-08-06; **implemented 2026-08-14** (SP1–SP9 +
SP11–SP13 — see the implementation-status note below; SP10's manual pass
pending). Chunks **SP1–SP13** (`SP` =
Server Picker; two-letter prefix per the R21+ convention — `TU` is reserved
for R36, `SP` collides with nothing). Phased: **A** (gawk-app core,
frontend-only) → **B** (relay identity probe: `gawk-server` + all four wire
mirrors) → **C** (server directory) → **D** (native broadcaster parity:
`gawk-broadcast` + `gawk-broadcast-windows`) → **E** (telemetry follows the
relay: `gawk-server` + `gawk-telemetry` + app). Phase A ships value on its
own; each phase is releasable independently under the per-component
release-please tags.

**Revised 2026-08-13** after the owner's adversarial review (PR #258,
findings F1–F11; evidence in `docs/reviews/relay-server-picker-design-review.md`
on its review branch): the production settings surface is re-grounded (F1),
the override indicator moves onto the session screens (F2), the secret
prompt becomes per-resolved-server (F3), the default entry's credential
storage and rotation are specified (F4/F9), and the identity handshake
gains timing, robustness, and display-sanitization rules (F5–F7), plus the
F8/F10/F11 clarifications. The D1–D13 decision set is unchanged.

**Extended 2026-08-13** (owner decision, same session): cross-relay
telemetry is **in scope**, not deferred — a picker that lets users roam
relays while their telemetry is silently rejected at token verification
(§4.10) would be a half-delivered feature. New decisions **D14–D17**, new
§4.10, new phase **E** (SP11–SP13); the former §7 "telemetry-URL awareness"
deferral is deleted.

**Implementation status (2026-08-14)**: SP1–SP9 and SP11–SP13 implemented,
all module gates green (gawk-app 1255 tests + build; gawk-server race suite;
gawk-telemetry suite; gawk-broadcast full-module race suite;
gawk-broadcast-windows workspace tests + native clippy — the two Opus-linked
crates' cross-lint rides CI's clang-cl runner per docs/38 D18). SP10's
manual cross-deployment pass is the open item. Recorded deviations, all
review-visible:

- **Native GUI probe display deferred (both GUIs)**: the 0x11 parse side
  ships in both wire mirrors and the pickers' plumbing is in place, but
  neither native GUI renders RTT/identity yet — the SP8/SP9 rows' probe
  criterion moves to a follow-up chunk. No probe traffic exists, so nothing
  regresses.
- **Native profiles are name-keyed** (`"default"` reserved), not id-keyed
  like the frontend schema — the two native apps share one shape
  deliberately; the frontend's id-keyed storage is browser-local and never
  interchanges files with them.
- **`off` beats an advertised 0x12 in the native apps**: the explicit
  telemetry opt-out (`TelemetryOff`) wins over D15's advertised-URL
  precedence — off means off. D15 is about *destination*, not consent.
- **D15's foreign-relay suppression guard was reverted (2026-08-14, review
  R3-A/R3-C)**: the frontend now matches the native fallback semantics —
  advertised wins, else the configured URL on any relay. The suppression
  variant silently stopped telemetry for same-fleet-via-non-default-URL
  topologies (direct IP, alternate DNS, migrated legacy settings), which
  the e2e telemetry scenario caught; the D15 row carries the dated
  revision. Per-send precedence re-resolution (G2), the 250 ms floor pins
  (G3), and the URL-keyed disclosure predicate landed in the same pass.
- **Both native engines' server-message read caps were raised** (258 →
  517 bytes) — the old cap, sized for BroadcastAnnounce, would have
  truncated a full-length TelemetryEndpoint into a parse failure. Found
  test-first on both sides; now in docs/gotchas.md.
- **Ingest's pre-R37 `-cors-origins` allowlist is deprecated and inert**
  (D17 wildcard superseded it); the flag still parses so existing deploys
  upgrade without an argument change, and a warning names the change.

---

## 1. Purpose

Today one gawk UI speaks to one relay. The frontend resolves its relay from
`config.js` (`relayUrl`) → a hostname check → `https://localhost:4433`
(`transportStore.ts:17`), and the only production way to point it elsewhere
is the dev-gated inline server panels on the broadcaster and viewer screens
(`showDevSettings = isDevEnvironment()` — `BroadcasterScreen.tsx:128`,
`ViewerScreen.tsx:227`). That is the right shape for "one operator, one
deployment" — and the wrong shape for where self-hosting is going: people run
their own `gawk-server` (docs/self-hosting.md), and eventually some may buy a
managed one. A relay you run yourself should be usable **from any gawk UI
that permits alternative relays**, without that UI's operator redeploying
anything.

The detail that makes this more than a convenience: **broadcast IDs are
per-relay.** A code minted on relay X is joinable only on relay X — there is
no cross-relay lookup and none is being added. So the moment two relays
exist, "just type the code" only works if viewer and broadcaster happen to
point at the same relay. The share link is what closes that gap: a link that
carries the relay (`#/view/ABC234?relay=…`) works from any permitting UI, no
matter which relay the broadcaster is on. That is why the link grammar is the
centerpiece and the picker is the supporting cast.

Priority 1, unchanged: **the join-by-code flow stays pristine.** A viewer on
the deployment's own relay types a code and watches; nothing in this
milestone adds a step, a decision, or a visible surface to that path.

What R37 adds:

- **`?relay=` on share and broadcast links** — a link "just works" regardless
  of which relay the sharer is connected to (§4.2).
- **A server picker reachable from the home screen** — a subtle chip opening
  a panel with the saved-server list, add/edit/remove, and per-server
  latency (§4.3).
- **Saved servers in localStorage** — a list of entries (label, URL,
  per-server publish secret, per-server dev cert hash) plus the currently
  selected one (§4.1).
- **A probe** — validity ("is this actually a gawk relay?") and latency,
  measured over the real QUIC path via the existing `/echo` route, with a
  small relay addition so the relay can also say *who it is* (§4.4).
- **A server directory** — an operator-configurable fetched list of known
  relays the picker can offer (§4.5).
- **Native parity** — saved server profiles in both native broadcaster GUIs
  (§4.8).
- **Telemetry that follows the relay** — the relay advertises its own
  telemetry ingest URL in-band, so sessions on a foreign relay report to
  *that* operator's collector instead of being rejected at the home
  deployment's token check (§4.10).
- **Managed-relay groundwork** — design-only: the schemas ship with the
  extension points a future paid/managed relay needs, no auth code (§4.9).

## 2. Decisions (locked with the owner, 2026-08-06)

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | **`?relay=` is accepted on both `#/view/{id}` and `#/broadcast`.** | Viewer links are the core "share and it just works" case. Broadcast links compose with R26's quick-start grammar (one bookmark per relay). The crafted-link risk on the broadcast route — pointing someone's broadcaster at a foreign relay — is defused by D4: secrets are per-server, so a foreign relay never receives a stored credential. |
| D2 | **A link's relay is a session-only override with an explicit save affordance.** It never silently joins the saved list and never changes the selected default. | Extends R26 D2 (links never persist settings). A link is pasted, forwarded, and long-lived; opening one must not rewrite what the user's own next session does. The save affordance is how the list grows deliberately. |
| D3 | **Credentials never travel in links or the directory.** `?relay=` carries an https origin only — no publish secret, no cert hash, no path/query on the embedded value. | Extends R26 D1 verbatim. A cert hash is also useless cross-device (it names a dev cert only the operator's own machine trusts), so nothing of value is lost. |
| D4 | **A saved server entry is `{label, url, publishSecret, certHashHex}` — secret and cert hash are per-server.** The legacy global keys migrate into entries on first load (§4.1.2). | Per-server secrets are the security property D1 leans on, not just tidiness: the stored secret for server A can never be presented to server B, whatever a link says. Labels because a URL is a poor display name; cert hash per-server because two dev relays have two certs. |
| D5 | **The deployment's own relay is a pinned, non-removable first entry** ("This deployment"), selected by default. | Reset-to-known-good is always one click, and a user cannot delete their way into a broken install. Its URL is `defaultServerUrl()`'s answer (config.js → hostname check → localhost) recomputed at load, never persisted — so a chart-side `relayUrl` change reaches users who have saved entries. |
| D6 | **Operator gate: `allowCustomRelays` in `config.js`, default `true`.** When `false`: the picker UI is hidden and `?relay=` is ignored with a quiet note (R26 D7 degrade — never fatal, the link still joins via the deployment relay). | "Any gawk UI that has enabled support" — enabled is the out-of-the-box state so shared links work across installs by default, and a locked-down deployment can opt out with one chart value. |
| D7 | **The probe is a WebTransport session to the existing `CONNECT /echo` route.** RTT is measured client-side by round-tripping echo datagrams; identity arrives as a new wire message the relay sends once at echo-session open (§4.4). | The relay has no browser-reachable public TCP listener — `/healthz`/`/statusz` sit on the ops endpoint — and a browser `fetch()` cannot be forced onto HTTP/3, so an HTTPS ping endpoint is a non-starter (§6). `/echo` already exists, is rate-limited, drain-aware, and measures the actual QUIC path viewers use. |
| D8 | **New wire message `TypeRelayIdentity` (0x11), relay→client on a server-opened uni stream at echo-session start.** Carries protocol version, server release version, an operator-set display name, and reserved extension bytes. Clients parse it and never send it. | The browser WebTransport API exposes no response headers (the ResumeToken 0x09 / RelayCapabilities 0x0F precedent), so identity must be in-band. A pre-R37 relay sends nothing: the probe still validates (upgrade succeeded) and measures RTT, and the picker labels it by URL. The display name is a new relay knob and is plumbed flags + `GAWK_*` env + Helm per the registryOptions invariant. |
| D9 | **The directory is a fetched JSON document**: `config.js` gains `serverDirectoryUrl`; the app fetches it when the picker opens — never at boot (the boot path stays fetch-free, the `termsUrl` rule). Directory entries are offers, not saves: adding one is the same explicit act as adding a manual entry, and entries carry no credentials. | Owner choice over a static config array (updatable without redeploying the frontend) and over a central registry service (out of scope, §7). The URL is operator-chosen and therefore trusted the way `config.js` itself is; §5 covers the residual handling rules. |
| D10 | **Managed-relay groundwork is design-only.** The entry schema, identity message, and directory schema each reserve an extension point (§4.9); no auth code, no account linkage, no entitlement checks ship in R37. | Groundwork means the schemas won't need a breaking rev when a managed relay wants a viewer credential — not speculatively implementing one now. |
| D11 | **Native parity = saved server profiles in both native GUIs** (Go/Linux and Rust/Windows), backed by the same entry semantics in each app's existing persistence (0600 JSON config on Linux, DPAPI-wrapped fields on Windows). CLIs stay flag-driven. | The GUIs already persist exactly one server + secret; generalizing to a list is the same shape the frontend gets. The CLI's `-url` flag is already a per-invocation picker. |
| D12 | **One milestone, phased chunks.** Phase A (app core) is releasable alone; B, C, D follow in any order after their dependencies. | The link grammar + picker deliver the product value without any server change; the probe, directory, and native parity each improve it independently. |
| D13 | **The relay's origin allowlist remains the relay-side gate.** A relay operator chooses which UI origins may use their relay via the existing `allowedOrigins` (empty = allow all, the current dev default). R37 adds no new server-side access control — it documents the interplay (§4.6, SP10). | "Usable from any UI" needs consent on both sides: the UI's `allowCustomRelays` and the relay's `allowedOrigins`. Both controls already exist or default open; what's missing is only the self-hosting doc telling a relay operator the second one matters now. |
| D14 *(2026-08-13)* | **The relay advertises its telemetry ingest URL in-band: new wire message `TypeTelemetryEndpoint` (0x12), relay→client on its own uni stream**, composing with the frozen 0x0D hello (whose strict exact-length parser cannot be extended). Sent on publish/subscribe sessions only when the relay both collects telemetry and has an advertised URL configured (`-telemetry-advertise-url` / `GAWK_TELEMETRY_ADVERTISE_URL` / chart `telemetry.advertiseUrl`, plumbed per the registryOptions invariant); never on `/internal/subscribe`. Clients parse it and never send it. | The ingest endpoint lives on the operator's *frontend* Ingress, not the relay, so the relay can only advertise what it is configured with — but it is the one party that also mints the token, so it is the only party that can name a destination where that token verifies. In-band because WebTransport exposes no response headers (the 0x09/0x0D/0x11 precedent). |
| D15 *(2026-08-13; revised 2026-08-14, review R3-A/R3-C)* | **A relay-advertised URL always wins; the configured URL is the fallback on ANY relay** (including every pre-R37 relay). *Revision note*: the original wording added a guard — "a non-default relay that advertises no URL reports nothing" — which the R3-A review falsified: the same fleet is legitimately reached through non-default URLs (direct IP, alternate DNS, a migrated legacy setting, the e2e harness), where the configured collector shares the fleet key and the suppressed batches would have verified. The guard silently stopped exactly those users' telemetry (the e2e failure was the proof), so it is dropped: fallback everywhere, precedence re-resolved at every send. Truly foreign fleets reject fallback batches at token verification — bounded, honest waste, the pre-R37 posture. | Owner choice: the source of truth moves relay-side, one precedence rule everywhere. The accepted cost is duplication on the happy path — a deployment's relay chart and app chart both name the same ingest URL — and the doc says so rather than hiding it. |
| D16 *(2026-08-13)* | **Choosing a relay is the telemetry consent: any resolved relay's advertised URL is honored — saved selection or bare `?relay=` link override alike** — and the in-session indicator (§4.3) states when diagnostics are being reported to a foreign operator. | Owner choice. The operator already terminates the session's QUIC traffic; diagnostics about that same session going to the same operator is proportionate, and the indicator makes it visible rather than silent. The alternative consent gates (saved-only, per-server toggle) were considered and declined as friction that mostly starves relay operators of the data they need to debug. |
| D17 *(2026-08-13)* | **Cross-origin ingest = wildcard CORS + a CORS-safelisted beacon.** The telemetry ingest listener answers preflights with `Access-Control-Allow-Origin: *`, and the unload flush switches to a safelisted content type (`text/plain` carrying the unchanged JSON envelope) so `sendBeacon` needs no preflight it cannot perform during unload. The dashboard/read listener stays never-public, unchanged. | Wildcard is safe here because auth is the in-body HMAC token — no cookies, no credentialed requests — and ingest is already the public listener by design (CLAUDE.md posture). The safelisted beacon is what keeps the final batch alive cross-origin; losing it was considered and declined. |

## 3. Where it plugs into the existing code (grounded)

- **`src/config.ts`** — the runtime-config home. Gains `allowCustomRelays`
  and `serverDirectoryUrl` getters with the standard "empty string counts as
  unset" rule; the chart's `configmap.yaml` renders both.
- **`src/state/transportStore.ts`** — today: one `serverUrl` / `certHashHex`
  / `publishSecret`, each its own localStorage key, `defaultServerUrl()`
  precedence documented in place. SP1 remodels this store around entries +
  selection + session override (§4.1); every consumer
  (`broadcaster.ts`, `viewer.ts`, the workers' session bootstrap) keeps
  reading resolved values from the same store, so the transport layer does
  not change.
- **`src/routing.ts`** — `parseRoute` currently anticipates no query;
  `#/view/ABC234?relay=…` would today fail `isValidBroadcastId` and redirect
  home. R26's QL1 already specifies splitting the query off before matching;
  SP2 lands on the same seam. **Whichever of QL1/SP2 merges first implements
  the split; the other consumes it** — the two grammars are disjoint
  (`relay` vs. R26's quality params) and both inherit R26 D7 (unknown params
  ignored, collected into a quiet note).
- **`src/features/broadcaster/BroadcasterScreen.tsx` /
  `src/features/viewer/ViewerScreen.tsx`** — the production surfaces' inline
  dev-gated server panels (`showDevSettings = isDevEnvironment()`,
  `BroadcasterScreen.tsx:128`, `ViewerScreen.tsx:227`). SP3 **removes both**
  and points both screens at the store's resolved values; the picker panel
  is the replacement, with the dev gate surviving only on the cert-hash
  field's *visibility* inside it (real users on real certs never need it).
  Not to be confused with `src/features/stream/ServerSettings.tsx`: that
  form's only consumers are the frozen `#/debug/*` tree's pages
  (`BroadcastPage.tsx:169`, `ViewPage.tsx:175`), and the debug tree stays
  untouched (F1).
- **`gawk-server/internal/transport/server.go`** — `handleEcho`
  (`server.go:1297`): upgrade, then echo datagrams verbatim; rate-limited,
  drain-aware, origin-checked like every route. SP5 adds one write: open a
  uni stream, send `RelayIdentity`, close. The k8s exec probes
  (`gawk-echo` against 127.0.0.1) ignore unread incoming uni streams, so
  they are unaffected.
- **`gawk-server/wire/wire.go`** — 0x10 is the last allocated type; SP4
  allocates **0x11 `TypeRelayIdentity`** and **0x12 `TypeTelemetryEndpoint`**
  and mirrors them in
  `gawk-app/src/transport/wire.ts`, `gawk-broadcast/internal/wirecheck`, and
  `gawk-broadcast-windows/crates/wire`, golden vectors byte-identical, all in
  the same PR (the Windows CI job triggers on `gawk-server/wire/**`).
- **`cmd/gawk-server/main.go`** — the new `-server-name` knob goes through
  `registryOptions` with flag + `GAWK_SERVER_NAME` + chart value, per the
  CLAUDE.md invariant.
- **`gawk-telemetry/internal/ingest/ingest.go`** — batch tokens are
  verified against the service's own fleet key
  (`VerifyTelemetrySessionToken(h.opts.Key, …)`, `ingest.go:341`), which is
  exactly why a foreign relay's sessions can't land data anywhere today;
  SP12 adds ingest-listener CORS + the safelisted beacon content type
  (§4.10) and touches nothing else.
- **`gawk-broadcast/internal/config/config.go`** — the 0600 JSON settings
  file (URL + secret + telemetry already). SP8 generalizes it to carry a
  server list. **`gawk-broadcast-windows/crates/app`** persists the same
  shape with DPAPI-wrapped secrets (docs/38 D14); SP9 follows that pattern
  per entry.

## 4. Design

### 4.1 The server model

#### 4.1.1 Entry schema and storage

```ts
interface RelayServerEntry {
  id: string;          // opaque, generated at add time ("default" reserved)
  label: string;       // user-editable display name
  url: string;         // https origin, e.g. "https://relay.example.com:4433"
  publishSecret: string; // per-server; "" = none stored
  certHashHex: string;   // per-server dev cert hash; "" = real certificate
  // reserved: `auth` (see §4.9) — absent in R37, tolerated on read
}
```

Two localStorage keys replace the three legacy ones:

- `gawk.servers` — JSON array of `RelayServerEntry`: custom entries, plus at
  most one **credentials-only record for the default** (`id: "default"`, no
  label, `url` set to the normalized default URL the credentials were saved
  against). The pinned default's *identity* is still never stored (D5 — its
  URL is recomputed every load); the record exists only so the default can
  carry a secret/cert hash, and its `url` is a guard: **when the recomputed
  default no longer matches it, the credentials are discarded** — a
  chart-side `relayUrl` change must not present the old relay's secret and
  cert hash to the new host (F9).
- `gawk.selectedServer` — the selected entry's `id`; absent/unknown ⇒
  `"default"`.

Unknown fields on parsed entries are preserved on rewrite (forward
compatibility for §4.9); a corrupt `gawk.servers` falls back to "no custom
servers" rather than throwing — losing a hand-added list beats a boot loop.

Resolution precedence, top wins:

1. **Session override** (a `?relay=` link, this tab only — never persisted)
2. **Selected entry** (`gawk.selectedServer`)
3. **Pinned default** (`defaultServerUrl()` — config.js `relayUrl` →
   hostname check → localhost, unchanged)

The session override is an in-memory pseudo-entry with empty
`publishSecret`/`certHashHex` unless the URL exactly matches a saved entry —
in which case that entry is used, credentials included. "Exactly matches"
means normalized-origin equality (§4.2) — and to make that test honest,
**entry URLs are normalized to origin form on every write path** (save,
edit, migration, directory add) with the same rule §4.2 applies to `?relay=`
values, so a hand-entered `https://Relay.Example.com:4433/` can never
silently fail to match its own link (F8).

Cross-tab rule (F11): the panel re-reads localStorage when it opens; writes
are last-writer-wins; no `storage`-event reactivity is promised — an open
tab keeps its resolved server until its own next navigation.

#### 4.1.2 Migration

On first load with legacy keys present (`gawk.serverUrl`, `gawk.certHashHex`,
`gawk.publishSecret`):

- Legacy URL equals the computed default (or is empty) → store the legacy
  secret + cert hash as the default's credentials-only record (§4.1.1),
  keyed to the normalized URL they were saved against, and select
  `"default"`.
- Otherwise → create one custom entry ("Migrated server", the legacy URL,
  legacy credentials) and select it — the user who pointed their install at a
  custom relay keeps working without noticing.

Legacy keys are removed after a successful write of the new ones. Migration
is idempotent and unit-tested both ways (SP1).

### 4.2 The `?relay=` link grammar

Grammar (both routes): `#/view/{id}?relay=<value>` and
`#/broadcast?relay=<value>` (composing freely with R26 params on the latter).

The value must parse as a URL with scheme `https`, no username/password, no
path (other than `/`), no query, no fragment. It is normalized to an origin
(`https://host[:port]`, default port elided). Anything else — `http:`, a
path, a smuggled credential — is **rejected as if absent** and reported in
the same quiet invalid-parameter note R26 D7 defines. Never fatal: the link
still joins, on the user's own selected server.

Behaviour on a valid value (and `allowCustomRelays` true):

- The session override is set before the connection is attempted; viewer and
  broadcaster flows are otherwise byte-identical.
- The in-session indicator (§4.3) renders on the viewer or broadcaster
  screen itself, naming the override's host — link-borne sessions route
  straight to those screens and never visit the landing page, so the
  warning lives where the session actually is (F2).
- The chip's panel offers **"Save this server"** for an override URL not in
  the list (D2). Saving creates a normal entry (label defaulted to the host)
  but does **not** change `gawk.selectedServer` — selection is its own click.

With `allowCustomRelays` false: the override is not applied; the quiet note
says the link asked for another server and this deployment doesn't allow it.

Broadcast-route specifics — **the secret prompt is decided per resolved
server, not per deployment** (F3). `requiresPublishSecret()`
(`config.ts:84`) is a property of the UI deployment and keeps governing only
the pinned default; needing a secret is a property of the *target relay*
(`server.go:513` answers a wrong or missing secret with a 401 at CONNECT,
which the browser surfaces as an opaque failure). So for any non-default
resolved server: prompt whenever that entry's `publishSecret` is empty, and
treat a publish connect failure with no secret presented as "this relay may
require a publish secret" — an entry field plus retry, not a dead end
indistinguishable from "unreachable". (The inverse mismatch — UI requires,
foreign relay open — disappears with the same rule.) A prompted secret is
stored in the resolved entry's §4.1.1 slot; for an unsaved link override it
is held for the session only (D2/D3) and persists only through "Save this
server". That the override relay starts with no stored secret is also the
security property: a crafted link can redirect a broadcaster's *attention*,
but never their credential (D1/D4). The R26 armed surface, when it exists,
renders the override host on the armed card for the same reason.

### 4.3 The picker UI

- **The chip.** A small server indicator on the landing page. On the default
  server it is visually quiet (muted, iconographic — the join-by-code flow
  reads exactly as today); on a non-default selection or an active session
  override it is prominent and names the host. The chip is the only new
  landing-page element (priority 1). Hidden entirely when
  `allowCustomRelays` is false.
- **The in-session indicator** (F2 — phase A, not deferred to R26). The
  chip's counterpart on the viewer and broadcaster screens: whenever the
  resolved server is not the pinned default — saved selection or link
  override — a persistent element on the session screen names the
  normalized host (plus the relay's §4.4 display name once known, host
  always alongside). It renders before capture is granted or a secret
  entered and is not dismissible while the non-default resolution is
  active. This is the load-bearing control in §5's crafted-link story, so
  it lives where link-borne sessions actually land; on the default server
  it does not render at all, keeping both screens exactly as today.
- **The panel.** Opened from the chip: the pinned default first, then custom
  entries; each row shows label, host, probe state (§4.4: latency in ms /
  probing / unreachable), and select-on-click. Add/edit/remove for custom
  entries (add form: label, URL, optional publish secret; cert-hash field
  dev-gated as today). The pinned default's *identity* (label, URL) is
  locked, but its **credential slots are editable** — that is the rotation
  path for a deployment relay's publish secret or dev cert hash after
  migration (F4; stored as the §4.1.1 credentials-only record). Directory offers render in their own section (§4.5).
  Broadcast and view pages keep using whatever the store resolves — the
  panel is reachable from the landing page and from the (dev-visible)
  server settings location it replaces.
- Selection persists (`gawk.selectedServer`); a manual selection during a
  link session persists too (a deliberate act, R26 D2's same carve-out).

### 4.4 The probe

**RTT** — frontend-only. Open `new WebTransport(url + '/echo')` (with
`serverCertificateHashes` when the entry carries a cert hash), send a few
small datagrams stamped with `performance.now()`, read the echoes, take the
median of the successful samples (target: 5 samples over ≲1 s), close. The
session is held open until the identity stream has been read **or** an
identity deadline (~1.5 s from upgrade) passes, whichever comes first — on a
fast link the echoes complete before the relay's uni stream arrives, and
closing on RTT alone would nondeterministically label a current relay as
pre-R37 (F5). Displayed per-entry in the panel; probed on panel open and on demand, never
in the background (an idle landing page generates zero relay traffic — the
probe rides the same rate limiter as every route, and k8s probes already
exercise `/echo` constantly).

**Identity** — the relay's half (SP4/SP5). On upgrading an echo session the
relay opens one unidirectional stream, writes one `RelayIdentity` message,
and closes the stream. The send is **best-effort and non-blocking**: it runs
off the echo loop's critical path with a short deadline, and any failure —
including an echo client that grants no uni-stream credit — abandons the
identity send, never the echo service (a hostile client must not be able to
wedge `handleEcho`; F5):

```
byte 0    Version (0x01)
byte 1    TypeRelayIdentity (0x11)
byte 2    flags — all bits reserved, must be 0 in R37
byte 3    uint8 versionLen (1–32), then that many ASCII bytes — the relay's
          release version ("1.42.0")
next      uint8 nameLen (0–64), then that many UTF-8 bytes — the operator's
          display name (-server-name / GAWK_SERVER_NAME / chart
          config.serverName; empty = unset)
rest      reserved extension bytes — parsers MUST ignore them (§4.9)
```

Two deliberate deviations from house parser strictness, both stated here so
they survive review: **trailing bytes are tolerated** (this message is the
extension point managed mode will use — a strict exact-length parser would
make every future field a breaking change, which is the exact trap the
TypeRelayCapabilities comment documents), and it is **echo-route only** in
R37 (publish/subscribe sessions don't need it; keeping them byte-identical
keeps the blast radius at zero for every existing client). The flags byte
stays strict (reserved-must-be-zero) — flags gate *interpretation* of what a
parser already reads, so an unknown flag genuinely is unparseable, unlike
appended fields. Clients parse `RelayIdentity` and never send it; one
arriving where a client is the sender is dropped (the ViewerCount rule).

**Picker semantics**: upgrade succeeded + echoes returned ⇒ *valid relay*,
show RTT. Identity received ⇒ show the display name — **sanitized, and
never in place of the host** (F6). The name is attacker-influenced trust
UI: the relay a crafted link points at chooses it, and "Official relay ✓"
must not be able to displace the one string a user can verify. So: control
and bidi-control characters are stripped, the name renders as plain text,
the 64-byte cap is enforced after UTF-8 validation with truncation on a
rune boundary, and for any non-default server the normalized host always
renders alongside it — "Juho's homelab — relay.example.com · gawk-server
1.42.0". A malformed identity message (nonzero flags, bad lengths, invalid
UTF-8) degrades to "no identity" and never fails an otherwise-good probe
(F7). No identity ⇒ label by URL (pre-R37 relay — still fully usable).
Failure is one combined state: browsers surface WebTransport failures
opaquely, so "unreachable", "not a gawk relay", "invalid certificate", and
"this relay does not allow this UI's origin (D13)" are generally
indistinguishable from JS. The panel's failure copy names all four causes
once rather than pretending to diagnose; per-cause probing is explicitly not
promised (§7).

### 4.5 The directory

`config.js` gains `serverDirectoryUrl` (default `""` = no directory; the
chart renders it like every other key). When set, opening the picker fetches
it (`cache: 'no-store'`, ~3 s timeout, errors degrade to "directory
unavailable" — the panel's own content never blocks on it). Never fetched at
boot (D9).

```json
{
  "version": 1,
  "servers": [
    { "label": "Official gawk fleet", "url": "https://api.gawk.ioio.fi:4433" },
    { "label": "EU mirror",           "url": "https://relay-eu.example.com:4433",
      "managed": false }
  ]
}
```

`version` must be `1` (else: directory unavailable). Entries: `label`
(≤64 chars, rendered as text), `url` (validated by the same rule as
`?relay=`, invalid entries dropped individually), optional `managed`
(boolean, default false — display groundwork for §4.9, no behaviour in R37).
**No credential fields exist in the schema**; an entry carrying extra fields
has them ignored. Directory rows render in their own panel section with the
probe available **on demand only — offers are never auto-probed on panel
open** (F10: opening the picker must not disclose the user's address to
every third-party host an operator listed; SP6's no-unrequested-traffic
assertion covers directory rows too), and "Add" copies one into
`gawk.servers` — the same explicit act as manual entry (D9). CORS note for operators: the URL is
fetched from the app origin, so serve it same-origin (a ConfigMap-mounted
static asset next to `/config.js`, the `terms.html` pattern) or with
`Access-Control-Allow-Origin` for the app's origin.

### 4.6 Two gates, both sides

Cross-UI relay use requires consent from both operators (D6, D13):

| Control | Owner | Default | Effect when closed |
|---|---|---|---|
| `allowCustomRelays` (`config.js`) | UI operator | open (`true`) | Picker hidden; `?relay=` ignored with a quiet note |
| `allowedOrigins` (relay flag/env/chart) | Relay operator | open when empty (dev default; the reference fleet lists its own origins) | Foreign UIs' upgrades are refused at CONNECT |

The self-hosting doc gains a "using your relay from other gawk UIs" section
(SP10): list the UI origins you accept in `allowedOrigins` (or leave empty
to accept any), set `config.serverName` so pickers can identify you, and
optionally host a directory for your users.

### 4.7 What join-by-code means with multiple servers

Typing a code searches exactly one relay: the resolved one (§4.1.1). A code
minted elsewhere is "broadcast not found" — correct and unavoidable without
cross-relay lookup, which is not being built. The product answer is D1: the
**share link carries the relay**, so the cross-relay join path is the link,
and the code-only path remains what it always was — same-relay, pristine.
The broadcaster's share UI includes `?relay=` in the copied link whenever
the broadcast's relay is not the deployment default (on the default, links
stay exactly as short as today).

### 4.8 Native broadcaster parity (phase D)

Both native GUIs get a server dropdown backed by a profiles list with the
§4.1.1 field set, plus the probe (§4.4 — both engines already dial
WebTransport, and identity parsing comes from their wire mirrors):

- **`gawk-broadcast` (Linux)**: `internal/config` grows
  `servers []ServerProfile` + `selectedServer` alongside the existing
  flat fields, which migrate exactly as §4.1.2 (file stays 0600; the GUI
  gains the dropdown + an edit dialog; `gawk-broadcast` CLI keeps `-url`).
- **`gawk-broadcast-windows`**: the same shape in the app's settings, each
  profile's `publishSecret` DPAPI-wrapped per docs/38 D14 (wrap on save,
  tolerate plaintext on read).

Native apps ignore `allowCustomRelays` — it is a *frontend deployment's*
posture; a binary on your own PC answers to you (the CLI already accepts any
`-url` today, so the GUI restricting itself would protect nothing).

### 4.9 Managed-relay extension points (design-only, D10)

What a managed relay will someday need that a self-hosted one doesn't:
per-viewer/broadcaster authorization (an entitlement, not just the shared
publish secret) and a way for the client to learn *that auth is required and
of what kind* before connecting. R37 reserves, and does not implement:

- **Entry schema**: a reserved `auth` object on `RelayServerEntry`
  (unknown-field-preserving storage, §4.1.1, is what makes this a no-op
  now).
- **Identity message**: the trailing extension bytes (§4.4) — an auth-modes
  field appends without a wire break. Deliberately *not* the flags byte:
  R37 parsers reject nonzero flags, so a future flag bit would break
  identity parsing on every R37-era client; appended fields are the
  extension mechanism, flags are not (F7).
- **Directory schema**: the `managed` marker (§4.5).

Nothing else — no token formats, no account model, no billing surface. When
managed relays become real they get their own milestone and design doc; R37's
promise is only that these three schemas won't need breaking revs to start
it.

### 4.10 Telemetry follows the relay (phase E)

Without this phase, R28 telemetry and R37 relay roaming contradict each
other: collection is gated by the *relay* (wire 0x0D `TelemetryHello` —
enabled flag, cadence, and a session token minted with the fleet's
telemetry key), but batches POST to the *UI deployment's* `telemetryUrl`,
whose ingest verifies tokens with its own key
(`gawk-telemetry/internal/ingest/ingest.go:341`). A session on a foreign
relay therefore either collects nothing (that fleet has telemetry off — the
default) or collects and is rejected at token verification. Nobody gets
data. Phase E makes the destination follow the same party that already owns
the gating and the token: the relay fleet.

**Wire (D14).** New message `TypeTelemetryEndpoint` (0x12), relay→client on
its own reliable unidirectional stream (the 0x0D hello is a strict
exact-length message on its own stream — extending either would break every
existing reader, the RelayCapabilities append-trap):

```
byte 0    Version (0x01)
byte 1    TypeTelemetryEndpoint (0x12)
byte 2    flags — all bits reserved, must be 0
bytes 3-4 uint16 urlLen (1–512), then that many bytes: an absolute https
          URL, ASCII, the fleet's ingest endpoint
rest      reserved extension bytes — parsers MUST ignore them (the
          RelayIdentity forward-compat stance, §4.4)
```

Sent once per publish/subscribe session, only when the relay both collects
telemetry and has an advertised URL configured; never on
`/internal/subscribe` (an edge is plumbing, not a client). Clients parse it
and never send it (the ViewerCount rule); a malformed message is dropped and
degrades to "no advertised URL". The knob —
`-telemetry-advertise-url` / `GAWK_TELEMETRY_ADVERTISE_URL` / chart
`telemetry.advertiseUrl` — goes through `registryOptions` per the
invariant. It names infrastructure the relay does not itself serve (ingest
rides the operator's frontend Ingress), so the self-hosting doc's SP10
section pairs it with a "verify with a probe from a foreign UI" step; the
reference deployment sets it to its own public ingest URL.

**Client precedence (D15, revised 2026-08-14).** Advertised URL →
configured URL (`config.js` `telemetryUrl` / its same-origin default), on
ANY relay — the pre-R37 fallback behavior, everywhere. Precedence is
**re-resolved at every send** (G2): 0x0D and 0x12 ride separate uni streams
with no ordering guarantee, so a late 0x12 simply redirects the rest of the
session's batches (already-sent ones went to the fallback; the ingest's
`seq` field keeps either dataset honest about gaps). Client-side the URL is
validated like every other URL in this design (absolute https, else
ignored, parse failure degrades to "no advertised URL"). The
reflector-rate bound on this attacker-influenced destination is the
client-side **250 ms sampling floor** (G3) — `lib/telemetry.ts` and the
native reporters clamp the hello's relay-chosen interval, and that clamp is
pinned by its own test in each module because a refactor dropping it would
otherwise fail nothing.

**Consent and visibility (D16).** Any resolved relay qualifies — saved
entry or bare link override. The in-session indicator (§4.3) carries the
disclosure: when a foreign relay's telemetry is active, the indicator names
it ("diagnostics shared with this server's operator"). No extra prompt: the
operator in question is already terminating the session's media transport.

**Cross-origin ingest (D17).** Two changes in `gawk-telemetry`, both on the
ingest listener only:

- **CORS**: answer `OPTIONS` preflights and set
  `Access-Control-Allow-Origin: *` on ingest responses. Wildcard is
  deliberate — authentication is the in-body HMAC token, no cookies or
  credentialed requests exist, and ingest is already the public listener;
  an origin allowlist would just be a second `allowedOrigins` for every
  relay operator to forget.
- **Beacon content type**: the unload flush switches to `text/plain`
  carrying the unchanged JSON envelope — a CORS-safelisted content type, so
  `sendBeacon` sends it cross-origin without the preflight it cannot
  perform during unload. Ingest accepts `text/plain` alongside
  `application/json`; in-session batches (fetch, which can preflight) stay
  `application/json`. The envelope bytes are identical either way — this is
  a transport-header concession, not a format fork.

**Native broadcasters (G1's native rule, stated).** A native app has no
deployment, so "default" is defined by URL provenance, not a resolution
model: an advertised 0x12 wins; an **explicitly configured** telemetry URL
(flag/env/config field) is honored on any relay; the hardcoded
`DefaultTelemetryURL` fallback pairs only with the reference fleet's relay —
against any other relay with nothing configured and nothing advertised, the
reporter sends nothing (there is no honest destination to fall back to).
The explicit `off` opt-out beats everything, an advertised URL included —
D15 is about *destination*, not consent. Both wire mirrors gain 0x12 in
SP4's allocation pass. This retires the awkwardness of
`gawk-broadcast/internal/config`'s `DefaultTelemetryURL` — a hardcoded
second constant that exists precisely because the ingest URL was never
derivable from the relay URL.

## 5. Security considerations

- **Credentials stay home.** Never in links or directories (D3, schema-level
  in §4.5); per-server storage (D4) means no link, saved or not, can cause a
  stored secret to be presented to a different origin than the user saved it
  for. This is the load-bearing property that makes D1's broadcast-route
  links acceptable.
- **A malicious relay in a viewer link** receives a viewer connection and
  serves it bytes; the viewer's parsers are already strict against hostile
  input (every wire parser validates), and the in-session indicator (§4.3)
  names the non-default host on the viewer screen itself. Worst case is
  garbage video from a server the user can see they're on.
- **A malicious relay in a broadcast link** gets the broadcaster's *stream*
  only if the broadcaster proceeds past the persistent in-session indicator
  on the broadcaster screen (§4.3 — rendered before capture is granted or a
  secret entered, not dismissible while the override is active) — and never
  gets a stored credential (above). Residual risk is social, the same class
  as any phishing link, and is why the indicator lives on the session
  screen, where link-borne sessions actually land (F2).
- **`?relay=` value hygiene**: https-only origin, no embedded credentials,
  no path/query (§4.2) — the value can name a host and nothing else.
- **Locked-down installs**: `allowCustomRelays: false` removes the whole
  surface (D6); relay-side, `allowedOrigins` remains the authoritative gate
  on which UIs may connect at all (D13).
- **Directory trust**: the URL is operator-configured (trusted like
  config.js itself), but entries are still validated per-field, labels
  render as text (no markup), and adding remains explicit — a compromised
  directory can *offer* a hostile server, not install or select one.
- **Raw broadcast IDs**: untouched. `?relay=` names a server, never a
  broadcast; ID exposure rules (HMAC-keyed `/statusz`) are unaffected.
- **Probe abuse**: the probe is user-initiated, bounded (a few datagrams),
  and subject to `/echo`'s existing rate limiter; no background probing
  (§4.4).
- **The advertised telemetry URL is attacker-influenced** (§4.10): the
  relay a crafted link points at chooses where the client POSTs batches.
  Bounds: https-only and length-capped, honored only while that same
  relay's hello enables collection, payload is solely the session's own
  diagnostics envelope (with the *obfuscated* broadcast key — raw IDs never
  ride telemetry, R9 D3), and the in-session indicator discloses the
  foreign destination. The cadence bound is NOT the hello's interval (a
  hostile relay sends 0): it is the client-side 250 ms sampling floor,
  test-pinned in every reporting module (G3). The envelope carries nothing
  *sensitive* beyond what the chosen relay already observes as the
  session's transport endpoint — client-side decode/render metrics and a
  coarse browser/OS class ride along; as a DDoS reflector it is a trickle
  on a per-user opt-in path. The disclosure keys on **normalized-URL
  equality** with the recomputed default, never entry identity — a saved
  duplicate of the deployment's own relay is not foreign. Accepted (D16).
- **Wildcard CORS on ingest** (D17) widens nothing that matters: the
  listener was already public by design, requests carry no cookies or
  ambient credentials, and every batch still dies without a valid HMAC
  token for *that* fleet's key. The dashboard/read listener is untouched
  and never routed publicly (the CLAUDE.md split stands).

## 6. Rejected alternatives

- **An HTTPS ping endpoint on the relay** (`GET /pingz` + browser `fetch`) —
  the relay has no public TCP listener and a browser cannot force `fetch()`
  onto HTTP/3 without prior Alt-Svc; adding a public TCP listener for a ping
  widens the exposed surface to solve a problem `/echo` already solves on
  the real path. (D7)
- **Identity via CONNECT response headers** — the browser WebTransport API
  exposes none; this is the third time the codebase hits this wall
  (ResumeToken, RelayCapabilities) and the answer stays in-band. (D8)
- **Reusing `TypeRelayCapabilities` (0x0F) for identity** — capabilities are
  per-session feature negotiation sent on media routes to *producers*;
  identity is human-facing metadata on the diagnostic route. Overloading one
  message for both audiences couples their revision cycles for no byte
  saved.
- **Cross-relay broadcast lookup** ("type a code anywhere") — requires a
  shared namespace or a registry between unrelated deployments; the share
  link carrying the relay achieves the user outcome without either. (§4.7)
- **Auto-selecting the lowest-latency server** — relay choice determines
  *where a minted code lives* (§4.7); silently moving it breaks every share
  link the broadcaster is about to send. Latency is displayed, choice stays
  explicit.
- **A central registry service relays register with** — a real server
  browser is its own milestone with its own abuse surface; the fetched
  directory covers the "offer my users a list" need at static-file cost.
  (D9, §7)
- **Per-cause probe failure diagnosis** — browsers deliberately blur
  WebTransport failure causes (fingerprinting); promising "wrong origin" vs
  "bad cert" copy would be guessing. One honest combined failure state.
  (§4.4)
- **Extending `TelemetryHello` (0x0D) in place with the URL** — its parser
  is strict exact-length in all four mirrors, so any appended field breaks
  every existing reader; and sending only an extended v2 hello would cut
  pre-change clients off from telemetry entirely. A composing message on
  its own stream costs one type allocation and breaks nobody. (D14, §4.10)
- **Dropping the final batch on cross-origin telemetry** — considered as
  the zero-change alternative to the safelisted beacon; declined because
  session-end data is disproportionately the diagnostic payload (the 0x0D
  design notes already record why one-shot session-boundary delivery gets
  special treatment). (D17)

## 7. Out of scope

- Any auth/entitlement implementation (D10 — schemas reserve, nothing
  ships).
- A central relay registry; relay-side directory self-registration.
- Cross-relay code lookup or broadcast migration between relays.
- Background/periodic probing, health monitoring, or auto-failover between
  saved servers.
- `allowCustomRelays` enforcement in native apps (§4.8).
- Cross-fleet telemetry *aggregation* (one dashboard over sessions spread
  across operators): phase E routes each session's data to its own relay's
  operator (§4.10); joining datasets across operators is nobody's current
  need.

## 8. Chunks & acceptance criteria

Phase A — app core (frontend-only; releasable alone):

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| SP1 | Server model: entry schema, `gawk.servers`/`gawk.selectedServer`, resolution precedence, URL normalization, migration | Unit: precedence (override > selected > default) exhaustively tested; **URLs normalized on every write path** — a mixed-case/trailing-slash saved entry still credential-matches its normalized link (F8); the default's credentials-only record is keyed to its URL and **discarded when the recomputed default URL changes** (F9); cross-tab semantics per §4.1.1 (re-read on panel open, last-writer-wins) tested (F11); migration from both legacy shapes (default-URL and custom-URL) verified incl. idempotency + legacy-key removal; corrupt JSON degrades to empty list; unknown entry fields survive a rewrite. All existing transport-consumer tests pass unchanged. |
| SP2 | `?relay=` grammar on `#/view/{id}` + `#/broadcast`: query split, validation/normalization, session override, save affordance, quiet notes, `allowCustomRelays` gating, per-server secret-prompt semantics | Unit: valid/invalid value matrix (scheme, credentials, path, port normalization); route tests for both routes with and without the flag; override never written to localStorage; saved-entry URL match attaches credentials, non-match yields empty ones. **F3 semantics tested**: deployment flag governs only the pinned default; a non-default server with an empty entry secret prompts; publish connect failure with no secret presented offers secret entry + retry; a prompted secret lands in the resolved entry's slot, session-only for an unsaved override. Compatible with QL1's query-split seam (whichever lands second rebases on the first). |
| SP3 | Picker UI: landing chip (quiet/prominent states), **in-session indicator on both session screens**, panel (list/select/add/edit/remove, pinned default, dev-gated cert-hash field), removal of both production dev panels, config plumbing (`allowCustomRelays` getter + chart value) | UI tests: chip hidden when flag off; quiet on default, prominent on non-default/override; **in-session indicator present on viewer and broadcaster screens before capture/secret whenever the resolved server is non-default, not dismissible while active, absent on the default server** (F2); default entry's identity not deletable/editable but its **credential slots editable** (F4); selection persists; manual selection during a link session persists. **Both production dev panels (`BroadcasterScreen.tsx:128`, `ViewerScreen.tsx:227`) removed, both screens read resolved values from the new store; the frozen `#/debug/*` tree incl. `ServerSettings.tsx` untouched** (F1). `configmap.yaml` renders the new key; landing page DOM unchanged vs. today except the chip. |

Phase B — probe + identity (server + wire mirrors + app):

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| SP4 | Wire: `TypeRelayIdentity` 0x11 **and `TypeTelemetryEndpoint` 0x12** in `wire.go` + all three mirrors, golden vectors (one allocation pass, one PR) | Append/Parse with vectors byte-identical across Go, TS, wirecheck, and Rust for both messages; parsers tolerate trailing bytes (tested), reject nonzero flags, reject out-of-range lengths and (0x11) invalid UTF-8, (0x12) non-https/oversize URLs; **client-side, a parse failure degrades to "no identity" / "no advertised URL" without failing the probe or the session** (F7); drop-if-sent-by-client rule tested for both. All four modules' gates green in one PR. |
| SP5 | Relay: send identity on `/echo` open (best-effort, non-blocking); `-server-name` knob (flag + `GAWK_SERVER_NAME` + chart `config.serverName`) through `registryOptions` | Go test: echo session receives exactly one identity uni stream with configured name/version; empty name when unset; **an echo client granting no uni-stream credit does not stall or fail the echo loop — the identity send is abandoned, echoes keep flowing** (F5); k8s exec probe path (`gawk-echo`) unaffected (probe test stays green); knob reaches production config (registryOptions test, the R2 lesson). |
| SP6 | App probe: on-demand `/echo` dial per entry, median RTT, identity wait window, sanitized identity display, combined failure state | Unit (mocked transport): median over lossy samples; timeout → failure state; identity absent → URL-labelled entry still selectable; **identity arriving after the echoes complete is still shown — the §4.4 wait window works** (F5); **name sanitization (control/bidi strip, rune-boundary truncation) and host-always-alongside-name asserted** (F6). e2e (tier-1): panel shows a live RTT against the test relay and its configured name. No probe traffic without user action (asserted). |

Phase C — directory:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| SP7 | `serverDirectoryUrl` config key + chart value, fetch-on-open, schema v1 validation, panel section, explicit add | Unit: version/URL/label validation incl. per-entry drop; fetch failure/timeout degrades without blocking the panel; no fetch at boot (asserted); **directory offers are probed on demand only — opening the panel generates no traffic to listed servers** (F10); added entry equals a manually-added one; credential-bearing fields in a directory entry are ignored. Chart renders the key. |

Phase D — native parity:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| SP8 | `gawk-broadcast`: profiles in `internal/config` (+ migration), GUI dropdown + editor, probe, 0x12 honor | Go unit: config round-trip + migration idempotency, file stays 0600; engine dials the selected profile; probe shows RTT/name in the GUI against a test relay; **R28 reporter uses an advertised 0x12 URL over the configured one, and the §4.10 no-URL-on-foreign-relay guard holds**. CLI behaviour unchanged. |
| SP9 | `gawk-broadcast-windows`: same, DPAPI-wrapped per-profile secrets | Rust unit: settings round-trip, DPAPI wrap-on-save/plaintext-on-read per profile; identity + telemetry-endpoint parse via `crates/wire`; GUI dropdown drives the engine's dial; **reporter honors 0x12 with the same precedence + guard**. Cross-compiled CI job green. |

Phase E — telemetry follows the relay (§4.10; depends on SP4's wire pass +
phase A's resolution model):

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| SP11 | Relay: send 0x12 on publish/subscribe when telemetry is enabled and `-telemetry-advertise-url` is set (flag + `GAWK_TELEMETRY_ADVERTISE_URL` + chart `telemetry.advertiseUrl`, through `registryOptions`); never on `/internal/subscribe` | Go test: enabled+URL ⇒ exactly one 0x12 stream per session on both routes; disabled or unset ⇒ none; internal subscribe ⇒ none; invalid configured URL refused at startup (fail fast, not a silent no-send); knob reaches production config (registryOptions test). |
| SP12 | `gawk-telemetry` ingest: CORS preflight + `Access-Control-Allow-Origin: *`, accept `text/plain` beacon bodies alongside `application/json` | Go test: OPTIONS answered with the CORS headers, POST responses carry them; identical envelope accepted under both content types; token verification and rate limits unchanged and still enforced under `text/plain`; dashboard/read listener serves **no** CORS headers (asserted — the never-public posture is visible in tests). |
| SP13 | App: 0x12 precedence (advertised wins, configured fallback on any relay — D15 as revised 2026-08-14), per-send re-resolution, `text/plain` unload beacon, indicator disclosure | Unit: precedence matrix incl. malformed-0x12-degrades, the configured-URL fallback with no 0x12 (pre-R37 behavior restored), a LATE 0x12 redirecting subsequent batches (G2), and the 250 ms floor clamping a below-floor hello (G3); unload flush uses a safelisted content type (no preflight dependency); indicator disclosure keys on normalized-URL equality, never entry id (G3). e2e (tier-1): the telemetry scenario pins the fallback path end-to-end (relay with no advertise URL, batches land at the configured collector); the advertised path is unit-covered on every side — its e2e needs an https ingest front the harness doesn't have. |

Docs & closure:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| SP10 | `docs/self-hosting.md` "using your relay from other gawk UIs" (+ directory hosting note + `telemetry.advertiseUrl` guidance incl. the verify-from-a-foreign-UI step), `docs/gotchas.md` sync for anything learned, manual verification pass | Self-hosting section covers `allowedOrigins`, `serverName`, directory hosting incl. CORS, and telemetry advertising. Manual pass: cross-relay share link end-to-end between two real deployments (reference fleet + a second relay), picker add/probe/select/remove, `allowCustomRelays: false` posture, link with unknown relay on a gated install, **and a foreign-relay session whose telemetry lands in that relay operator's dashboard** (§4.10). |

## 9. Verification

Automated gates are per-chunk above. The milestone-closing manual pass
(SP10) is the product claim end-to-end: broadcaster on relay B shares a link
into a UI deployed against relay A; a fresh viewer opens it, watches, saves
relay B, and joins by plain code after selecting it — with that session's
telemetry landing in relay B's operator's dashboard, not deployment A's
(§4.10) — while a second UI with
`allowCustomRelays: false` degrades the same link to its quiet note. Glass
latency and every existing flow (join-by-code on the default relay, R23
terms, R26 armed links once they exist) must be visibly unchanged on the
default path.
