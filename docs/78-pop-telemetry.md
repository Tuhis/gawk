# R77 — PoP telemetry: home mints, the PoP reports (docs/78)

**Status**: designed and implemented 2026-10-24 (PT1–PT6) from an owner
discussion (OD1–OD3) and a read of the code (§2); every automated row in §5
is green. Open: a hardware party (a real venue line) to see the hellos and
observations end to end. Chunk prefix `PT` (PoP Telemetry).

Touches `gawk-server` (the public `wire` package, the PoP link, home's
`popsrv`, the PoP's `popclient`, the transport's two hello sites, the ops
listener, `RelayProfile`) and `gawk-telemetry` (the ingest, session storage,
the rollup, the relay scrape and the joins). No client changes: browsers,
the desktop broadcasters and the iOS app see an ordinary telemetry hello.

---

## 1. Why

R74's PoP relays (docs/76) serve party broadcasts and home broadcasts to a
venue's LAN, and report nothing: docs/76 §8 keeps every fleet secret off a
PoP, the telemetry key included, so a PoP cannot mint a telemetry token,
sends no telemetry hello, and its viewers and broadcasters collect nothing.
Its own relay statistics are unreachable too: its ops listener is loopback
only, behind the venue's NAT. docs/77 D15 and D16 keep PoP traffic out of
home's data; this milestone adds PoP data, without giving a PoP the key.

## 2. What the code does today (read 2026-10-24)

- **Tokens** (`gawk-server/wire/telemetry_token.go`): `expHour ‖ nonce (12 B)
  ‖ tag (8 B)`, `tag = HMAC(telemetryKey, expHour ‖ nonce ‖ broadcastKey ‖
  role)[:8]`. Stateless: any relay pod mints, the ingest verifies with one
  HMAC. Whoever holds the key can mint for any broadcast and role.
- **Hello sites**: two, both synchronous at session start —
  `handlePublish` and `handleSubscribe` (`internal/transport/server.go`).
  Clients accept a hello whenever it arrives (the desktop shell begins its
  reporter on the event; `gawk-app` on the worker message).
- **Broadcast keys differ**: a PoP's registry obfuscates IDs with its own
  stats key, home's with the fleet's, so a token that verifies under home's
  key must be minted over home's `ObfuscateID(id)`.
- **The link** (`internal/popsrv`, `internal/popclient`, `internal/poplink`):
  a Go↔Go record protocol, additive (unknown record types are ignored),
  with request/answer precedent (`reserve` → `reserved`), per-link `uid`,
  and periodic `stats`. The link-home pod knows each PoP's reservations
  (`Reservations.Owner`) and, fleet-wide, the broadcasts it holds downlink
  legs for (`popSlots`).
- **`/statusz` is public**: one handler serves the public HTTP/3 listener
  and the ops listener, so nothing PoP-identifying may be added to it.

## 3. Owner decisions (2026-10-24)

| # | Question | Decision |
|---|---|---|
| OD1 | How does a PoP get tokens without the key? | **Home mints them over the link** (option A), not a per-PoP derived key (option B: offline, but a new token format, client changes, per-PoP revocation state at the ingest, and a compromised PoP could mint for any broadcast). |
| OD2 | How is a PoP session marked? | **An issuer key inside the nonce**, authenticated by the existing tag (no format or client change). |
| OD3 | Scope | **Both phases now**: client telemetry from PoP sessions, and the PoP's relay-side observations, so PoP sessions join their relay view as home sessions do. |

## 4. Decisions

### D1 — The key stays home; home mints for the PoP

The PoP asks home for tokens over its link; home mints them with the fleet
key, over home's broadcast key, and returns them with the hello's other
fields. The ingest's verification is unchanged. A revoked or re-paired PoP
has no link and gets no tokens; ones already issued expire within
`TelemetryTokenTTL` (24 h), the exposure any token has today.
`RelayProfile` refuses `-telemetry-key`, so the key cannot be put on a PoP
by hand either.

