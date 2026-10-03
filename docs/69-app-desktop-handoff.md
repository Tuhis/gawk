# R67 — Open a broadcast in the desktop app from `gawk-app` (docs/69)

**Status**: proposed 2026-10-03. Owner decision OD3 (docs/68 §2) was taken
the same day. Chunks **HO1–HO4** (§9) are not started. **Depends on R66**
([docs/68](68-desktop-gawk-links.md)): HO1 restates R66 LH1's vectors, and
HO3's automatic mode waits until a desktop release with LH2–LH5 is out.
Status lives in [`ROADMAP.md`](../ROADMAP.md).

**Relationship to earlier work**

- R66 makes the desktop apps handle `gawk://broadcast?room=&nick=&relay=`
  and defines the grammar (docs/68 D1). This milestone is the browser
  side: the SPA offers that link where the user is about to start a
  broadcast.
- Today the handoff goes only the other way. The desktop apps open the
  SPA at `#/room/<code>?rt=<grant>` ("Open room view", docs/44 §4.8).
- The SPA already points broadcasters at the native apps in one place: the
  "Sharing tips" `NATIVE_TIP` (`captureGuidance.ts`), linking to the site's
  download section (docs/46). That tip still says "for Linux or Windows",
  which predates macOS (R52). HO2 fixes the wording.

---

## 1. Why, and what "done" means

The common way into a broadcast is a room's "Start streaming" link (the
Mumble bot's button, `#/broadcast?room=<code>`) or the "Start streaming
here" button inside a room. Both always land in the browser broadcaster.
If you have the desktop app, the better capture path is one click away,
but nothing offers it.

The SPA **cannot tell whether the desktop app is installed**. No browser
API answers that. Timing heuristics (blur/visibility after a launch
attempt) are unreliable and fight the browser's own "Open …?" prompt, and
a loopback probe is blocked and unsafe (docs/68 §3). So the design never
guesses. It offers the app, keeps the browser flow intact underneath, and
remembers what the user chose.

### Milestone acceptance criteria

| # | Goal | Verified by |
|---|---|---|
| G1 | On Windows, macOS and Linux, the broadcaster's pre-start card and the room's "Start streaming here" offer **Open in the desktop app**. The link carries the room, nickname and non-default relay, and never a grant or secret. | unit tests + HO4 |
| G2 | Clicking it with the app installed opens the app prefilled (R66 G1/G2). Without the app, the page stays put with its state intact, and a **Didn't open?** note offers the download and **Continue in the browser**. | HO4, per browser (V-1) |
| G3 | The offer never appears on Android, iOS or ChromeOS, on viewer pages or on the landing page | unit tests |
| G4 | After the user picks **Always use the desktop app**, a broadcast-intent link opens the app on its own, at most once per page load. The page still shows the fallback and a one-click **Stop opening the app**. | unit tests + HO4 |
| G5 | `gawk-app` builds links that are byte-identical to R66's canonical `to_gawk` for every vector | unit test on the restated vectors |
| G6 | An operator can turn the offer off with `config.desktopHandoff: false`, plumbed through the chart | chart template test + unit test |
| G7 | The browser broadcast flow is unchanged when the offer is ignored: no extra prompt, and Start is the primary action | existing BroadcasterScreen tests stay green + new ones |

## 2. Owner decisions

| # | Decision |
|---|---|
| OD3 (2026-10-03) | **A button, plus a remembered choice.** The button is always available. Using it offers "always do this". Once chosen, broadcast-intent links try the app automatically, with the browser page underneath as the fallback. |

## 3. Alternatives considered and rejected

| Alternative | Why not |
|---|---|
| Detect the app (launch, then watch `blur`/`visibilitychange` with a timeout) | Unreliable across browsers; the "Open …?" prompt changes the timing; and a wrong guess either hides the browser flow or launches twice |
| A loopback HTTP probe of the app | Blocked by Private Network Access; a local attack surface (docs/68 §3) |
| Offering the app on viewer pages | There is no desktop viewer (docs/68 D3) |
| `navigator.registerProtocolHandler` | Lets a *web* page handle `web+…` schemes; the opposite direction, and `gawk` isn't on the safelist |
| Making the shared link `gawk://` | A raw `gawk://` link does nothing for anyone without the app. Shared links stay `https://`; the SPA is the switch. |

## 4. Decisions

### D1 — Where the offer appears

| Surface | What changes |
|---|---|
| `BroadcasterScreen` pre-start card ("Start a stream") | A secondary action under the primary **Start a stream**: "Open in the desktop app". Start stays the primary button (G7). |
| `RoomScreen` / `RoomPanel` "Start streaming here" | A secondary "…or in the desktop app" beside it |
| The "Sharing tips" `NATIVE_TIP` | Copy updated to "Windows, macOS or Linux" (macOS shipped in R52); its download link is unchanged |

