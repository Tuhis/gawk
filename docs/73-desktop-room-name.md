# R71 — Your name in a room, up front (desktop broadcaster)

**Status**: proposed 2026-10-19; owner decisions OD1–OD3 (§3) open. Chunks
**NM1–NM5** (§6). Status lives in [`ROADMAP.md`](../ROADMAP.md).
Drawn in Claude Design the same day: the canvas is
<https://claude.ai/artifact/6WBE1QR8isbLitnFzGiRSd> (private: ask the
maintainer); this doc wins where they differ.

**Relationship to earlier work**: revises the name row of
[docs/64](64-desktop-ux-pass-2.md) D16 (the room sheet) and the name strip
at the foot of [docs/60](60-desktop-redesign.md) D8's **Add to a room**
sheet. The name itself is unchanged: one nickname per app
(`engine::config::Config::nickname`), sent in `RoomHello` and by
`SetNickname`, and the stream's label is always that nickname
([docs/44](44-rooms.md) §4.8 revision 2026-10-03 (b), §11.1 revision
2026-09-14). **No wire, relay or engine change.** All of it is the shared
`crates/ui/main.slint` and `crates/ui/src/shell.rs`, so every platform gets
it at once ([docs/54](54-macos-native-broadcaster.md) D11).

---

## 1. Why

The owner: changing your name in the desktop app is too hidden, especially
for a first-time user. Today the name appears in exactly two places:

1. **The foot of the Add to a room sheet**, under the code field, Create,
   and the recents list: one muted line, "No name yet: the room picks one
   for you." with a **Change name** link. A first-time user's eye is on the
   code field and **Join**; they join without reading it.
2. **The active room sheet**, behind **Manage ›** on the room card
   (docs/64 D16), first row of a list.

