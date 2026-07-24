# R23 — Terms & conditions / usage terms (docs/29)

**Status**: designed 2026-07-24; **TC1–TC5 implemented 2026-07-24, automated
gates green (gawk-app: tsc + 765 vitest tests + oxlint + vite build; helm lint
+ render validated; gawk-broadcast: go vet + gofmt + app tests + GUI builds),
manual browser verify pending** (see §10). Chunks **TC1–TC5** (`TC` =
Terms & Conditions; two-letter prefix per the R21+ convention — `NA`/`MF`/`DV`
are claimed). Ships as `gawk-app-vX.Y.Z` (+ a `gawk-broadcast` doc/GUI touch);
**frontend-only, zero server / wire / broadcaster-protocol changes** — the one
server-adjacent touch is a new `gawk-app` chart value + ConfigMap key, no relay
code.

> **Legal caveat (load-bearing, restated in the shipped text).** The default
> terms in §7 below are a **protective template written to the operator's
> stated priorities, not legal advice**, and Claude is not a lawyer. Before a
> deployment is exposed beyond a closed circle of known friends, the text is
> worth a pass by a Finnish lawyer — particularly the liability, consumer-law,
> and personal-data clauses, which interact with mandatory EU/Finnish consumer
> and GDPR provisions that no contract can waive. Shipping the template is a
> correct outcome; presenting it as vetted legal cover is not. This caveat is
> a design constraint, and it appears verbatim in the operator-facing chart
> README so an operator can never inherit the text without inheriting the
> warning.

---

## 1. Purpose

The platform relays whatever is on a broadcaster's screen to whoever holds the
join code — today with **no stated rules, no stated retention position, and
nothing telling an operator's friends what is and isn't acceptable to stream.**
R23 closes that gap:

- `gawk-app` states the terms under which a deployment may be used, reachable
  from every surface.
- A **broadcaster acknowledges them once** before their first broadcast (the
  action with consequences — you are publishing your screen).
- A **viewer can read them from any surface without a click-through gate** (a
  join link that opens a modal instead of the stream is exactly the friction
  R1/R6 spent their budget removing).
- An **operator can replace the text for their own deployment without forking
  the app**, reusing the existing runtime-config mechanism.

The terms are written to **protect the operator to the maximum a template
reasonably can**: no warranty, no guarantees, the broadcaster is solely
responsible for all content they transmit, that content must be lawful **both
in Finland and in the broadcaster's own jurisdiction**, and the operator
retains broad discretion to monitor, analyze, moderate, block, and remove
content to maintain service quality and to exercise its judgement over unfit
content.

Join-by-code UX (no accounts, no login) is a locked product decision, so the
terms must **work without an identity to bind them to** — the whole design
below assumes there is no user record, and never introduces one.

## 2. Decisions (locked with the owner, 2026-07-24)

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | **Governing law is a one-liner**: "These Terms are governed by the laws of Finland." No explicit forum/court clause. | Owner choice. Keeps the document light; Finnish/EU mandatory consumer law fixes forum for consumers regardless. |
| D2 | **No hard age gate and no recorded acceptance of age.** The *text* states users must be 18+, and that a user under 18 may use the Service only with the consent of a parent or guardian. | Owner choice: "mention in texts, no need to record in the system." Consistent with the no-identity model — there is nothing to attach an age assertion to. |
| D3 | **Monitoring posture: reserve broad rights *and* state current practice.** The operator reserves the right to monitor, inspect, analyze, record, retain, block, interrupt, and remove any content at its sole discretion; the text *also* states factually that, as currently operated, the Service does not persistently record or store broadcast media — framed explicitly as a description of present operation, **not a warranty or a commitment against change.** | Owner choice. Maximum operator latitude without an outright false "we never record" that the future R21 ring buffer or a moderation feature could contradict. |
| D4 | **English only in v1.** | Owner choice; matches the roadmap non-goal. Operators localize via the override. |
| D5 | **Broadcaster acknowledgment is a *blocking* modal before the first broadcast's transport connect**, one-time, versioned; **viewers are never gated.** | The roadmap's "gate belongs before connect, not between connect and picker" — placed at `beginStart` in `BroadcasterScreen`, ahead of the existing publish-secret prompt. |
| D6 | **Override mechanism = the existing runtime `config.js`/ConfigMap**, extended with terms metadata (version + operator identity + optional body URL); the long-form body is overridden by mounting a ConfigMap file over a shipped static asset, the *same pattern* already used for `config.js` over `public/config.js`. **A bundled default component is always present**, so an un-configured install and dev both render real content with zero network dependency. | Reuses `gawk-app/src/config.ts` + `templates/configmap.yaml` + `templates/deployment.yaml` verbatim in shape; no new server endpoint; the app's zero-network-before-connect startup is preserved (see D7). |
| D7 | **The terms version is an explicit string field, not a content hash.** Re-acknowledgment fires when `termsVersion` changes; typo fixes that leave the version untouched do not re-prompt. The body is **fetched on the `#/terms` route open and, for the override path, at acknowledgment time — never at app boot.** | Roadmap key design question; a hash would re-prompt on every whitespace edit. Fetch-on-demand keeps the boot path free of a 404-prone network call. |
| D8 | **The native broadcaster (R14) gets a one-line reference + link, not a gate.** | It publishes to the same relay under the same operator's terms, but it is a binary run on the user's own PC, outside the deployment and outside `gawk-app`. A GUI "About" line + a README paragraph is proportionate; a modal gate in a Gio app is not. |