Nothing changes on the landing page, viewer screens or `#/join` (G3).
docs/46's rule that viewer and broadcaster screens carry no download links
is kept: the only download link added is the one inside the fallback note
(D4), shown after the user has asked for the app.

### D2 — Which devices see it

Only when `detectClientIdentity(navigator.userAgent).os` is `windows`,
`macos` or `linux` (`transport/client-identity.ts`, today used only for
metrics). This is presentation, not capability: `lib/browserSupport.ts`'s
"no user-agent sniffing" rule is about feature detection and stays as is.
A wrong guess costs a button that does nothing, and D4's fallback covers
that.

The offer is also hidden when `config.desktopHandoff` is `false` (D7) and
while a broadcast is live: the card is gone by then anyway.

### D3 — Building the link

`lib/desktopLink.ts`: `buildDesktopBroadcastLink({ room, nick, relay })`
returns R66's canonical `gawk://broadcast?…`.

- **`room`**: the pending room (`pendingRoom.code` from the
  `gawk:room-return` stash, or the room screen's code).
- **`nick`**: the nickname the user would attach with. That is the
  `?nick=` prefill if present, otherwise the stored `gawk:nickname`,
  sanitized with `sanitizeNickname`.
- **`relay`**: only when the resolved server isn't the default, the same
  rule as `relayQuerySuffix`. It is the normalized origin of the session
  override or the selected saved server. The desktop matches it against
  its own saved servers or offers to add it (docs/68 D5).
- **Never** a grant (`rt`), an attach secret or a publish secret (docs/68
  D1). A static room that needs an attach secret prompts for it in the
  desktop app, which may already hold it in its recent rooms. This is the
  one way the handoff is less smooth than staying in the browser; §8
  covers it.
- Parameter order and encoding follow `to_gawk` exactly, so G5 can assert
  byte equality.

### D4 — Launching, and what the page shows afterwards

- **The action is a real `<a href="gawk://broadcast?…">`**, so the launch
  carries user activation and right-click → copy works. The mechanism for
  the automatic mode (D5) is decided by V-1. The candidates are
  `location.href`, a synthetic anchor click, and a hidden iframe. Whichever
  is chosen must leave the SPA page and its state intact in every browser
  V-1 covers. A browser where none does gets no automatic mode.
- After a click, the card shows a note in place. It does not navigate, and
  it does not hide the Start button.
  - "Opening the desktop app… Didn't open? [Get the app] · [Continue in the
    browser]". [Get the app] is `SITE_DOWNLOAD_URL`. [Continue in the
    browser] closes the note.
  - Below it: "☐ Always open broadcast links in the desktop app" (D5).
- The page never tries to find out whether the launch worked. The note
  stays until it's dismissed or the user starts in the browser.

### D5 — The remembered choice (OD3)

- **Storage**: `gawk.desktopHandoff` = `"auto"` in localStorage, through
  `lib/storage.ts`'s guarded `readStored`/`writeStored`. If storage is
  unavailable, there is no automatic mode and the button still works.
  Absent means offer only. There is no stored "never": dismissing the note
  is enough.
- **When `"auto"` is set**:
  - Landing on `#/broadcast` **with `room=`** launches the app once, as the
    page loads, through V-1's mechanism. That is the link-from-outside
    case, the Mumble bot's button. A plain `#/broadcast` visit doesn't:
    the user is in the web app on purpose.
  - Clicking "Start streaming here" in a room goes straight to the app,
    since the click provides activation. The browser path is still on the
    card it would have navigated to. Because "Start streaming here"
    navigates to `#/broadcast` anyway, this is "launch, then navigate as
    before". The broadcaster card shows D4's note.
  - **At most once per page load**, and never again for the same URL in
    this tab. A sessionStorage flag keyed by the hash means back/forward
    and reloads don't relaunch.
  - The note shows "Opening the desktop app automatically · [Stop doing
    this]". The last action clears the key.
- **Never automatic**: when D2 hides the offer, when `relay=` was dropped as
  invalid or not allowed (the user should see that note in the browser),
  or while the terms or secret modal is open.

### D6 — The room stash is not consumed

`takeRoomReturn` empties the `gawk:room-return` stash on mount. Launching
the app does not change that: the room stays the browser card's pending
room, so **Continue in the browser** loses nothing. The two paths are
independent; neither waits for the other.

### D7 — Operator switch

`config.desktopHandoff` (default `true`) in `config.ts`'s runtime config,
templated in `deploy/charts/gawk-app/templates/configmap.yaml` the same way
as `allowCustomRelays` (`hasKey`, default `true`), with a documented
`values.yaml` key and a line in `docs/self-hosting.md`. A self-hosted
deployment whose users shouldn't be pointed at the official desktop
builds can turn it off.