Neither Ready nor Live shows the name once a room is chosen. The result: a
first-time broadcaster joins as the relay's `guest-<id>`
(`roomsrv.uniqueNickLocked`), their friends see a tile labelled `guest-4`,
and the only way to fix it is a sheet they have no reason to open. The web
app asks for a nickname on first join (docs/44 RM4, "Nickname prompt on
first join, remembered") and iOS keeps it on the People sheet and in
Settings (docs/70 D20, D22); the desktop app is the one client that never
asks.

Two smaller problems found while looking:

- **The app shows your name, not the room's.** Both rows print
  `cfg.nickname`. The relay may answer with a different form — `guest-4`
  for a blank name, `Juho (2)` on a collision — which appears only in the
  roster ("Juho (2) (you)"), with no explanation.
- **Every keystroke renames you.** `room-nickname-edited` flushes after a
  debounce (`shell.rs` `flush_nickname`), so typing "Juho" while live sends
  `J`, `Ju`, `Juh`… to everyone in the room whenever the user pauses. That
  was harmless in a sheet nobody opened; it isn't on a card (D3).

## 2. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **Ask for a name on the way into a room when there is none.** In the **Add to a room** sheet, **Join**, **Join** on a recent room and **Create a new room** with a blank nickname don't act at once: the sheet turns to a name step (§4.1). Title "What should the room call you?", one field, autofocused, the primary **Join ROOM** (or **Create room**), and a quiet **Join as a guest** under it. The typed name is saved (as any edit is) and the original action proceeds. A name already set skips the step entirely. Asked once: a guest who skipped is not asked again on the next join (`cfg.nickname_skipped`, D9). | The web's first-join prompt, in the one moment the user is deciding to be seen by others. It costs one screen only for users without a name, and never again. |
| D2 | **The name is a top-level row whenever a room is chosen.** On Ready and Paused, the Room list (`root.room-pending != ""`, and `LiveSide`'s `!room-active` list) grows a second row under the room: **Your name**. Set: "Juho · Everyone in the room sees this" with **Change**. Blank: the row is the open field itself, tinted, placeholder "Your name in the room", sub "Blank joins as a guest". No room chosen: no row (a solo broadcast has no use for a name). | "Make the name a top-level field when a room is selected" (the owner). It sits next to the room it applies to, above **Go live in ROOM**. |
| D3 | **On Live, your own roster row is the name row.** The room card's "Juho (you)" row gets **Rename**, which turns the row into the field in place (§4.3). The relay's form is what the row shows; when it differs from what you typed, the sub says why: "Someone here is already Juho" for a suffix, "You're a guest. Set a name" for `guest-<id>`. | Where you see yourself is where you change yourself — iOS's People sheet has the same shape (docs/70 D20). It also makes D2's row unnecessary on Live: the card already lists you. |
| D4 | **A guest on Live gets a nudge.** When the relay calls you `guest-<id>`, your roster row is tinted and the field is open under it, as D2's blank row is. Nothing is modal and nothing blocks streaming. | A user who skipped D1 (or came in through a link with a room and no `nick=`) still sees, every time they glance at the window, that their friends see `guest-4`. |
| D5 | **A rename is sent on Enter, Save or focus loss — never while typing.** `room-nickname-edited` no longer flushes; `room-nickname-accepted` and a new `room-nickname-blurred` do. **Esc** restores the saved name. The persisted value follows the same rule. | §1: a field on the card would otherwise rename you once per pause in typing, to everyone. |
| D6 | **One name component, three places.** A `NameRow` component (text + Change ↔ field + Save) replaces the three copies in `main.slint` today (the sheet strip, the room sheet's row, now the card row). It is used by D2 and D3; D1's step is its own layout. | The three copies already differ in wording ("Joining as", "In the room as", "No name yet"). |
| D7 | **The room sheet loses its name row** (*revises docs/64 D16*). With D3 on the card, it would be the same setting in two places, which docs/64 D15 rules out. The sheet keeps the links, Streams, Remove and End room. The Add to a room sheet's foot keeps a one-line "Joining as Juho · Change" only when a name is set; D1 covers the blank case. | docs/64 D15: no setting in two places. The Add sheet has no room yet, so D2/D3 aren't on screen there. |
| D8 | **Links are answers.** A `gawk://` link with `nick=` (docs/68 D4) fills the name and D1 never asks. A link with a room and no `nick=` and a blank name is D2's open row on Ready: the link never starts anything (docs/68 G1), so nothing needs to ask modally. | docs/68 §8: a link prefills, never acts. |
| D9 | **Rules on the field.** `engine::link::sanitize_nickname` on accept (the SPA's `sanitizeNickname`: whitespace collapsed, trimmed); input capped at `MAX_ROOM_NICKNAME_LEN` (32) bytes, the field refusing further input, not truncating on save. **Save** is disabled while the sanitized text is blank; becoming a guest again is clearing the field and pressing Enter, which asks nothing. `nickname_skipped` is a new `Config` bool, default false, set by **Join as a guest**, cleared by any non-blank name. | One rule set shared with links and the web, and a skip that stays skipped. |

## 3. Owner decisions (open)

| # | Question | Options | Recommendation |
|---|---|---|---|
| OD1 | Prefill D1's field with a suggestion? | (a) empty; (b) the OS account's display name (Windows `GetUserNameExW(NameDisplay)`, macOS `NSFullUserName`, Linux the GECOS field), first word only | **(a)**. (b) puts the person's real name in front of strangers one Enter away, and the account name on a gaming PC is often a username anyway. |
| OD2 | Also a **Name in rooms** row in Settings › App? | (a) no — the room surfaces are the only home; (b) yes, as iOS has (docs/70 D22) | **(b)**, as the *only* settings home, with D2/D3 counting as the room's view of it. A user hunting for "where do I change my name" opens Settings first. This bends docs/64 D15; say so in a dated note there if chosen. |
| OD3 | Ask in D1 again after a skip? | (a) never (D9's flag); (b) once per new room code | **(a)**. D2 and D4 keep it visible; asking twice is nagging. |

## 4. Layouts

Widths follow the window's narrow layout; the wide layout's right column is
the same component (docs/60 D7).

### 4.1 Add to a room → name step (D1)

```
┌ Add to a room ─────────────────────────────── ✕ ┐
│  ‹ Back                                         │
│                                                 │
│  What should the room call you?                 │
│  Everyone in LAN-PARTY sees this, and it labels │
│  your stream.                                   │
│                                                 │
│  ┌ Your name ─────────────────────────────────┐ │
│  │ Juho▌                                      │ │
│  └────────────────────────────────────────────┘ │
│                                                 │
│  [        Join LAN-PARTY        ]   (accent)    │
│            Join as a guest                      │
│                                                 │
│  You can change it any time from the room card. │
└─────────────────────────────────────────────────┘
```

Idle, **Join LAN-PARTY** makes it the pending room (docs/60 D8, unchanged);
live, it joins now. For Create: "Your new room's people see this…" and
**Create room**.

### 4.2 Ready with a pending room (D2)

Name set:

```
┌──────────────────────────────────────────────────┐
│ 👥  Room LAN-PARTY                            ✕  │
│     Joins when you go live                       │
├──────────────────────────────────────────────────┤
│ 🙂  Juho                                Change › │
│     Everyone in the room sees this               │
└──────────────────────────────────────────────────┘
        [        Go live in LAN-PARTY        ]
```

Name blank (tinted row, field open):

```
┌──────────────────────────────────────────────────┐
│ 👥  Room LAN-PARTY                            ✕  │
│     Joins when you go live                       │
├──────────────────────────────────────────────────┤
│ 🙂  ┌ Your name in the room ───────────┐ [Save]  │
│     │                                  │         │
│     └──────────────────────────────────┘         │
│     Blank joins as a guest                       │
└──────────────────────────────────────────────────┘
```

### 4.3 Live: the card's own row (D3, D4)

```
┌ ROOM LAN-PARTY ────── 3 streaming · 5 watching  Manage ┐
│ (J)  Juho (you)                               Rename   │
│      Live · 4 watching                                 │
│ (M)  Mika                                     Watch ↗  │
│ (S)  Sanna                                    Watch ↗  │
│ …                                                       │
```

Rename, in place:

```
│ (J)  ┌ Your name in the room ───────────┐  [Save]      │
│      │ Juho▌                            │              │
│      └──────────────────────────────────┘              │
```

As a guest (D4), tinted:

```
│ (g)  guest-4 (you)                                     │
│      You're a guest. Set a name:                       │
│      ┌ Your name in the room ───────────┐  [Save]      │
│      └──────────────────────────────────┘              │
```

A collision: "Juho (2) (you)" with sub "Someone here is already Juho".

## 5. Acceptance criteria

| Goal | Verified by |
|---|---|
| G1 A blank-name Join, Join-recent or Create shows the name step; a name already set goes straight through | shell tests on the fake platform, one per entry, asserting the sheet state and that `choose_room` / `room_join` ran only after the step |
| G2 The step's name is saved and the join proceeds; **Join as a guest** joins with a blank name, sets `nickname_skipped`, and the next blank join doesn't ask | shell tests; `config.rs` round-trip of `nicknameSkipped`, and an old file without it loads as false |
| G3 A `gawk://` link with `nick=` never shows the step | shell test: link prefill, then Join |
| G4 Ready and Paused show the name row exactly when a room is pending, open when blank | `layout_tests.rs` (`i_slint_backend_testing`) (row present/absent, field visible) |
| G5 Live's own row shows the relay's form, explains a suffix and a guest name, and renames in place | shell test feeding a `RoomSummary` whose own participant is `Juho (2)` / `guest-4`; `layout_tests.rs` for the sub text |
| G6 No rename while typing: five `edited` callbacks send zero `SetNickname`; Enter or focus loss sends one; Esc sends none and restores | engine fake session counting `room_set_nickname` calls; this test is written first and fails on today's debounce (CODE-REVIEW.md test-first) |
| G7 Sanitize and cap: `"  Ju   ho "` saves as `Ju ho`; input stops at 32 bytes, multi-byte included; Save disabled on whitespace only | shell tests + `layout_tests.rs` |
| G8 The room sheet has no name row; the Add sheet's foot shows it only when set | `layout_tests.rs` |
| G9 Each OS: a fresh profile, join a friend's room, see the step, name shows on the friend's tile; rename from the card while live | the owner's pass (NM5) |

## 6. Chunks

| Chunk | Scope | Done when |
|---|---|---|
| **NM1** | D5 + D9: rename on accept/blur only, Esc restore, sanitize + cap, `nickname_skipped` in `Config` | G6, G7, G2's config half |
| **NM2** | D6 `NameRow`; D1 name step in the Add sheet; D7 (sheet row removed, foot line conditional); D8 | G1, G2, G3, G8 |
| **NM3** | D2: the name row on Ready and Paused | G4 |
| **NM4** | D3 + D4: the card's own row, relay form and subs, guest nudge | G5 |
| **NM5** | OD2 if (b): Settings › App row; the owner's pass on Windows, macOS, Linux | G9 |

NM1 lands first: NM3 and NM4 put a field where a mid-typing rename would
be seen.

## 7. Risks

| Risk | Mitigation |
|---|---|
| D5 drops the "renames as you type" behaviour a user might rely on | Focus loss also commits, so clicking away saves; nothing is lost silently. |
| The Live card grows a row in the tight layout (docs/64 OD6) | The field replaces the row's own text rather than adding one, except D4's guest nudge, which goes away once named. |
| `nickname_skipped` hides D1 from someone who skipped by accident | D2's open row on Ready and D4's on Live stay. |