## 3. Where it plugs into the existing app (grounded in the code)

Everything below already exists; R23 adds to it, it does not restructure.

- **Routing** — `gawk-app/src/routing.ts` is a pure, unit-tested hash parser
  (`parseRoute`) feeding `App.tsx`. Add one variant `{ view: 'terms' }` for
  `#/terms`; add the corresponding `case` in `App.tsx` rendering a new
  `TermsPage`. Pure addition, covered by `routing.test.ts`.
- **Landing** — `features/landing/LandingPage.tsx` (the `GlassPanel` card).
  Add an unobtrusive footer link ("Terms of use") below the "Start a stream"
  affordance. This is the viewer/first-time entry point.
- **Broadcaster** — `features/broadcaster/BroadcasterScreen.tsx`. The
  acknowledgment gate goes in `beginStart` (line ~221), which today decides
  between the secret prompt and `handleStart`; the terms check runs *first*.
  The existing Settings `GlassPanel` (`settingsOpen`, line ~279) gains a
  "Terms of use" link.
- **Viewer** — `features/viewer/ViewerScreen.tsx` already renders the "⋮" More
  options `ContextMenu` (`ui/ContextMenu.tsx`, docs/24 finding 9,
  `anchor: 'bottom-right'`). Add a `{ label: 'Terms of use', onSelect }` item
  that navigates to `#/terms`. **No new chrome on the cinematic view** — the
  requirement from the roadmap.
- **Runtime config** — `gawk-app/src/config.ts` (`GawkRuntimeConfig`,
  `window.__GAWK_CONFIG__`, read from the ConfigMap-rendered `/config.js`) and
  `deploy/charts/gawk-app/{values.yaml,templates/configmap.yaml}`. Extend the
  config interface and the ConfigMap; ship the default `public/terms.html`
  static asset and mount over it exactly like `public/config.js`.
- **Persistence** — `localStorage` under the existing `gawk:*` convention
  (`gawk:muted`, `gawk:playout-mode`, …). New key
  **`gawk:terms-accepted`** storing the accepted version string.

## 4. Data model & mechanism

### 4.1 Runtime config additions (`config.ts` + ConfigMap)

Extend `GawkRuntimeConfig`:

```ts
export interface GawkRuntimeConfig {
  // …existing fields…

  // R23 (docs/29). Bumping this re-prompts every broadcaster for
  // acknowledgment (D7). A date-stamp is the recommended form, e.g.
  // "2026-07-24". Defaults to BUNDLED_TERMS_VERSION when unset.
  termsVersion?: string;

  // Substituted into the bundled default text's placeholders so an operator
  // gets correct attribution + a contact point without editing prose.
  // Ignored when a full body override is supplied.
  operatorName?: string;     // e.g. "Juho's homelab"
  operatorContact?: string;  // e.g. "gawk@example.com"

  // Optional full-body override. When set, TermsPage renders this document
  // instead of the bundled default. Recommended shape: "/terms.html", a
  // ConfigMap-mounted static asset (see 4.3). An absolute URL is allowed but
  // then subject to the app's connect-src CSP.
  termsUrl?: string;
}
```

Accessors mirror the existing ones (`getMaxDecoderQueueSize`, `getDvrBufferMs`):