### D8 — Copy

| Where | Text |
|---|---|
| Card secondary action | Open in the desktop app |
| Room, beside "Start streaming here" | …or in the desktop app |
| Note after a click | Opening the desktop app… Didn't open? **Get the app** · **Continue in the browser** |
| Checkbox | Always open broadcast links in the desktop app |
| Automatic note | Opened the desktop app automatically · **Stop doing this** |

## 5. Non-goals

- Handing viewers to any app (no desktop viewer; R65's iOS viewer is
  reached by its own `gawk://` links).
- Offering the iOS app from Safari on iOS. iOS Safari can't broadcast, so
  that is a natural follow-up once R65 ships, on the same grammar.
- Detecting whether the app is installed (§3).
- R26 quick-start parameters in the handoff (R26 isn't built; docs/68 §5).
- Carrying grants or secrets across (D3).

## 6. What deliberately does not exist

No install detection; no launch without either a click or a stored
"always"; no relaunch on back/forward/reload; no grant, attach secret or
publish secret in a `gawk://` link; no download link on a broadcaster
screen before the user asks for the app.

## 7. UX flows

**First time**: the Mumble bot's "Start streaming" → `#/broadcast?room=X`
→ the card shows Start (primary) and "Open in the desktop app" → click →
the browser's "Open gawk broadcast?" prompt → the app opens, prefilled →
the note in the tab offers "Always open…" → tick it.

**After "always"**: the next bot link → the tab opens, the app launches by
itself (the browser may still prompt the first time, unless "always
allow" was ticked there) → the tab shows "Opened the desktop app
automatically · Stop doing this", with Start still on the card.

**No app**: click → nothing visible happens (or the browser says it has no
handler) → the note's "Didn't open? Get the app · Continue in the browser"
→ Start works as before.

## 8. Risks

| Risk | Mitigation |
|---|---|
| A browser navigates away to an error page for an unhandled scheme | V-1 is checked before HO3 picks the auto mechanism; the button is a plain anchor in every browser, which V-1 also covers |
| Automatic launch is blocked without user activation | Then the automatic mode degrades to the note with the button; V-1 records each browser |
| Static rooms behind an attach secret: the desktop asks for it again | Its recent rooms may hold it (docs/68 D4); the browser path is one click away; carrying secrets is out (D3) |
| Users on an old desktop build without R66 | The "Didn't open?" note; release order (HO3 after R66 ships) |
| The offer reads as nagging | One secondary line, no modal, no download link until asked (D1, D4) |

## 9. Chunks and acceptance criteria

| Chunk | Scope | Accepted when |
|---|---|---|
| **HO1** | D3's builder, the restated vectors (after R66 LH1), D7's config and chart | Every R66 vector's canonical link is reproduced byte for byte (G5); no grant or secret can be produced, tested with a grant in the stash; the chart renders `desktopHandoff` with and without the value set (G6) |
| **HO2** | D1, D2, D4, D6, D8: the button, the room's secondary action, the note, the `NATIVE_TIP` copy | Component tests: the offer shows on the three desktop OS identities and on none of the others, nor on viewer and landing pages (G3); the anchor's `href` carries room, nick and a non-default relay (G1); after a click, Start is still enabled and the pending room is intact (G7, D6). An e2e step in `e2e/run.mjs` clicks the button in headless Chrome with no handler and asserts the page and room chip remain. |
| **HO3** | D5: the remembered choice and automatic launch, on V-1's mechanism | Unit tests: automatic only on `#/broadcast?room=` with `"auto"` set; once per page load; not on reload or back/forward; never on non-desktop OS, with a dropped relay, or with storage unavailable; "Stop doing this" clears it. Merged only after a desktop release containing R66 LH2–LH5. |
| **HO4** | The owner's pass: Chrome, Firefox and Edge on Windows; Chrome, Firefox and Safari on macOS; Chrome and Firefox on Linux; with and without the app installed; V-1 | G1, G2 and G4 recorded per browser in §12 |

## 10. V-items (recorded in §12)

| # | Question | Decides |
|---|---|---|
| V-1 | Per browser and OS, with no handler registered: does an anchor click, `location.href` or a hidden iframe to `gawk://…` leave the page in place? And with a handler, does a launch **without** user activation work, prompt or get blocked? | D4's and D5's mechanism, HO3's per-browser behaviour |
| V-2 | Does the browser's "always allow" for the scheme persist per origin, so the automatic mode becomes silent after the first time? | §7's expectations, D8's copy |

## 11. Open questions

None.

## 12. Deviations and field findings

None yet.