### D2 — Link records

Additive, in `internal/poplink` (not a wire type; no mirrors):

- `config.telemetry`: `{reportIntervalMs, advertiseURL}` while home collects
  telemetry; absent otherwise (the PoP then sends no hello, as today).
- PoP → home `telemetryMint {id, role, n}`: up to `n` (≤ 8) tokens.
- Home → PoP `telemetryTokens {id, role, broadcastKey, tokens[]}`, or
  `telemetryRefused {id, role, reason}` (`scope`, `rate`, `off`).

### D3 — The issuer key rides the nonce

```
nonce = 1 ‖ check (15 bits) ‖ issuerKey (3 B) ‖ random (7 B)
issuerKey = HMAC(telemetryKey, "gawk-telemetry-pop\x00" ‖ pop uid)[:3]
check     = HMAC(telemetryKey, "gawk-telemetry-issuer\x00" ‖ issuerKey)[:2] & 0x7FFF
```

Home's own tokens clear the top bit. After the tag verifies, the ingest
reads the issuer (`wire.TelemetryTokenIssuer`); the keyed check stops a
random nonce from an older pod naming one (1 in 2^15). PoP sessions keep
56 random bits (collision-free to about 2^28 PoP sessions). The issuer key
names no PoP to anyone without the fleet key; the name comes from D7.

### D4 — What home will mint

- **Broadcaster** tokens only for IDs the PoP has reserved
  (`Reservations.Owner(id) == pop`).
