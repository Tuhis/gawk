# R67 — Open a broadcast in the desktop app from `gawk-app` (docs/69)

**Status**: proposed 2026-10-03. Owner decision OD3 (docs/68 §2) was taken
the same day; the owner's UX review and D9 on 2026-10-06. **HO1–HO3**
(§9) are implemented; HO4, the owner's per-browser pass, is open. **Depends on R66**
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
| G2 | Clicking it with the app installed opens the app prefilled (R66 G1/G2). Without the app, the page stays put with its state intact, and the **Opening** modal offers the download and **Continue in the browser**. | HO4, per browser (V-1) |
| G3 | The offer never appears on Android, iOS or ChromeOS, on viewer pages or on the landing page | unit tests |
| G4 | After the user ticks **Always open broadcast links in the desktop app**, a broadcast-intent link opens the app on its own, at most once per room in a tab (D5). The page shows the **Opened** modal, with the fallback and a one-click **Stop doing this**. | unit tests + HO4 |
| G5 | `gawk-app` builds links that are byte-identical to R66's canonical `to_gawk` for every vector | unit test on the restated vectors |
| G6 | An operator can turn the offer off with `config.desktopHandoff: false`, plumbed through the chart | chart template test + unit test |
| G8 | A `#/broadcast?…&desktop=1` link tries the app once on load and shows the **Opening** modal, on the same devices and under the same conditions as the offer; the parameter leaves the URL before the first render (D9) | unit tests + HO4 |
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
| `RoomScreen` / `RoomPanel` "Start streaming here" | A secondary "…or in the desktop app" beside it. Its click launches the app (the click is the activation), stashes the room like "Start streaming here" does with `handoff: 'opening'`, and goes to `#/broadcast`, which opens the **Opening** modal over the card without launching again. **Continue in the browser** then leaves the user exactly where "Start streaming here" would have. |
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

**iPadOS needs an explicit exclusion.** iPadOS Safari sends a desktop
`Macintosh` user agent by default, and `detectClientIdentity` maps every
`Macintosh` UA to `macos`. The offer is therefore shown on `macos` only when
`navigator.maxTouchPoints <= 1`; an iPad reports more. On an iPad a
`gawk://broadcast` link isn't harmless, because R65's iOS app registers the
scheme. The gate is a small `isDesktopForHandoff(ua, maxTouchPoints)` in
`lib/desktopLink.ts`. It is not a change to `detectClientIdentity`, whose
metrics labels stay as they are.

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
  its own saved servers or offers to add it (docs/68 D5). **Omitting it
  means the default fleet**: in a `gawk://` link, a missing `relay=` is
  the default, not the receiving app's selected server (docs/68 D1). So
  "on the default here" carries over even when the desktop app has
  another server selected.
- **Never** a grant (`rt`), an attach secret or a publish secret (docs/68
  D1). A static room that needs an attach secret prompts for it in the
  desktop app, which may already hold it for that server (docs/68 D5a). This is the
  one way the handoff is less smooth than staying in the browser; §8
  covers it.
- Parameter order and encoding follow `to_gawk` exactly, so G5 can assert
  byte equality.

### D4 — Launching, and what the page shows afterwards

- **The action is a real `<a href="gawk://broadcast?…">`**, so the launch
  carries user activation and right-click → copy works. The click keeps
  the anchor's own navigation, the path every "open in app" link takes.
- **The automatic launches (D5, D9) use a hidden iframe** whose `src` is
  the link, removed a few seconds later. With no click behind it, a
  top-level navigation (`location.href`) to a scheme with no handler can
  replace the page with an error page in some browsers; an iframe's
  failure stays inside the iframe. Chosen 2026-10-06 ahead of V-1 (§12);
  HO4 records whether it holds in every browser it covers.