```ts
export const BUNDLED_TERMS_VERSION = '2026-07-24';

export function getTermsVersion(): string {
  return getRuntimeConfig().termsVersion ?? BUNDLED_TERMS_VERSION;
}
export function getOperatorName(): string {
  return getRuntimeConfig().operatorName?.trim() || 'the operator of this deployment';
}
export function getOperatorContact(): string | null {
  return getRuntimeConfig().operatorContact?.trim() || null;
}
export function getTermsUrl(): string | null {
  return getRuntimeConfig().termsUrl?.trim() || null;
}
```

ConfigMap (`templates/configmap.yaml`) gains the four keys under the existing
`window.__GAWK_CONFIG__` object; `values.yaml` gains a documented `config`
block. Only non-secret, client-safe values — same rule the ConfigMap comment
already states.

### 4.2 The bundled default vs. the override (D6/D7)

```
                       ┌ termsUrl set? ─ yes ─► fetch(termsUrl) on route open
 #/terms  ► TermsPage ─┤                          │  ok ─► render (sanitized) in R6 chrome
                       └ no ──────────────────────┤  4xx/5xx/timeout ─► render bundled default
                                                  (bundled default also renders while fetch is
                                                   in flight, so the page never shows blank)
```

- **`BundledTerms`** is a React component holding the §7 text, styled with the
  R6 long-form typography tokens (`styles/global.css`). It is always in the
  bundle: dev, un-configured installs, and every override-fetch failure fall
  back to it. Operator name/contact placeholders are filled from config.
- The **override** is an HTML *fragment* (not Markdown — avoids pulling a
  Markdown renderer into the bundle; HTML is what nginx already serves).
  `TermsPage` fetches it lazily and inserts it **sanitized** (a small
  allowlist sanitizer — headings, paragraphs, lists, emphasis, links, no
  scripts/styles/iframes/event handlers) inside the same page chrome, so an
  operator's document can never inject script into the SPA origin. This is the
  one security-review-worthy line in R23; the sanitizer is unit-tested against
  a script-injection corpus.
- **Boot is untouched.** Nothing about terms runs before the first user action
  reaches `#/terms` or `beginStart`; the roadmap's zero-network-before-connect
  startup concern is designed out by never fetching at boot and always having
  the bundled fallback.

### 4.3 Operator customization (chart)

Two override depths, both no-image-rebuild:

1. **Identity only** (the common case) — set `config.operatorName`,
   `config.operatorContact`, and bump `config.termsVersion` on any meaningful
   edit. The bundled default text renders with correct attribution.
2. **Full text** — mount a ConfigMap over `public/terms.html` (identical
   deployment wiring to the existing `config.js` mount) and set
   `config.termsUrl: /terms.html` + `config.termsVersion`. The operator now
   owns the whole document.

The shipped `public/terms.html` default is an **empty placeholder** (so a
default install with no `termsUrl` uses the bundled component, and an operator
who mounts over it gets their content). The chart README documents both depths
and carries the §0 legal caveat.

### 4.4 Acknowledgment gate (D5)

- Key: `gawk:terms-accepted` → the version string last agreed.
- `hasAcceptedCurrentTerms() === (localStorage['gawk:terms-accepted'] === getTermsVersion())`.
- In `beginStart` (before connect, before the secret prompt): if not accepted,
  open a **blocking** `GlassPanel` + scrim modal (the same primitive the
  Settings panel uses) — a short plain-language summary, a "Read the full
  terms" link to `#/terms`, and **Agree** / **Cancel**. Agree writes the key
  and continues into the existing start flow; Cancel aborts, status stays
  `idle`, nothing connects. Re-prompts automatically when `termsVersion`
  changes (the stored string no longer matches).
- **Viewers never hit this path** — `ViewerScreen` reads terms, never gates.

## 5. Chunk breakdown & acceptance criteria

Each chunk lands test-first per `CODE-REVIEW.md`. "Verified by" is the gate.

### TC1 — Terms surface (route + page + bundled default + entry points)

| Goal | Verified by |
|------|-------------|
| `#/terms` parses to `{ view: 'terms' }`; unknown routes still redirect home | `routing.test.ts` cases added |
| `TermsPage` renders the bundled default in R6 chrome; back-to-home affordance | component test; manual visual |
| Landing footer link, broadcaster Settings link, and viewer "⋮" menu item all navigate to `#/terms` | `LandingPage`/`ViewerScreen` tests; the viewer menu adds one `MenuItem`, no new chrome asserted |
| Non-gated surfaces are byte-identical except the added links (no viewer overlay/chrome change) | ViewerScreen snapshot/DOM diff limited to the menu item |