- **Viewer** tokens for those, and for home broadcasts the PoP holds a
  downlink leg for (the link-home pod's `popSlots`). The PoP opens the leg
  before its viewer's hello, so the slot exists by then.
- **Rate**: a token bucket per PoP (10 a second, burst 100). Refusals are
  answered, counted (`gawk_pop_telemetry_refused_total{reason}`) and never
  fatal.

### D5 — The PoP's pool

Per `(id, role)` the PoP keeps up to 4 unused tokens, asks for more below 2,
and drops a pool nobody has drawn from for 10 minutes. A refusal is asked
again every 500 ms within the session's wait: a new party broadcast's
reservation, or a downlink viewer's leg slot, reaches the link-home pod a
moment after the session starts, and a `scope` refusal in that moment is
not the answer. A session takes one token; when
the pool is empty it waits up to 5 s in its own goroutine and then sends the
hello late — or not at all — never delaying the session. The hello and the
ingest-URL record are byte-identical to a home pod's.

### D6 — Rollout

Older homes ignore `telemetryMint` and send no `config.telemetry`; older
PoPs ignore `config.telemetry`. Both degrade to today's "no telemetry from
PoPs". Tokens from home pods that predate D3 carry a random top bit; the
keyed check keeps them home's.

### D7 — The relay side: PoP observations through home

- The PoP sends `telemetryObs {broadcasts: [{id, stats}]}` every
  `StatsEvery` (10 s) while `config.telemetry` is set: its hub's `/statusz`
  entry for each broadcast it holds (the fields `gawk-telemetry` reads), by
  raw ID, which the link already carries in `stats`.
- The link-home pod keeps the latest per PoP and serves them on a new
  **ops-only** route, `GET /internal/telemetry/pops`:
  `{pops: {<issuerKey>: {displayName, atMs, broadcasts: {<home broadcast key>: stats}}}}`
  — keyed by home's obfuscation, never a raw ID, never the wall code. 404 on
  pods that hold no link and on relays that predate it.
- `gawk-telemetry`'s scrape reads it beside `/statusz` and stores the
  observations with `pod = "pop:<issuerKey>"` and `role = "pop-origin"` or
  `"pop-edge"` (the PoP hub's own role), plus the display name.

### D8 — The joins keep home and PoP apart

- A session with an issuer joins only its PoP's observations
  (`pod == "pop:" + issuer`); a home session never joins a PoP's.
- The live projection's broadcast aggregation (origin, `viewersGlobal`,
  ingress) reads home pods only; PoP observations feed only the sessions
  they issued.
- The rollup row's `issuer` (docs/77-era field, D3) and `relayPod`
  (`pop:<issuerKey>`) say where a session was served; `config.pop` carries
  the display name when the observations had one.
- `leg-a-*` rules on a PoP-issued broadcaster session read the PoP's ingress
  window — which **is** that broadcaster's leg A (LAN to PoP).

## 5. Chunks and acceptance criteria

| Chunk | What | Acceptance | Verified by |
|---|---|---|---|
| **PT1** | `wire`: issuer tokens (D3) | Issuer tokens verify as any token's; the issuer is read back; home tokens never name one; another key, a corrupted check or a bare marker bit names none; golden vectors unchanged | `wire/telemetry_issuer_test.go`; `TestGoldenTelemetrySessionToken` (Go) and the TS golden |
| **PT2** | Home mints (D1, D2, D4) | Over a real home+PoP fleet, a party broadcaster's and a party viewer's hello carry a token that verifies under home's key over home's broadcast key and names the PoP's issuer; a downlinked home broadcast's PoP viewer gets one too; a broadcaster token for an ID the PoP does not own is refused (`scope`); the bucket refuses past its burst (`rate`); `RelayProfile` refuses `-telemetry-key` | `transport` `TestPopSessionsGetHomeMintedTelemetry`, `TestHomeMintsOnlyWithinScopeAndRate`; `config` `TestParseRelayFlagsRejects` |
| **PT3** | The PoP's pool and hello (D5) | A session's hello and ingest URL arrive from home's tokens without delaying the session; a refusal while a reservation or leg slot is still propagating is retried within the 5 s window; no telemetry configured sends no hello (today's behaviour) | `TestPopSessionsGetHomeMintedTelemetry` (a new broadcaster's first mint races its reservation); `TestAPopOfAHomeWithoutTelemetrySendsNoHello` |
| **PT4** | Ingest and storage (D3) | The issuer comes from the verified token only, never the envelope; it is stored on every line and the rollup row and recovered from disk | `ingest_test.go`, `sessions/issuer_test.go`, `storedshape` golden |
| **PT5** | PoP observations through home (D7) | The PoP's per-broadcast stats reach the link-home pod's `/internal/telemetry/pops`, keyed by home's broadcast key and the issuer key, with no raw ID or wall code anywhere in the body; `/statusz` is unchanged; other pods 404 | `popsrv` fleet test, a body scan for the raw ID and wall code |
| **PT6** | Joins (D8) | `gawk-telemetry` stores PoP observations under `pop:<issuer>`; a PoP-issued session gets relay coverage, relay facts and `config.pop` from its PoP only; a home session of the same broadcast never reads them; the live broadcast aggregation and peer medians are home's, and a PoP viewer gets its own | `relayscrape` `TestScrapeStoresPopObservationsAsThePops`; `readapi` `TestSessionsJoinOnlyTheRelayThatServedThem` (fails without the filter); `live` `TestPopObservationsFeedOnlyTheirSessions` |

## 6. Non-goals

- **Offline telemetry**: with the venue's internet down there are no tokens,
  and clients could not reach the ingest anyway.
- **Scraping PoPs directly**: home forwards what the link already carries.
- **Naming PoPs on public surfaces**: the display name appears only on the
  ops route and in the telemetry service, never in `/statusz` or an event.

## 7. Risks

| Risk | Answer |
|---|---|
| A compromised PoP fills telemetry for broadcasts it serves | Bounded by D4's scope and bucket, and every record is marked with its issuer, so it can be filtered or dropped by issuer. |
| A hello arrives late | Clients begin on arrival; at most the session's first seconds go unreported. |
| `telemetryObs` volume | One record per 10 s per PoP, a few KB per broadcast; capped by the PoP's broadcast limits. |