- After a click, the page opens the **Opening** modal over the card. It
  is styled like the page's other modals (the terms and publish-secret
  prompts: scrim, centred glass panel, title, muted body, a secondary and a
  primary action). The card underneath is untouched: Start stays enabled
  and the pending room stays (G7, D6). Owner decision 2026-10-06, replacing
  the inline note first drafted here (§12). The modal holds, in order:
  - the title "Opening the desktop app…";
  - "Didn't open? Get the app, or continue in the browser.";
  - "☐ Always open broadcast links in the desktop app" (D5);
  - **Get the app** (secondary, `SITE_DOWNLOAD_URL` in a new tab) and
    **Continue in the browser** (primary, closes the modal). The scrim and
    Escape close it too.
- The page never tries to find out whether the launch worked. The modal
  stays until it's dismissed.

### D5 — The remembered choice (OD3)

- **Storage**: `gawk:desktop-handoff` = `"auto"` in localStorage (the
  `gawk:` prefix every other stored preference uses), through
  `lib/storage.ts`'s guarded `readStored`/`writeStored`. If storage is
  unavailable, there is no automatic mode and the button still works.
  Absent means offer only. There is no stored "never": dismissing the
  modal is enough.
- **Where the trigger is detected.** `applyRouteRoom` runs in App.tsx's
  route resolution before `BroadcasterScreen` mounts. It moves `?room=`
  into the `gawk:room-return` stash and strips it from the hash. By the
  time the screen can check its own state (D5's "never" conditions are
  screen state), every visit looks like a plain `#/broadcast`. So the
  stash records where the hop came from: `RoomReturn` gains
  `source: 'link' | 'room'`. `applyRouteRoom` writes `'link'`, and
  `RoomScreen`'s `stashRoomReturn` writes `'room'`. The screen reads it
  through the existing `takeRoomReturn` on mount. A stash without the
  field (an older tab) counts as `'room'`, which never auto-launches.