### TC2 — Operator customization (runtime config + override + fallback)

| Goal | Verified by |
|------|-------------|
| `GawkRuntimeConfig` gains `termsVersion`/`operatorName`/`operatorContact`/`termsUrl`; accessors default correctly | `config.test.ts` |
| `TermsPage` renders `termsUrl` content when the fetch succeeds; falls back to bundled on 4xx/5xx/timeout; never shows blank | `TermsPage` test with mocked fetch (ok / 404 / reject) |
| Override HTML is **sanitized** — script/style/iframe/`on*`/`javascript:` stripped | sanitizer unit test against an injection corpus; a `<script>` in the fixture never executes |
| Operator name/contact placeholders substituted in the bundled default | component test |
| `values.yaml` + `configmap.yaml` carry the keys; chart lints; README documents both override depths + the legal caveat | `helm lint`/template render; README review |

### TC3 — Broadcaster acknowledgment gate

| Goal | Verified by |
|------|-------------|
| First `beginStart` with no accepted terms shows the blocking modal; **no transport connect happens** until Agree | `BroadcasterScreen.test.tsx` — assert no session created on mount/Cancel |
| Agree persists `gawk:terms-accepted = version` and proceeds into the existing start (then the secret prompt if configured) | test: localStorage write + `handleStart` reached in the right order |
| Already-accepted current version → no modal, straight through | test |
| Bumping `termsVersion` re-prompts a previously-accepted broadcaster | test with changed config |
| Cancel leaves status `idle`, no zombie session | test |

### TC4 — Default terms text (the writing deliverable)

| Goal | Verified by |
|------|-------------|
| The §7 text is embedded as `BundledTerms` and matches this doc | content review against §7 |
| Covers: acceptable use; content relayed to anyone with the code; broadcaster solely responsible; lawful in Finland **and** the broadcaster's own country; 18+ / under-18 parental consent (D2); broad monitoring/moderation rights + current-practice recording note (D3); "as is" / no warranty; limitation of liability; indemnification; suspension/termination; changes; light personal-data note; Finnish governing-law one-liner (D1); operator contact | clause-by-clause checklist below |
| The §0 legal caveat is reproduced in the chart README | README review |
| Recording statement stays accurate if R21 (DVR ring) lands — worded as short-lived in-memory, present-operation, not a warranty | cross-check against docs/26 |

### TC5 — Native broadcaster reference (R14)

| Goal | Verified by |
|------|-------------|
| `gawk-broadcast` GUI shows a one-line "By broadcasting you accept the operator's terms of use" with the deployment's `#/terms` URL (derived from the relay/app host) | manual GUI check |
| `gawk-broadcast/README.md` carries a short terms paragraph | README review |
| **No modal gate** added to the native app | code review (D8) |

TC5 is droppable/deferrable independently — it touches a separate module and
is a documentation-grade change, not a product gate.

## 6. Non-goals (roadmap-confirmed)

- Accounts, signatures, or any **server-side record of acceptance** — there is
  no identity to attach it to (R2 non-goals stand).
- A **cookie banner** — the app sets no cookies and runs no analytics; the
  only client storage is functional `localStorage` (`gawk:*`), which the terms
  disclose.
- **Per-viewer legal gating** / click-through for viewers.
- **Localization beyond English** in v1 (D4).
- **No relay/wire/broadcaster-protocol change.** R23 is `gawk-app` + one chart
  value; the relay never learns what the terms say.

## 7. The default terms text (`BundledTerms`)

> Rendered with `{{operatorName}}` / `{{operatorContact}}` substituted from
> runtime config; where no contact is configured, the contact line reads "the
> operator of this deployment." Version and effective date come from
> `termsVersion`.

---

### Terms of Use

**Effective version: {{termsVersion}}**

These Terms of Use ("**Terms**") govern your access to and use of this
self-hosted streaming service (the "**Service**"), which is operated by
**{{operatorName}}** (the "**Operator**"). The Service lets a broadcaster share
their screen and lets viewers watch it in a web browser by entering a join
code. The Service uses no accounts and no login. **By using the Service — as a
broadcaster or as a viewer — you agree to these Terms. If you do not agree, do
not use the Service.**

