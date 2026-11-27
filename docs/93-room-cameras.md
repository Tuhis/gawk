# R89 — Cameras in a room: a call with faces

**Status**: designed 2026-11-27 with the owner on a design canvas, and
the open decisions answered by the owner the same day (§2). Not started.
**CM0, the measurement spike, gates everything else**: the numbers in
D5, D8 and D10 are proposals until it has run (§9.1).

**Design canvas**: <https://claude.ai/artifact/Uo9sCZYWinfVEgQWfxQKRY>.
The canvas is private to the owner until shared from its Share menu. The
boards this document builds on are C (Focus), C2 (Focus with four
streams), C3 (nobody streaming), C4 (Grid with four streams) and the
shared join card. Boards P1, P2 and P3 are the three phone layouts the
owner still has to choose between (§10). Boards A, B and D are the
directions that were not chosen (§8). The boards were not rendered or checked while they were
drawn, so their spacing is a sketch, and the stream and camera content
in them are placeholders.

**Scope**: the web app's room view (`gawk-app/src/features/room`), a
camera publish path in `gawk-app`, the room control protocol
(`gawk-server/wire/room.go` and its two mirrors), the relay's room
registry (`gawk-server/internal/roomsrv`) and its options, a ticket
check on `/publish` and `/subscribe` for camera broadcasts, and the
screening path (`gawk-server/screening`, `gawk-sampler`, `gawk-admin`).
No change to the media wire, the hub, the Mumble bridge, the desktop
broadcasters or the iOS app (§7).

---

## 1. Why, and what "done" means

A room with voice (R42, R79) is already most of a call: people join by
code, talk through the room's Mumble channel, and watch each other's
streams. Two things are missing. Nobody can see anybody, and a room in
which nobody streams is an empty stage with a voice dock.

The owner expects both kinds of call: friends talking with cameras on
and no stream at all, and friends watching one or several game streams
together. This milestone adds a camera to a participant and one layout
model that covers both, without a second "meeting mode".

A camera is a small broadcast. The relay path that carries a game stream
(publish, keyframe cache, fan-out, cascade) carries a camera unchanged.
What is new is the control-plane fact "this broadcast is Alice's
camera", the capture and encode profile, and the room view.

### Milestone acceptance criteria