- **When `"auto"` is set**, there are exactly two triggers:
  - **A link from outside**: the screen mounts with a stash whose
    `source` is `'link'` (the Mumble bot's `#/broadcast?room=`). It
    launches the app once, through V-1's mechanism. A plain `#/broadcast`
    visit has no stash and never launches: the user is in the web app on
    purpose.
  - **"Start streaming here" in a room**: the click handler in
    `RoomScreen` launches the app itself, since the click provides
    activation. It then stashes `source: 'room'` and navigates to
    `#/broadcast` as before. The stash also says the app was already
    launched (`handoff: 'auto'`), so the screen doesn't launch a second
    time; it shows the **Opened** modal.
- **At most once per room in this tab.** A sessionStorage flag
  `gawk:handoff-done:<code lower-cased>` is set when either trigger fires,
  and both check it first. A reload has no stash (`takeRoomReturn` reads
  and clears), so it never relaunches. Following the same room's link
  again in the same tab doesn't relaunch either; the offer and the
  modal's **Open again** still do.
- An automatic launch opens the **Opened** modal (owner decision
  2026-10-06, replacing the inline note): the title "Opened the desktop
  app"; "Didn't open? Get the app, or continue in the browser." with
  **Get the app** as an inline link; between two dividers, the quiet line
  "Broadcast links open in the desktop app automatically." with
  **Stop doing this** under it, which clears the key and closes the modal;
  then **Open again** (secondary, the same `gawk://` anchor as the offer)
  and **Continue in the browser** (primary, closes the modal).
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
| Opening modal: title | Opening the desktop app… |
| Opening modal: body | Didn't open? Get the app, or continue in the browser. |
| Opening modal: checkbox | Always open broadcast links in the desktop app |
| Opening modal: actions | **Get the app** · **Continue in the browser** |
| Opened modal: title | Opened the desktop app |
| Opened modal: body | Didn't open? **Get the app**, or continue in the browser. |
| Opened modal: automatic line | Broadcast links open in the desktop app automatically. **Stop doing this** |
| Opened modal: actions | **Open again** · **Continue in the browser** |

### D9 — `?desktop=1`: a link that asks for the app (owner addition, 2026-10-06)

A page outside the app that already knows its user broadcasts from the
desktop app (a chat bot's card, a pinned link) can say so:
`#/broadcast?room=<code>&desktop=1`.

- **Parsing**: `parseRoute` reads `desktop` on the broadcast route only;
  exactly `1` turns it on and any other value is ignored. Like `?nick=`, it
  rides the route into the screen as a prop (`linkDesktop`) and
  `applyRouteDesktop` strips it from the hash before the first render, so
  a reload or a copied link doesn't launch again.
- **What it does**: on mount, the screen launches the app once through D4's
  mechanism and opens the **Opening** modal, the same screen as a click on
  the offer, including the "always" checkbox. It works without a room
  (a bare `gawk://broadcast`).
- **It is not the remembered choice.** It stores nothing and doesn't use
  the once-per-room flag: it is one explicit request in one link. When
  `"auto"` is also stored, the parameter wins and the Opening modal shows,
  so the checkbox reflects the stored choice.
- **Never**: the same conditions as D5's "never automatic". When D2 hides
  the offer, `config.desktopHandoff` is off, or `relay=` was dropped, the
  parameter is ignored and the page is the ordinary broadcast page.
- **Launch without activation**: like D5's automatic launch, this runs on
  mount with no click behind it. Chromium takes it when a click on the link
  opened the page in a new tab, and refuses it after a same-tab link, a
  typed URL or a reload (§12). A browser that refuses still shows the
  modal; **Continue in the browser** closes it and the offer stays on the
  card.

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
the tab shows the **Opening** modal, which offers "Always open…" → tick
it, then **Continue in the browser** (or just leave the tab).

**After "always"**: the next bot link → the tab opens, the app launches by
itself (the browser may still prompt the first time, unless "always
allow" was ticked there) → the tab shows the **Opened** modal, with
**Stop doing this**, **Open again** and **Continue in the browser**; Start
is still on the card underneath.

**A `desktop=1` link** (D9): the tab opens, the app launches by itself
once, and the tab shows the **Opening** modal, as after a click.

**No app**: click → nothing visible happens (or the browser says it has no
handler) → the modal's "Didn't open? Get the app, or continue in the
browser" → **Continue in the browser** → Start works as before.

## 8. Risks

| Risk | Mitigation |
|---|---|
| A browser navigates away to an error page for an unhandled scheme | V-1 is checked before HO3 picks the auto mechanism; the button is a plain anchor in every browser, which V-1 also covers |
| Automatic launch is blocked without user activation | Then the automatic mode degrades to the modal, whose **Open again** (or the offer) is a click; V-1 records each browser |
| Static rooms behind an attach secret: the desktop asks for it again | Its recent rooms may hold it (docs/68 D4); the browser path is one click away; carrying secrets is out (D3) |
| Users on an old desktop build without R66 | The modal's "Didn't open?"; release order (HO3 after R66 ships) |
| iPadOS Safari reports a `Macintosh` UA, so the offer would show and could open the iOS app | D2's `maxTouchPoints` gate, tested with real UA strings in HO2 |
| The offer reads as nagging | One secondary line, no modal, no download link until asked (D1, D4) |

## 9. Chunks and acceptance criteria

| Chunk | Scope | Accepted when |
|---|---|---|
| **HO1** | D3's builder, the restated vectors (after R66 LH1), D7's config and chart | Every R66 vector's canonical link is reproduced byte for byte (G5); no grant or secret can be produced, tested with a grant in the stash; the chart renders `desktopHandoff` with and without the value set (G6) |
| **HO2** | D1, D2, D4, D6, D8: the offer, the room's secondary action, the Opening modal, the `NATIVE_TIP` copy | Component tests: the offer shows on the three desktop OS identities and on none of the others, nor on viewer and landing pages (G3); `isDesktopForHandoff` is tested with real UA strings, including iPadOS Safari's `Macintosh` UA with `maxTouchPoints` 5 (hidden) and macOS Safari with 0 (shown); the anchor's `href` carries room, nick and a non-default relay (G1); after a click, the Opening modal is open, and once it is closed Start is still enabled and the pending room is intact (G7, D6); the room's secondary action launches, stashes `handoff: 'opening'` and the broadcaster opens the modal without a second launch. An e2e step in `e2e/run.mjs` clicks the button in headless Chrome with no handler and asserts the page and room chip remain. |
| **HO3** | D5 and D9: the remembered choice, automatic launch and `?desktop=1`, on D4's mechanism | Unit tests, against D5's detection point: with `"auto"` set, `applyRouteRoom` stashes `source: 'link'` and the screen launches once on mount; a room's "Start streaming here" click launches from the handler and the screen, seeing `source: 'room'`, doesn't launch again; a plain `#/broadcast`, a stash without `source`, and a second trigger for a room already in `gawk:handoff-done:<code>` never launch; not on reload; never on non-desktop OS, with a dropped relay, or with storage unavailable; "Stop doing this" clears it. D9: `parseRoute` reads `desktop=1` and nothing else; `applyRouteDesktop` strips it; the screen launches once and opens the Opening modal; never on non-desktop OS, with the config off or a dropped relay; nothing is stored. Merged only after a desktop release containing R66 LH2–LH5 (gawk-broadcast-desktop v2.5.0, 2026-10-04). |
| **HO4** | The owner's pass: Chrome, Firefox and Edge on Windows; Chrome, Firefox and Safari on macOS; Chrome and Firefox on Linux; with and without the app installed; V-1 | G1, G2 and G4 recorded per browser in §12 |

## 10. V-items (recorded in §12)

| # | Question | Decides |
|---|---|---|
| V-1 | Per browser and OS, with no handler registered: does an anchor click, `location.href` or a hidden iframe to `gawk://…` leave the page in place? And with a handler, does a launch **without** user activation work, prompt or get blocked? | D4's and D5's mechanism, HO3's per-browser behaviour |
| V-2 | Does the browser's "always allow" for the scheme persist per origin, so the automatic mode becomes silent after the first time? | §7's expectations, D8's copy |

## 11. Open questions

None.

## 12. Deviations and field findings

- **2026-10-06, the owner's UX review** (a design canvas of the four
  screens): the after-click note became the **Opening** modal and the
  automatic note the **Opened** modal, both in the style of the page's
  existing modals (D4, D5). The copy in D8 changed with them: "Opened the
  desktop app" drops "automatically" from the title and moves it to its
  own line beside **Stop doing this**, and **Open again** was added so a
  launch that didn't happen can be retried from the modal.
- **2026-10-06, `?desktop=1`** (D9, G8): an owner addition to the spec.
- **2026-10-06, launch mechanism chosen ahead of V-1**: a hidden iframe
  for the launches with no click behind them, the anchor's own navigation
  for clicks (D4). The room's "…or in the desktop app" also uses the
  iframe from inside its click, because the page moves to `#/broadcast`
  right after. V-1 remains HO4's job; if a browser fails it, that
  browser's launch path changes here.
- **2026-10-06, V-1 partly measured headless, no handler registered**
  (Chromium 151 and Firefox from Playwright, on Linux). Signal: a launch
  Chromium takes puts up its external-protocol prompt, which holds every
  trusted click on the page from then on; a control page without a launch
  keeps taking clicks.
  - **No handler, any mechanism, both engines: the page stays.** Chromium
    keeps it silently; Firefox logs "Prevented navigation to 'gawk://…'
    due to an unknown protocol" for `location.href`, an anchor `click()`
    and an iframe alike.
  - **Chromium, a launch on load with no click of its own**: taken when
    the page was opened **in a new tab by a click on a link** (a chat
    bot's card), through the iframe and `location.href` alike; **not**
    taken after a same-tab link, a typed URL or a reload — the iframe then
    logs "Not allowed to launch 'gawk://…' because a user gesture is
    required", `location.href` does nothing. So D5's automatic mode and
    D9's `?desktop=1` work for the case they exist for, a link that opens
    a new tab; otherwise the modal shows and nothing opens, and its
    fallback covers that.
  - **Chromium, inside a click**: an anchor's navigation, `location.href`
    and `location.href` followed in the same task by the hash change to
    `#/broadcast` are all taken — the hop doesn't cancel the launch.
  - Still open for HO4: real handlers, Safari, Edge and Windows/macOS.