**1. The Service, provided "as is."** The Service is a private, self-hosted
deployment made available at the Operator's discretion, primarily for use among
people known to the Operator. It is provided **"as is" and "as available," with
no warranties of any kind**, whether express, implied, or statutory, including
without limitation any implied warranties of merchantability, fitness for a
particular purpose, non-infringement, availability, uptime, quality, or
reliability. The Operator does not warrant that the Service will be
uninterrupted, timely, secure, error-free, or that any content will be
transmitted without delay, loss, or degradation.

**2. Eligibility.** You must be at least **18 years of age** to use the
Service. If you are under 18, you may use the Service **only with the consent
and under the supervision of a parent or legal guardian**, who accepts these
Terms on your behalf and is responsible for your use. By using the Service you
represent that you meet these requirements.

**3. How the Service works; the join code.** Starting a broadcast produces a
short join code. **Anyone who holds the join code can view the broadcast.** The
code is the only access control for viewing; the Operator does not verify the
identity of viewers. You are responsible for deciding with whom you share a
join code and for the consequences of sharing it.

**4. Your content and your responsibility for it.** As a broadcaster, you are
**solely and fully responsible for everything you transmit through the
Service** — your screen, applications, audio, and anything visible or audible
within them (your "**Content**"). You represent and warrant that you have all
rights, licenses, and permissions necessary to transmit your Content and to
allow it to be viewed by those who hold the join code, and that your Content
does not infringe or violate the rights of any third party.

**5. Lawful use.** You may transmit only Content that is **lawful both in
Finland and in the country or jurisdiction from which you use the Service.**
You are responsible for knowing and complying with the laws that apply to you.
Without limiting the foregoing, you must not use the Service to transmit,
request, or facilitate any Content or conduct that:

- is illegal, or promotes, facilitates, or depicts illegal acts;
- infringes any copyright, trademark, trade secret, privacy, publicity, or
  other right of any person;
- discloses another person's private or personal information without their
  consent;
- is defamatory, harassing, threatening, hateful, or intended to intimidate;
- is sexually explicit involving minors, or exploits or endangers minors in any
  way;
- contains malware, or is intended to disrupt, damage, or gain unauthorized
  access to any system, network, or data;
- attempts to circumvent, overload, or interfere with the Service, its limits,
  or its security; or
- violates any applicable law or regulation.

**6. Monitoring, moderation, and content management.** The Operator reserves
the right, **at its sole discretion and without notice or liability, to
monitor, inspect, analyze, record, retain, block, interrupt, throttle,
suspend, or remove any Content or broadcast, and to restrict or terminate any
person's access to the Service**, for any reason — including to maintain
service quality, to enforce these Terms, to comply with law, and to exercise
the Operator's own judgement as to what is unfit for the Service. The Operator
is under **no obligation** to monitor Content, and any decision to do so, or
not to, does not create any duty or liability.

**7. Recording and data.** *As the Service is currently operated,* it relays
broadcast media in real time and **does not persistently record or store
broadcast media**; any technical caches used to relay a stream are short-lived,
held only in memory, and not retained. **This paragraph describes present
operation only. It is not a warranty and not a commitment against change**, and
it does not limit the rights the Operator reserves in Section 6. The Service
may process limited technical data necessary to operate it (for example, join
codes and network connection information such as IP addresses, which may appear
transiently in operational logs). To the extent any such data is personal data,
it is processed only to provide and protect the Service; questions about
personal data may be directed to the contact in Section 13.

**8. Limitation of liability.** To the maximum extent permitted by applicable
law, the Operator (and anyone involved in providing the Service) shall **not be
liable** for any indirect, incidental, special, consequential, exemplary, or
punitive damages, or for any loss of profits, data, goodwill, or other
intangible losses, arising out of or relating to your use of or inability to
use the Service, whatever the cause and under any theory of liability. To the
maximum extent permitted by applicable law, the Operator's total aggregate
liability arising out of or relating to the Service is limited to **zero
euros**. Nothing in these Terms excludes or limits liability that cannot be
excluded or limited under mandatory applicable law.

**9. Your indemnity.** To the maximum extent permitted by applicable law, you
agree to **indemnify, defend, and hold harmless the Operator** from and against
any claims, demands, damages, losses, liabilities, costs, and expenses
(including reasonable legal fees) arising out of or relating to your Content,
your use of the Service, or your breach of these Terms or of any law or
third-party right.

