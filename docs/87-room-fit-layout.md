# R83 — Fit: a room layout that follows the streams' shapes

**Status**: designed 2026-11-10; FL1–FL3 implemented 2026-11-10 (§13). Open: FL4, the owner pass.

**Scope**: the web app's room view only (`gawk-app/src/features/room`).
No relay, wire, telemetry, native-broadcaster or iOS change.

The room grid ([docs/44](44-rooms.md) D11) lays out every POV in
`ceil(√n)` columns of equal cells, and each video letterboxes inside its
cell. It never looks at the streams' shapes or the viewer's screen, so
two broadcasters sharing 32:9 ultrawide screens into a room land side by
side on a 16:9 viewer's screen, and 75 % of that screen is black. Stacked
one above the other, the same two streams fill all of it.

This milestone adds **Fit**: a fourth layout mode, next to Grid, that
places the tiles from each stream's real aspect ratio and the stage's
size so as much of the screen as possible is video. Grid stays exactly
as it is and stays the default (OD1).

## 1. Why, and what "done" means

The owner's report: two ultrawide broadcasters in one room make the
experience worse for viewers on ordinary 16:9 screens, because the
streams go side by side instead of on top of each other. The same
waste shows up in other mixes: a phone-portrait stream next to a 16:9
one, five mixed shapes with the people panel open, or three 16:9
streams on an ultrawide viewer. §12 has the measured figures.

### Milestone acceptance criteria

