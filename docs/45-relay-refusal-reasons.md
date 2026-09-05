# R43 — Relay refusal reasons the browser can see (docs/45)

**Status**: designed 2026-09-05; **not started**. Chunks **RR1–RR5** (`RR` =
Refusal Reasons; two-letter prefix per the R21+ convention). Non-mandatory:
nothing is broken, the relay's answers are precise today — it is the browser
that cannot read them. Proposed as a follow-up to R42, not part of it: it
changes a relay behaviour every client depends on and touches all four wire
mirrors.

## 1. Purpose

Every refusal the relay hands a client today is an HTTP status on the
`CONNECT` request, written before the WebTransport upgrade: 429 at
capacity, 401 for a wrong publish secret, 404 for an unknown broadcast,
451 for a ban (docs/07, docs/06 §4, docs/42 D15). That is the right answer
for the native broadcasters, which read `rsp.StatusCode` (`gawk-broadcast`
`internal/engine/relay.go:185`, `gawk-broadcast-windows`
`crates/engine/src/resume.rs` `resume_terminal`) and turn it into a
sentence for the user.

The browser cannot read it. The WebTransport JS API exposes neither the
status nor the body of a refused `CONNECT`: `ready` rejects with a bare
`WebTransportError` (the viewer already knows — `ViewerScreen.tsx:174`, "a
WebTransportError hides the HTTP status"). So in the browser every one of
those refusals reads the same: **"WebTransport connection rejected"** on the
broadcaster, **"Streamer offline"** on the viewer. The trigger for this
document was a relay at its broadcast cap on the dev stack (2026-09-05): the
relay log said `hub: max concurrent broadcasts reached`, the broadcaster
page said nothing useful, and the operator had to read the container log to
find out.

There is exactly one channel that carries a reason to a browser: an
**accepted session that the relay then closes with an application close
code and a reason string**. `wt.closed` resolves with `{closeCode, reason}`
in every engine. The relay already uses it — for the *race* case only:
`server.go:909` `sess.CloseWithError(429, "max concurrent broadcasts
reached")` when a mint loses the capacity re-check after the upgrade.

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Policy refusals on `/publish`, `/publish/{id}` and `/subscribe/{id}` are answered after the upgrade, with a close code + reason.** Policy = capacity (per-pod and cluster-wide), wrong or missing publish secret, unknown broadcast ID, ban (451 today), invalid resume token. | It is the only reason-bearing channel a browser has (§1). The relay's *decision* does not change — the same checks run, in the same order — only the *envelope* it is delivered in. |
| D2 | **Rate limiting stays pre-upgrade** (`rateLimited()` → 429, no session). | It is the abuse gate. A refused `CONNECT` costs one HTTP/3 request; an accepted-then-closed session costs a QUIC handshake and a session object. Keeping the limiter in front bounds the cost of D1 to one short-lived session per *legitimate* refusal, which the limiter already caps per source. Draining (503) also stays pre-upgrade: kube-proxy has already stopped sending new flows (docs/22), and a client that still arrives should retry elsewhere, not read a sentence. |
| D3 | **New close codes in `wire.go`, not the HTTP numerics.** `4008 AtCapacity`, `4009 Unauthorized`, `4010 NotFound`, `4011 Forbidden` (bad resume token / wrong attach secret); the existing `4006 TerminatedByOperator` covers the ban case on this path too. The ad-hoc `CloseWithError(429, …)` / `500` sites become these. | Close codes are what every client keys on (`terminalPublisherMessage`, `closeCodeError`, `close_code_message`) and they are already mirrored with golden vectors in all four implementations; a bare HTTP numeric in the close-code space is a convention leak that the R2 review let through because it was a race path nobody expected to see. |
| D4 | **The reason string is diagnostic, not UI copy.** Clients render the sentence for the *code* (their existing tables, extended) and log the string. | The string is free text from a server the client may not trust (a third-party relay, docs/40); the code is the contract. It also keeps the three broadcasters reading identically — the R17/R39 rule that "the same close code must read the same on all three". |
| D5 | **A close before the announce is a connect failure, not a session death.** In the browser, `BroadcastPipeline.connectTransport` treats a `closed` that settles before the first server message as `BroadcastStartError('connect', …)` with the code's sentence; `handleSessionGone` never sees it, so no resume is scheduled. The viewer's `connect` does the same before the first datagram/keyframe. | Today a post-upgrade 429 would enter `handleSessionGone`, and 429 is not in `isTerminalPublisherClose`, so the browser would *retry* against a full relay on the reconnect schedule. D1 makes that path the normal one, so it must be terminal-for-this-attempt. |
| D6 | **Natives add the new codes to their terminal sets** (`terminalForPublisher`, `terminal_for_publisher`) and their message tables, and keep their HTTP-status handling for anything still answered pre-upgrade (rate limit, drain). | They lose the status they read today on the policy paths and gain the same sentence through the channel they already have for it. Both engines already run the "close code → sentence" path for 4000/4004/4006. |
| D7 | **The R37 secret prompt becomes exact.** `BroadcasterScreen`'s "a secret-less connect to a non-default relay failed → prompt for a secret" heuristic (docs/40 §4.2 F3) is keyed on `4009` instead of "any connect failure on a foreign relay". | The heuristic exists only because the status was invisible. With the code in hand, a full relay no longer prompts for a secret. |
| D8 | **docs/07 records the reversal.** Its "capacity rejection moved pre-upgrade (HTTP 429, symmetric with subscribe)" decision was correct for what could be observed at the time; the new evidence is §1. | The R2 decision is cited in three places; leaving it unqualified would send the next reader to re-derive this. |

**Rejected**: a preflight HTTP GET (`/capacity`, or reading `/statusz`)
before dialing — the ops listener is never public (CLAUDE.md), the relay's
h3 endpoint serves no plain GET the browser could reach through a
self-signed cert in dev, and it would race the real answer anyway. A
reason-only header on the `CONNECT` response — the browser cannot read
response headers of a refused `CONNECT` either.

## 3. Where it plugs in

| Piece | Where it is today | What RR changes |
|---|---|---|
| Publish refusals | `internal/transport/server.go` `handlePublish`: 401 secret (`:742`), 404 / 403 / 429 / 400 on the claim path (`:773–822`), pre-upgrade `CheckPublishNew` → 429 on mint (`:886`), post-upgrade `CloseWithError(429/500, …)` (`:909, :928`) | RR2: the policy branches move after `Upgrade`; the numerics become D3 codes. The rate limiter and drain checks stay where they are. |
| Subscribe refusals | `handleSubscribe`: 404 / 429 pre-upgrade via `CheckSubscribe` (`:1268–1277`), post-upgrade `CloseWithError(429, …)` and `CloseCodeBroadcastEnded` (`:1342–1345`) | RR2 likewise; the `ErrNotFound`-after-upgrade → 4000 rule (docs/06 §4) is unchanged. |
| Close codes | `wire/wire.go` 4000–4007; mirrors `wire.ts`, `internal/wirecheck`, `crates/wire` | RR1 allocates 4008–4011 with doc comments in the existing style and vectors in every mirror. |
| Browser connect | `transport/broadcaster.ts` `connectTransport` / `handleSessionGone` / `terminalPublisherMessage`; `transport/viewer-transport.ts` `reportClosed`; `features/viewer/ViewerScreen.tsx` `errorCardCopy` | RR3 (D5, D7): the pre-announce close becomes a connect error with the code's sentence; error cards gain "This relay is full", "This relay needs a publish secret", "No such broadcast". |
| Natives | `gawk-broadcast/internal/engine/resume.go` `terminalForPublisher`, `closeCodeError`; `crates/engine/src/resume.rs` `terminal_for_publisher`, `close_code_message` | RR4 (D6). |
| Docs | docs/07 §"Rate Limiting Status Code" and §post-review note; docs/06 §4 route table | RR5 (D8). |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **RR1** | Close codes 4008–4011 in `wire.go` with doc comments; mirrored in `wire.ts`, `wirecheck`, `crates/wire`; golden vectors byte-identical | The four mirror suites; the Windows CI job runs on the `gawk-server/wire/**` trigger |
| **RR2** | Relay: policy refusals after the upgrade (D1), rate limit and drain untouched (D2), D3 codes everywhere a numeric was used | `transport` tests: each refusal yields an accepted session closed with the expected code and a non-empty reason; a rate-limited dial still yields HTTP 429 with no session; `/statusz` connection outcomes unchanged (`OutcomeLimitRejected` etc. keep counting) |
| **RR3** | Browser: pre-announce close → connect failure with the code's sentence, no resume scheduled (D5); viewer error cards per code; the secret prompt keyed on 4009 (D7) | `broadcaster.test.ts`: a 4008 close before the announce rejects `start()` with `BroadcastStartError('connect')` and schedules no reconnect; `ViewerScreen` card copy per code; `BroadcasterScreen.test.tsx`: 4008 on a foreign relay does *not* open the secret prompt, 4009 does |
| **RR4** | Natives: the codes are terminal and read the same sentences (D6) | `resume_test.go`, `resume.rs` tests extended: the terminal set is exactly {4000, 4004, 4006, 4008–4011}; the sentence table covers each |
| **RR5** | docs/07 reversal note, docs/06 route table, `docs/gotchas.md` entry ("the browser cannot see a refused CONNECT's status") | Review |

Success criterion, end to end: on the dev stack with `MAX_BROADCASTS=1` and
one pubsim running, `#/broadcast` → Start shows **"This relay is full"**
on the error card, with no reconnect attempts in the console, and the
relay log still carries the same warning line it does today.

## 5. Security considerations

- The reason string is never rendered (D4); a hostile relay cannot put
  words in the UI.
- D1 does not widen what an unauthenticated client can learn: 401 vs 429
  vs 404 were already distinguishable to any native client.
- The cost of an accepted-then-closed session is bounded by the rate
  limiter (D2). The connection-rate defaults (docs/07) are per source
  address; a distributed dial storm costs the relay the same handshakes
  it does today for *accepted* sessions, which is the capacity the
  limits are already sized for.