**10. Availability, suspension, and termination.** The Service may be changed,
limited, suspended, or discontinued at any time, in whole or in part, with or
without notice. The Operator may **deny, suspend, or terminate access for
anyone at any time and for any reason**, including suspected violation of these
Terms. There is no entitlement to the Service and no guarantee of continued
availability.

**11. Changes to these Terms.** The Operator may update these Terms from time
to time. Material changes are indicated by a change to the effective version
shown above, and continued use of the Service after such a change constitutes
acceptance of the updated Terms.

**12. Governing law.** These Terms are governed by the laws of Finland.

**13. Contact.** Questions about these Terms or the Service may be directed to
**{{operatorContact}}**.

**14. Miscellaneous.** If any provision of these Terms is held unenforceable,
the remaining provisions remain in full force. The Operator's failure to
enforce any provision is not a waiver of it. These Terms are the entire
agreement between you and the Operator regarding the Service and supersede any
prior understanding on that subject. Section headings are for convenience only.

---

## 8. Verification plan

- **Automated (CI gate):** `routing.test.ts`, `config.test.ts`,
  `TermsPage`/sanitizer tests, `BroadcasterScreen.test.tsx` gate ordering,
  `helm lint`/template render, full `tsc`/vitest/lint/build green.
- **Manual browser (`gawk-app:verify` / the R20 tier-1 harness):**
  - viewer surface — `#/terms` reachable from the "⋮" menu; the cinematic view
    is otherwise unchanged; no gate on join;
  - broadcaster — first start shows the acknowledgment before any connect;
    Agree proceeds and does not re-prompt; a `termsVersion` bump re-prompts;
  - override — set `termsUrl` to a mounted `/terms.html` and confirm it renders
    sanitized; point it at a 404 and confirm the bundled default renders;
  - a `<script>` in the override fixture does **not** execute (CSP + sanitizer).
- **Operator runbook:** the chart README's two-depth customization + the legal
  caveat, dry-run against a values override.

## 9. Open items deliberately deferred

- **Suggest/allow multiple languages** — deferred (D4); the `termsUrl` override
  already lets an operator serve any language today.
- **A viewer-facing "you are watching a private stream" one-liner** distinct
  from full terms — possible R6-polish follow-up, not part of R23.
- **Machine-readable acceptance receipts** — out of scope; contradicts the
  no-identity model (non-goal).

## 10. Implementation status (2026-07-24)

TC1–TC5 landed together; automated gates green in both modules. Deviations
from the design above, all immaterial:

- **New feature dir** `gawk-app/src/features/terms/`: `TermsPage.tsx` (route
  chrome + override fetch/fallback), `BundledTerms.tsx` (the §7 default text),
  `sanitize.ts` (+ `sanitize.test.ts`, the XSS corpus), `acceptance.ts` (+
  test), `terms.module.css`.
- **Override fetch happens only on the `#/terms` route open**, not also at
  acknowledgment time as §4.2's diagram allowed — the acknowledgment modal
  shows a short summary + a link, so it never needs the full override body. Boot
  stays fetch-free (the load-bearing property is preserved).
- **`getTermsVersion` treats an empty string as unset** (falls back to
  `BUNDLED_TERMS_VERSION`), so the ConfigMap renders an empty `termsVersion`
  default instead of duplicating the version constant across app + chart.
- **Chart**: `config.termsHtml` → ConfigMap `terms.html` key → mounted over
  `public/terms.html`; opting in is `config.termsUrl: /terms.html`. A default
  install renders the bundled default and issues **no** `/terms.html` fetch.
  `toJson` quoting verified against hostile operator names (quotes/`&`).
- **Viewer menu opens `#/terms` in a new tab** (`window.open`), not an in-place
  hash change — reading the terms never tears down the live view.
- **TC5**: `internal/app.TermsLink()` derives `<app-url>/#/terms` (mirrors
  `JoinLink`); the Gio GUI settings card shows a `Caption` notice + the link
  when an app URL is set; `gawk-broadcast/README.md` gained a Terms section.
  No native gate (D8).
- The sanitizer returns `''` without a DOM (SSR/node), so its tests carry the
  `@vitest-environment jsdom` directive (as do `acceptance`/`TermsPage`).