| Goal | Verified by |
|---|---|
| The room's layout segment offers **Grid · Fit · Focus · Hide videos**; Fit is persisted like the other modes, Grid stays the default, and Grid's layout is unchanged | FL3 component tests; `e2e/run.mjs --rooms` still passes its Grid steps unchanged |
| In Fit, each tile's box has its stream's aspect ratio (within 2 %), so no tile letterboxes; tiles keep roster order (reading order) | FL1 unit tests; FL3 component test; `e2e/run.mjs --rooms` Fit step with two shapes (one pubsim on the default 4:3 fixture, one on `-fixture large`, 16:9): each tile's box aspect against its `data-frame-w/h` |
| Two 32:9 streams on a 1920×1080 stage stack one above the other and fill **≥ 95 %** of it (Grid: 25 %) | FL1 unit test |
| Fit's score (D4) is **never below Grid's** for the same tiles and stage, by construction (D3's grid family, D5's floor) | FL1 property test over random shapes, counts and stages, with and without a previous arrangement (D5) |
| Over §12's 40 cells (ten mixes × four stages) Fit's coverage is **above Grid's in 32 and equal in the other 8** | FL1 table test reproducing §12 |
| A plain room (every stream 16:9) on a 16:9 stage gives the same coverage as Grid for 2–4 streams | FL1 unit test |
| A stream's shape comes from its **decoded frames**, not metadata; before the first frame a tile counts as 16:9; a change under 2 % does not move the layout | FL2 tests |
| Resizing the window or opening the people/voice panel re-lays out, and switching Grid ↔ Fit ↔ Focus moves tiles, without remounting any (no media session re-dials) | FL3 component test with a stubbed `ResizeObserver` (same tile elements before and after); `e2e/run.mjs --rooms` (for each active broadcast, the set of subscriber `key`s on `/statusz` of the tiles' primary sessions — entries without `stripeLeg` or `internal`, since stripe legs come and go on their own — random per session and always set, non-empty and unchanged across Grid → Fit → Focus) |
| The layout doesn't flap: when only the stage size changes, the current arrangement is kept unless the new best beats it by more than 3 %, or it falls below Grid (D5) | FL1 unit test |
| The worst cases lay out fast: 10 tiles (the exhaustive search, ~2 000 candidates) and a 40-stream wall each in **< 5 ms** | FL1 timing test (generous bound, CI-safe) |
| Under 720 px Fit degrades to Focus, as Grid does | FL3 component test |

## 2. Owner decisions (2026-11-10)

| # | Question | Decision |
|---|---|---|
| OD1 | Replace the grid, or offer the new layout as a choice? | **A choice.** Fit is an alternative layout the viewer picks; Grid stays the default and unchanged. |

The questions the 2026-11-10 discussion left open take the defaults the
prototype recommended. They are written down as decisions (§3) so the
owner pass (FL4) can revise any of them in place:

- Fairness against coverage: D4's score.
- Phones: Fit degrades to Focus below 720 px, like Grid (D8).
- iOS: not in this milestone (§6).
- The focus-mode strip: unchanged, still 16:9 (§6).

## 3. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **Fit is a fourth `RoomMode`, `'fit'`**, placed second in the segment: Grid · Fit · Focus · Hide videos. It is stored in `gawk:room-mode` like the others; an unknown stored value still loads as Grid. Its icon is three tiles of different widths in two rows. | OD1. A mode is one click to try and one to undo, and needs no new menu. Next to Grid because it is the same idea, all POVs at once. |
| D2 | **The `0` key returns to the last all-POV mode** the viewer chose, Grid or Fit, not always Grid. It is its own preference, `gawk:room-grid` (`grid` \| `fit`, default `grid`), written whenever Grid or Fit is chosen, so it survives a reload made in Focus (keys 1–4 store `focus` in `gawk:room-mode`). The hide-videos card's Grid button follows it too, and is labelled after it. | `0` means "back to everyone"; someone who chose Fit shouldn't be thrown into Grid by it. |
| D3 | **Candidates**, two families. (a) For both orientations (rows of tiles, and columns of tiles for portrait streams) and every order-preserving split of the tiles into consecutive groups, two sizings: **justified**, where each row's tiles share a height and fill the stage's width, then the whole block scales down to fit the height; and **balanced**, where each row's height is capped at an equal share of the stage height. Up to 10 tiles every split is tried (2^(n−1) splits × 2 orientations × 2 sizings: 2 048 at 10); above 10, only the even splits into k groups, for each k (~160 at 40). (b) **Grid's own geometry** for every column count: equal cells in reading order, each tile the video's rectangle centred in its cell. | Justified rows give each tile its own shape with no letterbox and handle mixed shapes; balanced rows keep the plain grid where it is better (3× 16:9 on a 16:9 stage, §12). Order-preserving keeps the number keys reading in order. Family (b) contains today's Grid, so Fit's score is never below Grid's; it changed no cell of §12. An exhaustive search at a 40-stream wall would be 2^39. |
| D4 | **The score is Σ ln(tile area)**, the highest wins (ties go to the first candidate found). | It maximises coverage but a layout that shrinks one tile to a sliver loses to one a little emptier and fairer: the log punishes small tiles. Plain total area picked layouts with a 4 % tile beside a large one. |
| D5 | **Stability.** When only the stage size changed since the last layout (same tiles in the same order, same shapes after D7's deadband), the arrangement in use (family, orientation, split or k, sizing or column count) is re-sized for the new stage and kept while its score is within **ln(1.03) × n** of the best, i.e. the new best's geometric-mean tile area is less than 3 % better, **and not below today's Grid** (`ceil(√n)` columns) on the same stage; an arrangement dropped either way is replaced by the fresh best. A tile joining, leaving or changing shape starts fresh. | A window dragged across a boundary, or a panel opening, otherwise flips the arrangement back and forth. A split means nothing once the tiles change. The Grid floor keeps §1's "never below Grid" true with a previous arrangement too. |
| D6 | **Placement.** The block is centred on the stage both ways, and each row (column) is centred within it. Gap 2 px, as in Grid. | The leftover space is split once around the layout instead of being letterbox inside every cell. |
| D7 | **The stream's shape.** A remote tile reports `displayWidth / displayHeight` of its decoded frames (the viewer's `frameWidth`/`frameHeight` stats). The broadcaster's own preview tile reports its `<video>`'s `videoWidth / videoHeight` on `loadedmetadata` and `resize`. Before either, a tile counts as **16:9**. A new value is taken only when it differs from the one in use by **more than 2 %**. Shapes are clamped to **1:4 … 4:1**. An away or offline tile keeps its last shape: shapes live in `RoomScreen`, keyed by broadcast ID, so a tile's retry (which remounts its session) or a mode switch never forgets one. Each tile also exposes its decoded size as `data-frame-w` / `data-frame-h` for the e2e. | The project's rule: trust the frames, not `getSettings()` or other metadata (docs/01). The 2 % deadband keeps a reconnect or a quality step at the same shape from moving the room. The clamp keeps a broken stream from producing a degenerate layout. |
| D8 | **Narrow screens**: under 720 px Fit shows as Focus, exactly as Grid does, and its segment button carries the same title. | Out of this milestone's scope (§6); the phone room is the focus room by design (docs/44). |
| D9 | **Rendering.** Fit tiles are absolutely positioned (`left/top/width/height` in px, merged into `TileFrame`'s style beside the strip's `--i`) inside the stage. The stage's size comes from a small `useElementSize` hook: a `ResizeObserver` where there is one, otherwise (jsdom, which has neither one nor layout) the window's inner size, re-read on `resize`; tests that need a given size stub it. The tile list stays one flat list keyed by broadcast ID, so switching Grid ↔ Fit ↔ Focus moves tiles and never remounts one. Within Fit, tiles animate to new boxes over the existing `--dur` (the global reduced-motion rule already collapses it); Grid ↔ Fit snaps, since a CSS grid cell and an absolute box don't transition. Fit tiles use the Grid tile variant (volume pill, all POVs mixed), and every `.stage[data-mode='grid']` rule that isn't about the CSS grid itself applies to Fit too. | docs/44's rule that a mode switch must not re-dial a media session. |
| D10 | **Edge chrome.** A Fit tile whose top lies inside the header band (`--chrome-top`, 4.5rem) takes `data-edge-top` and `--edge-top: band − tileTop` (never less than the label's usual 0.6rem), so its label sits just below the header; a tile whose bottom lies inside the footer band (`--chrome-bottom`, 4rem) takes `data-edge-bottom` and `--edge-bottom` the same way. The bands are converted with the root font size, not hard-coded px. This is Grid's rule (docs/44, revision 2026-09-05) measured from where the tile actually is. | Fit's tiles don't always start at the stage edge (the block is centred), so "first row" is not the test, and Grid's fixed `top: var(--chrome-top)` would leave a label well below the header on a tile that starts 40 px down. |
| D11 | **The layout is a pure function**, `fitLayout(aspects, width, height, opts) → { rects, arrangement }` in `gawk-app/src/features/room/fitLayout.ts`, with no DOM, so it is unit-tested directly. | Most of this milestone's risk is in the arithmetic, and pure arithmetic is cheap to test. |

## 4. What changes where

| File | Change |
|---|---|
| `features/room/fitLayout.ts` (new) | D3–D6, D11. |
| `features/room/roomPrefs.ts` | `RoomMode` gains `'fit'`; `loadRoomMode` accepts it; `gawk:room-grid` (D2). |
| `lib/useElementSize.ts` (new) | D9's size hook. |
| `features/room/RoomTile.tsx` | Tiles report their stream's shape (D7) through an `onAspect` callback and expose `data-frame-w/h`; tiles take an optional `box` (px) and edge offsets from the caller. |
| `features/room/RoomScreen.tsx` | The Fit segment button, `0`'s last all-POV mode (D2), the stage's `ResizeObserver`, shapes per broadcast ID, `fitLayout` per render, tile boxes and edges (D9, D10). |
| `features/room/room.module.css` | `.stage[data-mode='fit']` positioning and transition, the Grid edge rules extended to Fit with D10's offsets. |
| `ui/Icons.tsx` | `FitIcon`. |
| `e2e/run.mjs` | The `--rooms` pass runs its second pubsim on `-fixture large` and gains a Fit step between Grid and Focus. |
| `gawk-server/internal/pubsim/fixture/README.md`, `gawk-pubsim`'s flag comment | The large fixture's scope note names the rooms pass too (in this design PR, which releases nothing). |
| `docs/44-rooms.md` | D11 gains a dated pointer to this doc. |

## 5. Risks and limits

- **Sizes are no longer equal.** A 32:9 stream next to a 16:9 one is
  twice as wide in the same row. That is the point (the same height, no
  letterbox), but some viewers may read a bigger tile as "featured". D4's
  log score limits how unequal it gets; Grid remains for anyone who
  prefers equal cells.
- **The layout can move when a stream's shape changes**, e.g. a
  broadcaster switching from a 32:9 desktop to a 16:9 game window. D7's
  deadband and D5's stability keep it to real changes, and the move is
  animated.
- **Above 10 tiles the search is coarser** (D3). Walls up to 40 still get
  the best even split, which is what Grid does today, plus the shapes.
- **The rooms e2e carries the large fixture.** It is the only 16:9 fixture
  (`default` and `magenta` are 320×240), so the Fit step's second shape
  costs a 5 Mbps noise stream in a pass whose "tile advanced" polls are
  3–5 s, and striping engages on it (hence the primary-only key check).
  A small 16:9 fixture would keep the pass cheap but is a pubsim change;
  taken only if the pass proves flaky.
- **Tile chrome on small tiles.** A thin 32:9 tile at the top of a busy
  room has little height for the label and the volume pill, and a tile
  shorter than both chrome bands together (~136 px) touches both, so its
  label and controls can meet. They already overlay the video in Grid,
  where a busy wall has the same limit; FL4 judges whether it needs more.

## 6. Out of scope

- **iOS.** The native room grid (`RoomPlayerScreen.tileSize`, docs/70
  D19/D26) assumes 16:9 and has the same problem. Porting `fitLayout` to
  Swift is a follow-up once the web version has been through FL4.
- **Phones on the web** (D8).
- **The focus-mode strip**, still 16:9 thumbnails.
- **Desktop broadcasters' room card** — it shows no video grid.
- **A per-room or relay-side layout hint.** The layout is the viewer's
  own choice from what their own browser decodes.

## 9. Chunks

| Chunk | What | Acceptance |
|---|---|---|
| **FL1** ✅ | `fitLayout.ts`: D3–D6, D11. | Vitest first: two 32:9 on 1920×1080 stack and cover ≥ 95 %; each rect has its tile's aspect (2 %); rects stay inside the stage, never overlap, keep reading order; §12's table reproduced with no previous arrangement (above Grid in 32 cells, equal in 8); the score never below Grid's over random inputs, with and without a previous arrangement; 2–4 × 16:9 on 16:9 equal Grid; D5's stability holds the arrangement on a 1 % resize (inputs away from the Grid floor), gives it up on a large one, and resets when a tile joins; a deterministic case where the kept arrangement is within 3 % of the best but below Grid on the new stage, and is replaced by the fresh best; above 10 tiles the even-split path runs; 10 and 40 tiles each finish in < 5 ms; zero tiles and zero-size stages return empty; clamped shapes. |
| **FL2** ✅ | Stream shapes: D7 in `RoomTile.tsx` and `RoomScreen.tsx`, `data-frame-w/h`. | Tests: a remote tile reports its frames' shape from the stats; no report before the first frame; a < 2 % change is not taken, a larger one is; the shape survives the tile's retry; the own preview reports `videoWidth/videoHeight` on `resize`; the clamp. |
| **FL3** ✅ | The mode: D1, D2, D8–D10, `useElementSize`, the CSS, the icon, the e2e step, the docs/44 pointer. | Component tests: the segment's four radios, Fit persisted and reloaded, `0` back to the last all-POV mode (also after a reload made in Focus), the hide-videos card's button following it, Fit tiles positioned from `fitLayout` with the stage's (stubbed) size, the same tile elements across Grid ↔ Fit ↔ Focus and a resize, edge flags and offsets from the bands, Fit as Focus under 720 px. `e2e/run.mjs --rooms`: a Fit step where each tile's box aspect is within 2 % of its `data-frame-w/h` (4:3 and 16:9), with, for each active broadcast, the non-empty set of primary-session subscriber `key`s on `/statusz` (no `stripeLeg`, no `internal`) unchanged from Grid through Fit to Focus (`sessionId` is telemetry's and absent in this pass); `room-fit.png` written. |
| **FL4** | **Owner pass** on the reference deployment: two ultrawide broadcasters and a 16:9 viewer (the report), a phone broadcaster in the mix, the people and voice panels opening, a window resize, a broadcaster changing source mid-room. | §1's rows confirmed by eye; findings written into §12; any decision revised in place with a dated note. |

## 12. Measurements (prototype, 2026-11-10)

A Node prototype of D3–D6 was run against the room mixes below, at four
stage sizes, before this doc was written. "Coverage" is the share of the
stage that is video; "smallest" is the smallest tile's share. The stage
sizes are a 1080p screen, the same with the 340 px people panel open, a
21:9 ultrawide viewer and a phone in portrait (where today's web app
shows Focus instead; included for the iOS follow-up).

Coverage, Grid → Fit:

| Mix | 1920×1080 | 1580×1080 (panel) | 2560×1080 | 390×844 |
|---|---|---|---|---|
| 2× 32:9 | 25 % → 100 % | 21 % → 82 % | 33 % → 75 % | 6 % → 26 % |
| 2× 21:9 | 37 % → 66 % | 31 % → 81 % | 50 % → 50 % | 10 % → 39 % |
| 2× 16:9 | 50 % → 50 % | 41 % → 61 % | 67 % → 67 % | 13 % → 52 % |
| 32:9 + 16:9 | 37 % → 75 % | 31 % → 81 % | 50 % → 56 % | 10 % → 39 % |
| 3× 16:9 | 75 % → 75 % | 62 % → 71 % | 56 % → 89 % | 19 % → 78 % |
| 2× 32:9 + 16:9 | 50 % → 83 % | 41 % → 67 % | 52 % → 82 % | 13 % → 52 % |
| 16:9 + phone portrait | 51 % → 79 % | 52 % → 65 % | 53 % → 94 % | 31 % → 79 % |
| 4× 16:9 | 100 % → 100 % | 82 % → 82 % | 75 % → 75 % | 26 % → 95 % |
| 5 mixed (32:9, 16:9, 21:9, 16:10, 16:9) | 48 % → 91 % | 40 % → 89 % | 64 % → 85 % | 12 % → 87 % |
| 3× 16:9 + phone portrait | 81 % → 81 % | 69 % → 94 % | 61 % → 64 % | 44 % → 77 % |

Fit is above Grid in 32 of the 40 cells and equal in the other 8 (2× 16:9, 3× 16:9, 4× 16:9 and 3× 16:9 + phone on 1080p; 2× 21:9, 2× 16:9 and 4× 16:9 on the ultrawide; 4× 16:9 with the panel open). After the doc review, Grid's own geometry was added as a second family (D3 b); it changed no cell.

Two findings shaped the decisions:

- **Justified rows alone lose to Grid** when a grid with an empty cell is
  better: 3× 16:9 on 1920×1080 went from 75 % to 67 %, with one tile
  twice the size of the other two. Adding the balanced sizing (D3) fixed
  every such case; the combined search was never below Grid in this
  table.
- **Maximising total area alone** picked, for 2× 32:9 + 16:9 on the
  panel-open stage, 97 % coverage with a 9.7 % tile; the log score (D4)
  picks 67 % with no tile under 13 %. That is the fairness trade D4
  makes on purpose.

FL1's table test reproduces this table from the shipped function; FL4's
findings go below it.

## 13. Implementation notes (FL1–FL3, 2026-11-10)

- **Scoring without rectangles.** The first cut built every candidate's
  rectangles and took 3.4 ms at 10 tiles, too close to §1's bound on a
  busy CI worker and run on renders. A tile's area is A·h² in either
  frame, so the rows family is scored from its row heights alone and only
  the winner is placed: 1.0 ms at 10 tiles and 0.5 ms at 40 (best of
  100 runs in Vitest, Node 24). The timing test takes the best of seven
  runs, so a stall on a shared runner doesn't fail it.
- **D5's Grid floor**, added after the design PR's review, was test-first:
  over 2 000 random inputs the property test with a previous arrangement
  found one kept arrangement scoring 47.08 against Grid's 47.18 before
  the floor went in.
- **D5 keeps its previous result in React state**, recomputed during render
  when its inputs change, not in a ref read during render.
- **The e2e** (`e2e/run.mjs --rooms`, run locally against Playwright's
  Chromium 1254): `fit ok: 320×240 in 639×479, 1280×720 in 639×359` on
  the harness's 1280×720 page. For a 4:3 and a 16:9 stream side by side
  Grid's own geometry (D3 b) narrowly wins on score (two equal columns,
  each tile exactly its shape) over one justified row. The footer has
  faded by the time the Grid step's sampling gap ends, so the step moves
  the pointer before it clicks Fit.