| Goal | Verified by |
|---|---|
| A participant turns their camera on and off from the room; others in the room see it within the time CM0 measures as achievable (target: under 1 s from the click) | CM3/CM4 component tests; CM8 e2e with Chrome's fake camera device; CM9 owner pass |
| With no stream in the room, people fill the stage as tiles (canvas C3). When a stream arrives, they become bubbles and the stream takes the stage | CM5 component tests for both states and the change between them |
| In Focus, the focused stream fills the window, other streams are thumbnails on the right edge with their streamer's face on the thumbnail, and everyone else is a bubble on the left edge with the current speaker enlarged (canvas C2) | CM5 component tests; CM9 owner pass |
| In Grid and Fit, each streamer's face sits on their own tile and everyone else is in a left column that covers no stream (canvas C4) | CM5 component tests |
| A face never appears twice on screen | CM5 component test over every layout with one streamer who has a camera |
| Turning a camera on or off, or a participant leaving, never interrupts any stream's playback | CM4 component test (the fake viewer sessions: no re-dial); CM8 e2e reads `/statusz` |
| Camera video and voice stay inside the lip-sync window D10 sets | CM0 measurement; CM9 repeat on the reference deployment |
| A room at the camera cap on the weakest device CM0 tests keeps every stream at its frame rate | CM0 measurement |
| Nothing changes when cameras are off on the relay: no capability, no UI, no new relay behaviour, the room records byte-identical | CM2 off-path test (the docs/44 RM2 shape); CM5 component test |
| A client that does not ask for the camera capability (desktop broadcaster, iOS app, an older web app) sees the room exactly as today | CM1 vectors; CM2 test with a capability-less session beside a camera |
| A gawk ban or kill, a room removal, or the participant leaving ends their camera at once | CM2 tests; CM8 e2e |
| Removing or banning a participant closes the camera sessions they are watching, on every pod, without relying on their client | CM2 unit test; CM8 `e2e-cluster` step with a viewer on an edge pod that ignores the removal |
| A camera can be watched only by someone in its room: a `/subscribe` to a camera without a valid view ticket for that room is refused on every pod, and `#/view/<id>` of a camera shows nothing | CM2 unit tests (no ticket, expired, another room's ticket); CM8 `e2e-cluster` step against an edge pod |
| A phone browser can turn its camera on, pick front or back, and see the call in a layout made for its width | CM6 component tests at phone width; CM9 owner pass on a phone |
| Cameras are screened by `gawk-sampler` from the first release, and a flag says it is a camera | CM7 tests in `gawk-sampler` and `gawk-admin` |
| Every new knob reaches production through `registryOptions` | The R2 carry-all test, extended |

---

## 2. Owner decisions (2026-11-27)

| # | Question | Decision |
|---|---|---|
| OD1 | Which kind of call? Camera-only, or cameras beside game streams | **Both.** One model has to cover a room with no stream and a room with several. |
| OD2 | Which of the four mocked directions? (A: one tile per person and per screen; B: the streamer's face on their stream, others in a row below; C: full-bleed stream with floating face bubbles; D: cameras in the pinned side panel) | **C's Focus.** The follow-up boards C2, C3 and C4 extend it to several streams, to no stream, and to Grid. |
| OD3 | Camera resolution. The first draft proposed 640×360 at about 350 kbit/s | **720p30 at about 1.2 Mbit/s** (D5). 360p is too soft. CM0 still confirms the bitrate. |
| OD4 | Grid and Fit: where do faces go? | **A streamer's face on their own stream by default, with an option to dock those faces to the gutter** (D15, §5.5). People who are not streaming are in the gutter either way. |
| OD5 | Does a camera require being in voice? | **No.** Independent switches (D9). |
| OD6 | Is a joinable camera ID acceptable? | **No. Watching a camera requires a view ticket from the room, in the first version** (D22). |
| OD7 | The per-room camera cap | **8, operator can raise** (D8), pending CM0. |
| OD8 | Are cameras screened by `gawk-sampler`? | **Yes, from day one** (D23). |
| OD9 | Which clients are in the first version? | **The web app including phones.** The phone layout is part of this milestone (CM6), not a follow-up. Native apps stay out (§7). |
| OD10 | What happens to my camera when I leave voice or the tab goes to the background? | **It stays on until I turn it off** (D6). |
| OD11 | Must removing someone from a room also close the camera sessions they are already watching? | **Yes, required** (D22). The relay guarantees it; it does not rely on the client. |
| OD12 | If CM0 finds a phone cannot carry eight 720p cameras, what gives? | **The phone shows fewer faces** (speaker first, the rest initials behind "+N"). No second picture per camera, no lower rate for everyone. |
| OD13 | What does OD4's dock option move? | **The streamers' faces only.** People who are not streaming are always in the gutter. |
| OD14 | Who gets a face on the stage? | **Everyone in voice and everyone with a camera on** (D14). People only watching stay in the People panel. |
| OD15 | The camera's starting state on the join card | **Off the first time, then whatever I last joined with on this device** (D19). |
| OD16 | Do rooms without cameras get the centred call bar? | **No.** Only camera-capable rooms; elsewhere today's voice dock is unchanged (D17). |
| OD17 | If faces lag voice outside the lip-sync window and the camera path cannot be made faster | **Ship it and state the measured number.** Voice latency is never spent on matching video; an unpleasant skew is the milestone's go/no-go (D10). |
| OD18 | Which screening mode do cameras run in before calibration? | **The same mode and thresholds as streams**, from the first day (D23). False flags on faces are expected until CM7 tunes them. |
| OD19 | Does the speaker's bubble grow? | **In Focus only** (D16). |
| OD20 | Do events and announcements fire for cameras? | **Events yes, marked as cameras.** "Started streaming" announcements ignore cameras by default (D12). |

The rest of §3 is this design's proposal, written as decisions so the
owner pass can revise them in place. What is still open is in §10.

---

## 3. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **A camera is an ordinary broadcast**, published from the browser over `CONNECT /publish` and watched over `/subscribe`. The hub, the keyframe cache, the cascade and the media wire do not change. | The property that kept R42 bounded (docs/44 D1). A camera joins instantly for the same reason a stream does: the cached keyframe. |
| D2 | **A camera is not an attachment.** It lives in a separate **camera roster**, as the voice roster does (docs/80 D15): an entry is a participant ID, the camera's broadcast ID and flags. `RoomState` and `RoomAttachment` stay byte-identical. | `-max-room-broadcasts` defaults to 4, and "N streaming", the What to show list, the number keys and Focus all iterate attachments. Six cameras as attachments would fill the cap and all four of those. `RoomAttachment` also has no owner field, and its flags are masked, so a new bit would fail older parsers. |
| D3 | **A new room capability, `RoomCapCamera` (0x04).** The camera roster and the stream-owner list (D4) go only to participants that set it in `wantCaps`. A relay without `-room-cameras` never advertises it. | The R79 pattern. Desktop and iOS clients keep working without a release, and an operator who has not turned cameras on runs today's relay. |
| D4 | **The relay tells camera-capable clients who owns each stream.** The camera roster record carries a second list: attachment broadcast ID → participant ID. | The registry already tracks `ownerPID` per attachment (`roomsrv/registry.go`), but the wire never exposed it. The layout needs it to put a streamer's face on their own stream. |
| D5 | **Camera encode profile: 1280×720, 30 fps, about 1.2 Mbit/s** (OD3; CM0 confirms the bitrate), through the existing codec negotiation. Never the stream ladder (`ladder.ts`, tuned for 1080p60). A camera that delivers less than 720p is sent at what it delivers. | In a call with no stream, two people are two tiles of about 600 px each, and on a high-DPI screen 360p is visibly soft there. Per-viewer adaptation is a standing non-goal (ROADMAP R4), so every viewer receives every visible camera at this one rate: 8 cameras are about 10 Mbit/s of faces per viewer. That is the cost of this decision, and CM0 measures whether a phone can carry it (§9.1). |
| D6 | **A camera is room-scoped.** It ends when its owner turns it off (close code 4014, R78: at once), leaves the room, is removed, or the room ends. Nothing else ends it (OD10): leaving voice does not, and a tab in the background keeps sending. A dropped control session keeps the camera for a short grace (proposal: 10 s) so a reload is invisible. | The opposite of a stream, which outlives its room (docs/44 D1). A camera with no room is a face broadcasting to nobody who asked for it. |
| D7 | **The camera publish is authorised by a ticket from the room's home pod**, not by `-publish-secret`. `CameraStart` on the control session is answered with a short-lived, single-use, HMAC-signed ticket (the docs/80 D7 shape: room, participant, nonce, home epoch, 30 s expiry). The browser dials `CONNECT /publish?camera=<ticket>`. Any pod verifies it offline. | A participant who is only watching holds no publish secret, and must not be given one. The ticket also tells the relay at publish time that this broadcast is a camera, which is what lets it apply D8's caps and D12's label before the first frame. |
| D8 | **Caps, all through `registryOptions`**: `-room-cameras` (default **off**), `-max-room-cameras` (proposal: 8 per room), `-camera-max-bandwidth` (proposal: 2 Mbit/s per camera, enforced at ingest; headroom over D5's rate). Flags, `GAWK_*` envs and Helm values. A camera counts toward `-max-broadcasts` and its viewers toward the subscriber caps, as any broadcast. | The R2 invariant. Default-off keeps every deployment byte-identical until the operator opts in. The per-room cap is the receive-side budget: each visible camera is one WebTransport session and one `VideoDecoder` in every viewer. |
| D9 | **Camera does not require voice.** Any participant in a camera-capable room may turn their camera on, in voice or not. The speaking ring needs voice (D16). | A group on another voice tool still benefits. It also keeps the two features independently deployable: cameras need no bridge. |
| D10 | **Lip sync is measured, not designed in.** Voice travels browser → bridge → Mumble on its own clock; camera video travels through the relay. v1 aligns nothing. CM0 measures the skew with the shipping stacks. Target: audio leads video by no more than 90 ms and lags by no more than 185 ms (ITU-R BT.1359's acceptability thresholds, to be confirmed against the source in CM0). If the measurement fails the target, the camera path is made faster where it can be (§9.1). If that is not enough, the feature ships with the measured skew stated, or the owner calls a no-go on it (OD17). Delaying voice is not an option, not even as a switch. | Voice's target is 100 ms mouth-to-ear (docs/80 OD31). Delaying it to match video spends conversational latency on a cosmetic. The owner's standing rule is that latency claims are measured before they go into a doc as facts. |
| D11 | **Cameras play at the live edge always**, whatever the room's playback preset. No deep buffer, no DVR catch-up, no striping. | A face two seconds behind the voice is worse than a face that drops frames. Presets are for streams. |
| D12 | **The relay labels a camera broadcast as one** (`kind: camera`) in `/statusz`, metrics, the `/internal/admin/*` responses and the events' `data`. A camera's start and stop are events like any broadcast's (OD20). A consumer that announces streams, such as the Discord announcer, ignores cameras unless configured otherwise. | Operators and event consumers must be able to tell a face from a game stream: a "started streaming" announcement for every camera toggle is noise, and a moderator looking at a flag needs to know what they are about to see. |
| D13 | **One layout model: a person is a tile when nothing is streamed and a bubble when something is.** There is no meeting mode and no switch. | OD1. The room's shape follows what is in it. |
| D14 | **Who gets a face**: everyone in voice (bridged gawk users and native Mumble users) and everyone with a camera on. With a camera: live video. Without: their initial. A native Mumble user never has a camera and carries the existing `Mumble` tag. People who are only watching stay in the People panel. | The stage shows the people who are present in the call. Twenty silent watchers as bubbles would bury them. |
| D15 | **A face appears once.** A streamer's face rides on their own stream (tile, thumbnail, or, for the focused stream, the left column with a screen icon). Everyone else is in the left column. In Grid and Fit a viewer can dock the streamers' faces into the left column instead (OD4), remembered per device. | Canvas C2 and C4. Two bubbles for one person reads as two people. The dock option is for a game whose HUD sits under the face. |
| D16 | **Speaking is shown from the existing local detection** (docs/80 D16): a green ring on the bubble, a green outline on a tile. In Focus the speaker's bubble is enlarged; in Grid and Fit nobody grows. Only people in voice see it. | No new signal on the wire. In Focus there is one place to look, so enlarging is useful; with no focus it would make the layout jump. |
| D17 | **The call bar**: Microphone (with its settings caret), Camera (with a settings caret), Share screen, Deafen, centred in the footer. It replaces the voice dock's position, in camera-capable rooms only (OD16): a room without the capability keeps today's dock exactly. Not in voice, the microphone and deafen buttons are one **Join voice** button. | One place for "what am I sending". The dock sits bottom-left today and has no room for two more controls beside the layout segment. |
| D18 | **Share screen is today's Start streaming here.** Same flow, same attach, a shorter label in the bar. | No second way to start a stream. |
| D19 | **The first join card gains a camera preview** with microphone and camera toggles, above the existing talk-mode choice, and one new sentence: "Your camera is seen only by people in this gawk room." The camera toggle is off the first time and afterwards starts as it was at the last join on this device (OD15). | People check how they look before they are seen. The sentence is needed because the card already says the voice channel is shared with people outside gawk, and that is not true of the camera. |
| D20 | **A camera is subscribed only while its face is on screen**, and every camera session closes under What to show → None and when the tab is hidden. | The R86 rule (docs/90 D3) applied to faces: hiding is unsubscribing. |
| D21 | **Mumble sees nothing.** Cameras are a gawk room feature; the bridge and murmur are untouched. | docs/80 OD4: the integration is voice only. |
| D22 | **Watching a camera requires a view ticket** (OD6). A camera-capable participant asks the room for one (`CameraWatch`) and gets a short-lived, HMAC-signed ticket naming the room, the participant and the home epoch (proposal: 60 s, reusable inside that time, renewed by the client as it runs out). The browser dials `/subscribe/<id>?camera=<ticket>`. A pod serving a camera broadcast refuses a viewer whose ticket is missing, expired or for another room. The ticket is checked when the session opens. **When a participant is removed or banned, the relay closes every camera session opened with their tickets, on every pod** (OD11): a camera viewer session records the participant from its ticket for this. | A face is more sensitive than a game, and a camera's ID reaches every participant. Without the ticket, anyone who was once in the room could keep opening `#/view/<id>`. A room-scoped ticket, not one per camera, keeps a face appearing on screen from costing a control round trip. |
| D23 | **Cameras are screened from day one** (OD8). `gawk-sampler` samples a camera's cached keyframe as it does any broadcast's. Cameras run in the same mode and with the same thresholds as streams from the first day (OD18). The screening route tells the sampler the broadcast's kind, so a flag in `gawk-admin` says it is a camera and CM7 can report camera flags apart from stream flags. | One mode is one thing for an operator to reason about. The sampler's model was calibrated on game streams, so false flags on faces are expected at first; the kind on every flag is what lets CM7 measure that and decide whether cameras need thresholds of their own. The sampler's prohibitions hold unchanged: no frame bytes written, logged, posted or exposed. |

---

## 4. Where it plugs into the existing code (grounded)

- **Room protocol**: `gawk-server/wire/room.go`. Capabilities are a bit
  mask (`RoomCapChat` 0x01, `RoomCapVoice` 0x02). Event and command
  kinds reserve 0x40–0x4F for chat and use 0x50–0x5F for voice; a reader
  skips an unknown kind (`ErrUnknownRoomKind`) and a relay answers
  `RoomRejectUnsupported`. The voice roster is a snapshot event sent
  after every `RoomState` and on every change, to participants that
  asked for the capability. Cameras follow that shape in a new
  0x60–0x6F sub-range.
- **Ownership**: `internal/roomsrv/registry.go` records `ownerPID` on
  each attachment (set at attach, adopted on re-attach). D4 exposes it.
- **Attach proof**: a `RoomCommand` Attach carries the broadcast's
  16-byte resume token (docs/44 D9). `CameraAttach` uses the same proof.
- **Caps**: `-max-room-broadcasts` defaults to 4 and
  `-max-room-participants` to 50 (`internal/config/config.go`);
  `-voice-max-users-per-room` to 25.
- **Stop ends the broadcast**: close code 4014 (R78, docs/79) already
  ends a broadcast at once instead of a five-minute grace. A camera
  toggle uses it.
- **Capture**: `gawk-app/src/media/capture.ts` owns `getDisplayMedia`;
  `features/room/voice/voiceCapture.ts` owns `getUserMedia` for the
  microphone. Camera capture is a third entry point, and the CLAUDE.md rule "trust
  the frame in hand" applies: configure the encoder from the first
  `VideoFrame`, never from `getSettings()`.
- **Room view**: `RoomScreen.tsx` (header, footer, the layout segment),
  `RoomTile.tsx` (one stream), `fitLayout.ts`, `roomVisibility.ts`,
  `voice/VoicePanel.tsx` (the dock, `MicToggle`, `DeafenToggle`, the
  first-join card), `voice/speaking.ts` (local speaking detection).
  Copy lives in `roomCopy.ts` and `voice/voiceCopy.ts`.
- **Narrow screens**: Grid and Fit already show as Focus on a narrow
  screen. The phone layout (CM6) starts from Focus.
- **Screening**: `gawk-sampler` reads keyframes from the relay's
  `/internal/screening/…` route, not through `/subscribe`, so D22's
  view ticket does not touch it. Its shared types are the public
  `gawk-server/screening` package; adding the kind there needs commits
  touching `gawk-admin/` and `gawk-sampler/` in the same PR.
- **Not checked yet**: how `internal/transport` gates `/publish` and
  `/subscribe` today, and how an edge pod learns facts about a
  broadcast it pulls. D7 and D22 depend on both; CM2 starts by reading
  them and revises §5.2 if the shape here does not fit.

---

## 5. Design

### 5.1 Wire

All in `gawk-server/wire/room.go`, mirrored in `wire.ts` and
`gawk-broadcast-desktop/crates/wire` with golden vectors byte-identical
across the three copies. Exact layouts and numbers are fixed in CM1.

| Addition | Direction | Carries |
|---|---|---|
| `RoomCapCamera = 0x04` | both | The capability bit in `wantCaps` and `caps`. |
| `RoomCommandCameraStart = 0x60` | client → relay | Nothing. Asks for a publish ticket. Rejected with `Unsupported` (cameras off), `Limit` (`-max-room-cameras`), `Unavailable`. |
| `RoomEventCameraTicket = 0x60` | relay → one participant | The ticket (D7). Out of sequence, as `VoiceTicket` is. |
| `RoomCommandCameraAttach = 0x61` | client → relay | The minted broadcast ID and its resume token, as proof. |
| `RoomCommandCameraStop = 0x62` | client → relay | Nothing. Removes the caller's camera from the roster. |
| `RoomCommandCameraWatch = 0x63` | client → relay | Nothing. Asks for a view ticket (D22). Allowed for any camera-capable participant, with or without a camera of their own. |
| `RoomEventCameraViewTicket = 0x62` | relay → one participant | The view ticket and its expiry. Out of sequence. |
| `RoomEventCameraRosterUpdated = 0x61` | relay → capable participants | A snapshot, replace never merge: the cameras (participant ID, broadcast ID, flags with `Live`) and the stream owners (attachment broadcast ID, participant ID). Sent after every `RoomState` and on every change to either list. |

A full roster (8 cameras, 4 stream owners, six-character IDs) is well
under 200 bytes, far inside `MaxRoomRecordSize`.

The Rust mirror carries the vectors and needs no runtime use. Because
`gawk-ios` uses the desktop workspace's `wire` crate by path, the PR
that changes it needs a commit touching `gawk-ios/` (`CONTRIBUTING.md`).

### 5.2 Relay

1. `CameraStart`: the home pod checks the capability, the per-room cap
   and that the participant has no camera, then mints the ticket.
2. `CONNECT /publish?camera=<ticket>`: any pod verifies the ticket
   offline (signature, expiry, home epoch), skips the publish-secret
   gate, mints a broadcast ID, marks the broadcast `kind: camera` and
   applies `-camera-max-bandwidth`. The R39 ban gate applies as on any
   publish.
3. `CameraAttach`: the home pod verifies the resume token, checks the
   ticket's nonce has not been used, and adds the roster entry.
4. `CameraWatch`: the home pod checks the capability and mints a view
   ticket for the room (D22).
5. `CONNECT /subscribe/<id>?camera=<ticket>`: a pod that serves a
   camera broadcast verifies the ticket offline and checks its room
   against the room the broadcast was published for (taken from the
   publish ticket). No ticket, an expired one or another room's: the
   session is refused, with the code CM2 picks from `wire.go`. A
   stream's `/subscribe` is unchanged. An edge pod has to know that a
   broadcast it pulls is a camera, and of which room, before it lets a
   viewer in. How it learns that is not designed yet (§4); the rule is
   that it fails closed.
6. When a participant is removed or banned, the home pod has every
   pod close the camera viewer sessions that carry that participant
   (D22). How the instruction reaches other pods is CM2's to design,
   on the path an operator kill takes; the bus carries no room
   control today, so this is checked first (§4).
7. The home pod ends a camera broadcast (through the path an operator
   kill uses) when its owner leaves or is removed, the room ends, or
   the reconnect grace expires (D6). A camera whose publisher session
   ends first is removed from the roster by the hub's existing
   lifecycle hook.

A non-home pod proxies the commands to the home, as it does every room
command (docs/44 §4.5). The camera roster is rebuilt from live sessions
after a home move, like the participant roster: a client whose camera
is live re-sends `CameraAttach` after a resync.

Cameras never appear in the `Room` CR. They are as dynamic as
participants (docs/44 D5).

### 5.3 Publishing from the browser

- `getUserMedia({ video })` with the profile's constraints, then the
  existing encoder path with the camera profile (D5). On a phone the
  constraint includes `facingMode`, and the Camera settings popover
  switches front and back.
- A second publish pipeline beside the screen's. Whether the current
  broadcaster session and its store can run two instances in one tab is
  not known; CM0 answers it and CM3 builds whatever it needs.
- On Chrome the encode runs on the main thread (no worker-side
  `MediaStreamTrackProcessor`, CLAUDE.md "Rejected"). A tab that shares
  a screen and a camera runs two encoders there. CM0 measures it.
- Lowest-latency uplink only: datagrams, keyframes on reliable streams.
  The Reliable uplink's per-GOP carrier is for streams.
- Camera off closes the publisher with 4014 and sends `CameraStop`.
  Camera on is a new broadcast with a new ID every time.
- The self view is the local track, mirrored, never a round trip.

### 5.4 Watching

A face is a small player: one `/subscribe` session opened with the
room's view ticket (D22), one `VideoDecoder`, no audio lane, live edge
(D11). The client keeps one unexpired view ticket while any face is on
screen and asks for the next before it runs out. It mounts when the face is on screen
and unmounts when it is not (D20). The decoder's output is drawn
cropped to the bubble or tile.

### 5.5 Layout

The stage has three zones. Which are present depends on what is in the
room and the layout segment.

| Room state | Stage | Left column | Right column |
|---|---|---|---|
| No stream (C3) | People as tiles, in a grid | none | none |
| Streams, Focus (C, C2) | The focused stream, full-bleed | Floats over the stream: the focused stream's owner (with a screen icon) and everyone not streaming; the speaker enlarged (136 px, others 84 px) | The other streams as thumbnails (216×122), each a button that focuses it, with its streamer's face on the thumbnail's corner |
| Streams, Grid or Fit (C4) | The streams, laid out as today | A reserved gutter (about 170 px) so no face covers a stream: everyone not streaming. Not reserved when it would be empty | none; each streamer's face (72 px) sits in the bottom-left of their own tile. With faces docked (OD4), the streamers' faces are in the gutter too, each with a screen icon, and the tiles carry none |

Rules that hold in every state:

- **No stream → the layout segment is dimmed.** It lays out streams and
  there are none.
- **Switching Grid ↔ Focus moves only streams.** The left column stays
  where it is, and a streamer's face travels with their stream.
- **Tiles become bubbles when the first stream arrives**, and bubbles
  become tiles when the last one leaves. This is the one animated
  moment; under `prefers-reduced-motion` it is instant.
- **Overflow**: each column shows at most what fits the stage height.
  The rest collapse into a "+N" bubble that opens the People panel.
  Speakers are promoted into the visible set.
- **Idle**: bubbles and thumbnails fade with the rest of the chrome
  after the existing idle timeout. In Focus the current speaker's
  bubble stays.
- **Dragging**: in Focus the left column can be dragged to the right
  edge and the thumbnails to the left, remembered per device. This is
  for a game whose HUD sits under the default edge.
- **What to show** is unchanged and still lists streams only. Under
  None, faces fall back to initials and no camera downloads (D20).
- **A streamer with no camera** has no bubble on their stream; their
  name chip is enough.

The 170 px gutter costs stream area in Grid: at 1280 wide a 2×2 grid's
tiles are about 540×304 instead of 624×351. When everyone in the call
is streaming and faces are on their streams, there is no gutter and the
grid has its full width.

**Docking** (OD4): a face's context menu in Grid and Fit offers
`Move faces to the side`, and, once docked, `Put faces on streams`. It
is one setting for all streamers' faces, remembered per device. Focus
is not affected: its thumbnails keep their faces.

**Phones** (OD9) are in this milestone. A phone shows Focus only, as
today. Its layout for faces, thumbnails and the call bar is drawn on
the canvas before CM6 builds it (§10).

### 5.6 Controls

- **Call bar** (D17). Camera on is a filled button, the same treatment
  as the checked layout segment. The Camera caret opens a popover with
  the camera to use and a "Mirror my view" switch. Voice has no
  microphone picker today; the camera needs one on day one because
  laptops with two cameras and phones with front and back are the
  common case.
- **Join card** (D19): the existing "How do you want to talk?" card,
  with the preview above it. Joining voice with the camera toggle on
  starts the camera after the voice session connects.
- **Tile and bubble menus**: a face's context menu offers what the
  voice roster row offers today (per-person volume), plus "Hide
  camera for you", remembered per room like What to show.
- **Permission denied or no camera**: the Camera button stays, and
  pressing it shows one line saying why, as the microphone does
  (`failureText` in `voiceCopy.ts`).

### 5.7 New words

Every new string goes into `roomCopy.ts` or `voiceCopy.ts` with a test,
as the existing ones do. New labels in this design: `Camera`,
`Turn camera on` / `Turn camera off`, `Camera settings`, `Share screen`,
`Mirror my view`, `Hide camera for you`, `Move faces to the side`,
`Put faces on streams`, `Switch camera` (phones: front and back), and the
join card's camera sentence. Everything else on the canvas is an existing label.

---

## 6. Risks and prohibitions

- **A view ticket is checked when a session opens**, so a removal has
  to close the sessions already open (D22, OD11). Until CM2 has built
  and tested that across pods, a removed person with a modified client
  could keep watching faces; the milestone does not ship without it.
- **Someone who leaves on their own** keeps a valid ticket for up to
  its lifetime (proposal: 60 s). Their open camera sessions are closed
  the same way when their control session ends.
- **A camera's broadcast ID is still never displayed** and never in a
  share link. The view ticket is the gate; the ID staying out of sight
  is the second line.
- **Never expose a camera's broadcast ID raw** in `/statusz`, metrics
  or telemetry; key it by the per-process HMAC like any broadcast.
- **`gawk-sampler` scores faces from the first release** (D23). Its
  prohibition holds unchanged: no frame bytes are written, logged,
  posted or exposed. Its model was calibrated on game streams and
  cameras run in the streams' mode (OD18), so an operator who enforces
  on streams should expect false flags on faces until CM7 has tuned
  them. The terms must say that cameras are screened.
- **No camera video is ever written to disk, logged or recorded by
  gawk.** The docs/80 D24 rule for voice, extended; CM2 adds the
  source-walk test.
- **Echo**: docs/80 OD17 accepted echo without a headset and its
  echo-cancellation rows are still open. Camera calls move people from
  headsets to laptop speakers. CM0 includes those rows.
- **Terms**: the terms of use (docs/29) describe game streams. CM7
  revises them before cameras are on by default anywhere.
- **Churn**: every camera toggle is a broadcast minted and ended, and
  in cluster mode a Lease claimed and released. `-conn-rate-limit`
  (3/s, burst 10 per address) bounds it per client. CM0 measures a
  toggle's cost on the reference deployment.

---

## 7. Out of scope

- **The iOS app and the desktop broadcasters.** docs/67 OD1 calls
  camera broadcasting a different product, and the desktop apps have
  no camera path. They do not set `RoomCapCamera` and see today's
  room. Watching cameras in the iOS room player is a follow-up.
- **Per-viewer adaptation** (simulcast, SVC). A standing non-goal. The
  consequence is a camera cap sized for a friend group, not a meeting
  of thirty, and one 720p rate for every viewer, phones included.
- **Background blur, virtual backgrounds, filters, reactions.**
- **Recording.**
- **Room chat.** Still reserved (docs/44 §4.11).
- **Cameras in a plain broadcast** outside a room.
- **Aligning voice and video clocks** (D10), unless CM0 forces it.

---

## 8. Rejected

- **A · Call grid** as the only model: one tile per person and per
  screen. It is what C3 looks like with no stream, and it is kept for
  exactly that state. With streams, faces compete with games for area,
  and the room looks like every meeting tool.
- **B · Facecam** as the only model: the streamer's face on their
  stream and everyone else in a row below. Its face-on-the-stream idea
  is kept (D15). The row takes height from every stream.
- **D · People rail**: cameras in the pinned Voice panel. The smallest
  change, and 320 px of width for faces nobody chose to look at.
- **Compositing the camera into the stream on the broadcaster** (a
  burnt-in facecam). No extra session, but a viewer cannot hide or move
  it, it spends stream pixels and bitrate, the native broadcasters have
  no camera, and it does nothing for people who are not streaming.
- **Cameras as attachments** (D2).
- **A WebRTC SFU for the call.** Set aside project-wide (CLAUDE.md).
  The camera path here is the same self-owned pipeline as the stream.
- **Delaying voice to match video** (D10).

---

## 9. Chunks

| Chunk | What | Acceptance criteria |
|---|---|---|
| **CM0** | Measurement spike, before any design is final (§9.1) | Every row of §9.1 has a number or a "could not measure, because"; D5, D8 and D10 are revised in place from them; a go/no-go for the milestone is written into §11 |
| **CM1** | Wire: `RoomCapCamera`, the 0x60–0x63 commands, the 0x60–0x62 events, in Go, TS and Rust | Golden vectors byte-identical across the three copies; a parser without the kinds skips them; fuzz and round-trip tests as for the voice records; the `gawk-ios` commit |
| **CM2** | Relay: publish and view tickets, `/publish?camera=`, `/subscribe/<id>?camera=`, the camera roster and stream owners, room-scoped lifecycle, `kind: camera` labelling, the three options through `registryOptions` + envs + Helm. Starts by reading `internal/transport` and the edge pull path (§4) | Unit tests per command and reject reason; a replayed or expired publish ticket is refused; a camera `/subscribe` with no view ticket, an expired one or another room's is refused on the origin and on an edge, and a stream's `/subscribe` is unchanged; a removal, a ban and a leave each close that participant's open camera viewer sessions on the origin and on an edge; a camera dies with its owner's session, a removal, a ban and a room end; a non-home pod proxies; the off-path test (cameras off: byte-identical records, `Unsupported`); the carry-all options test; the no-recording source walk |
| **CM3** | App publish: camera capture, the camera profile, the second pipeline, toggle, reconnect, device choice | Unit tests for the profile and the encoder configured from the first frame; toggle off sends 4014 and `CameraStop`; a dropped publish reconnects on its resume token inside the grace; permission denied and no device each render their line |
| **CM4** | App watch: the face player, the view ticket and its renewal, subscribe-while-visible, live edge only | Component tests with the fake viewer sessions: a face that leaves the screen closes its session, None closes all, a camera change re-dials no stream, a face mounting after the ticket expired gets a fresh one first |
| **CM5** | Room view: the layout model (§5.5), docking, the call bar, the join card preview, speaking rings, overflow, idle, dragging, the new copy | Component tests for each row of §5.5's table and each rule under it; the "appears once" test; every new string in a copy module with a test; no camera UI when the room lacks the capability |
| **CM6** | Phone layout and phone camera (OD9): one of the three canvas boards (§10), then built; front and back camera; the visible-face limit if CM0 calls for one (OD12) | The owner picks a board; component tests at phone width; the footer and the call bar fit on screen with the thumb bar's 44 px targets; switching front and back keeps the same roster entry |
| **CM7** | Moderation and policy (D23): the kind on the screening route (`gawk-server/screening`, with the `gawk-admin` and `gawk-sampler` commits it requires), calibration on faces, the admin portal's label, the events' `data` field (with the `gawk-admin` commit the `events` package requires), the terms text | A camera is sampled and its flag is distinguishable from a stream's in the portal, in an event and in `/statusz`; cameras run in the stream mode, with a test; the false-flag rate on faces at the stream thresholds, and whether cameras need their own, are recorded in §11; the announcer ignores a camera's events by default, with a test; the terms revision, saying cameras are screened, is merged |
| **CM8** | e2e: a rooms step with Chrome's fake camera device, single-pod and `e2e-cluster` | Two browsers in a room: camera on is seen by the other, camera off removes it, the owner leaving ends the broadcast (`/statusz`), a stream beside it never re-dials |
| **CM9** | Owner pass on the reference deployment | Every milestone criterion marked "owner pass" checked on a laptop and a phone; the lip-sync and load measurements repeated; results in §11 |

Order: CM0, then CM1 → CM2, with CM3 and CM4 against CM2's relay, then
CM5. CM6 and CM7 can run beside CM5, and both are part of the first
release (OD8, OD9): no relay runs with cameras on before the sampler
knows a camera from a stream. Cameras stay default-off after it, as
every optional feature does.

### 9.1 CM0: what is measured

With the shipping stacks (Chrome, Firefox and Safari on desktop; Chrome
on Android and Safari on iOS; the reference deployment's relay and
bridge), not a lab model.

| Question | Decides |
|---|---|
| Glass-to-glass latency of a 720p30 camera through the relay, and mouth-to-ear latency of voice on the same pair of machines: the skew, and its spread over ten minutes | D10. If outside the window: lower the camera's latency (no pacing, drop-to-latest), or accept and say so. Not: delay voice. |
| Decode and render cost of 1, 4 and 8 camera sessions beside one and four 1080p60 streams, on the reference laptop and on a mid-range phone: stream frame rate, dropped frames, CPU | D8's `-max-room-cameras` default, and whether a phone needs a lower visible-face limit |
| Whether a browser limits concurrent WebTransport sessions per page or per origin, and at what number | D8; whether faces need to share a session later |
| A screen encoder and a camera encoder in one tab on Chrome's main thread: encode queue depth and stream frame rate with the camera on and off | Whether CM3 needs the camera encode elsewhere, or a lower profile while sharing |
| Whether the current broadcaster session and stores run twice in one tab | CM3's size |
| Picture quality of faces at 720p30 at 800, 1200 and 1800 kbit/s, in a bubble and in a two-person C3 tile on a high-DPI screen; hardware or software encoder chosen per browser; what each phone's camera and encoder deliver when asked for 720p30 | D5's bitrate |
| A phone on a mobile network receiving 4 and 8 cameras at D5's rate beside one stream: stalls, dropped frames, data used per minute | Whether a phone needs a lower visible-face limit (the only lever without per-viewer adaptation) |
| Time from the Camera click to the first frame on another machine; relay and API-server cost of one toggle in cluster mode | The "under 1 s" criterion; the churn risk in §6 |
| Echo with laptop speakers and no headset, each browser's echo cancellation on | Whether docs/80 OD17 still holds once cameras ship |

---

## 10. Open decisions for the owner

Two rounds of questions were answered on 2026-11-27 and are OD3–OD20
in §2. One remains.

| # | Question | State |
|---|---|---|
| OD-A | Which phone layout? | Three boards are on the canvas under "Phone, three ways to hold a call": P1 a strip of faces and thumbnails under the stream, P2 bubbles over the stream as on desktop, P3 two pages (stream, people) with a swipe. The owner picks one; CM6 builds it. |

---

## 11. Results

Empty until CM0 runs.
