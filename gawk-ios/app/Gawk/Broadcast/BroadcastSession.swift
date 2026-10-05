import Foundation
import Observation

/// What the Broadcast screen shows (docs/67 D7, docs/70 B1–B4).
enum BroadcastPhase: Equatable {
    case idle
    case connecting
    case live(code: String, joinLink: String)
    case resuming(attempt: UInt32)
    /// Stop was asked for; the core is winding the session down.
    case stopping
    case ended(reason: String?)
}

/// The room a broadcast joins (docs/70 D16): a code with what it
/// presents, or a room minted from this broadcast.
enum RoomChoice: Equatable {
    case join(code: String, attachKey: String, creatorToken: String)
    case create

    var code: String? {
        if case .join(let code, _, _) = self { return code }
        return nil
    }
}

/// A room card after something happened to the room (docs/60 D10, docs/70
/// D17): a title and a body, dismissed by the user.
struct RoomCard: Equatable {
    let title: String
    let body: String
}

/// The Quality menu (docs/70 D12): Auto is docs/67 D19's rule at start.
enum QualityChoice: String, CaseIterable, Codable {
    case auto
    case standard
    case cellular

    var title: String {
        switch self {
        case .auto: "Auto"
        case .standard: "Standard"
        case .cellular: "Cellular"
        }
    }

    /// The line under the Quality row on Ready.
    var line: String {
        switch self {
        case .auto: "1080p · 60 fps on Wi-Fi, 720p · 30 on cellular"
        case .standard: "1080p · 60 fps · up to 8 Mbps"
        case .cellular: "720p · 30 fps · up to 3 Mbps"
        }
    }

    /// The rung: Auto takes Cellular on an expensive path, Standard
    /// otherwise.
    func resolve(expensivePath: Bool) -> Quality {
        switch self {
        case .auto: expensivePath ? .cellular : .standard
        case .standard: .standard
        case .cellular: .cellular
        }
    }
}

/// One broadcast as the UI sees it: the core's `Broadcaster` behind a
/// main-actor model. Capture (ScreenCaptureKit on a device, D27's test
/// source in debug builds) feeds it frames and audio from its own queues.
@MainActor
@Observable
final class BroadcastSession {
    private(set) var phase: BroadcastPhase = .idle
    private(set) var viewerCount: UInt32 = 0
    private(set) var failure: String?
    /// The first Live of this broadcast, for the LIVE chip's clock.
    private(set) var liveSince: Date?
    /// The rung in use (D12).
    private(set) var quality: Quality?
    /// Where frames went and the upload rate, sampled once a second (K4).
    private(set) var counters: BroadcastCounters?
    /// The last broadcast's numbers, for the summary card (B4, K5).
    private(set) var summary: BroadcastSummary?
    /// The last broadcast's code, which viewers keep for the relay's grace.
    private(set) var lastCode: String?
    /// A start refused before it went live (D25), with its HTTP status.
    private(set) var refusal: (reason: String, status: UInt16?)?

    // MARK: Room (D16–D18, K2)

    /// The room the next start joins, or the one this broadcast is in.
    private(set) var pendingRoom: RoomChoice?
    private(set) var room: RoomView?
    private(set) var roomAttached = false
    /// A gated static room admitted us as a watcher: ask for its key.
    private(set) var roomNeedsKey = false
    private(set) var roomStatus: String?
    var roomCard: RoomCard?

    /// The core object; capture threads read it through `media`.
    @ObservationIgnored private var broadcaster: Broadcaster?
    @ObservationIgnored let media = MediaSink()
    /// Called when the broadcast ends, whoever ended it: capture stops.
    @ObservationIgnored var onEnded: (() -> Void)?
    /// A room seen, with the relay it's on: Your rooms keeps it (K12).
    @ObservationIgnored var onRoomSeen: ((RoomView, String) -> Void)?
    /// The Live Activity's hooks (D15).
    @ObservationIgnored var activity: BroadcastActivity?
    @ObservationIgnored private let identity: IdentityStore
    /// The relay the current broadcast publishes to, whose identity it holds.
    @ObservationIgnored private(set) var relay: String?
    /// Which broadcast is current: a stopped one's late reports are dropped.
    @ObservationIgnored private var generation = 0
    /// How long a stop waits for the core to confirm before the screen
    /// gives up on it and says so.
    @ObservationIgnored var stopGrace: Duration = .seconds(5)
    @ObservationIgnored private var tracker = SummaryTracker()
    @ObservationIgnored private var sampler: Task<Void, Never>?
    /// We asked to leave or end the room: its end is quiet.
    @ObservationIgnored private var roomLeaving = false

    init(identity: IdentityStore) {
        self.identity = identity
    }

    var isActive: Bool {
        switch phase {
        case .idle, .ended: false
        default: true
        }
    }

    var liveCode: String? {
        if case .live(let code, _) = phase { return code }
        return lastCode
    }

    /// Starts publishing. A stored identity for this relay is reclaimed
    /// (R17), so a restart within the grace keeps the code (G6).
    func start(
        relayURL: String, secret: String, quality: Quality,
        nickname: String, telemetry: Bool, insecure: Bool,
        captureSource: String = "test-source"
    ) {
        guard !isActive else { return }
        let stored = identity.load(relay: relayURL)
        var roomCode = "", attachKey = "", creatorToken = "", roomNew = false
        switch pendingRoom {
        case .join(let code, let key, let token): (roomCode, attachKey, creatorToken) = (code, key, token)
        case .create: roomNew = true
        case nil: break
        }
        let options = BroadcastOptions(
            relayUrl: relayURL,
            publishSecret: secret,
            broadcastId: stored?.code ?? "",
            resumeTokenHex: stored?.token ?? "",
            quality: quality,
            roomCode: roomCode,
            roomAttachSecret: attachKey,
            nickname: nickname,
            insecure: insecure,
            telemetry: telemetry,
            captureSource: captureSource,
            roomNew: roomNew,
            roomCreatorTokenHex: creatorToken
        )
        failure = nil
        refusal = nil
        summary = nil
        roomCard = nil
        roomStatus = nil
        room = nil
        roomAttached = false
        roomNeedsKey = false
        roomLeaving = false
        viewerCount = 0
        liveSince = nil
        counters = nil
        tracker = SummaryTracker()
        self.quality = quality
        phase = .connecting
        relay = relayURL
        generation += 1
        let listener = Listener(
            session: self, generation: generation, relay: relayURL, identity: identity
        )
        let b = Broadcaster.start(options: options, listener: listener)
        broadcaster = b
        media.attach(b)
        startSampling()
    }

    /// Ends the broadcast. The screen leaves the live state at once
    /// (`.stopping`); `.ended` follows when the core has closed the session,
    /// or after `stopGrace` with a reason if it never confirms.
    func stop() {
        media.attach(nil)
        broadcaster?.stop()
        broadcaster = nil
        switch phase {
        case .idle, .ended, .stopping: return
        default: break
        }
        phase = .stopping
        let stopped = generation
        Task { [weak self, stopGrace] in
            try? await Task.sleep(for: stopGrace)
            guard let self, self.generation == stopped, self.phase == .stopping else { return }
            self.finish(reason: "The app stopped broadcasting, but the server didn't confirm it.")
        }
    }

    /// D12: a new rung while live, on the same code (`Session::republish`
    /// in the core); viewers see a short freeze. Before going live it's
    /// just the rung the start uses.
    func setQuality(_ q: Quality) {
        guard q != quality else { return }
        quality = q
        broadcaster?.setQuality(quality: q)
    }

    /// K6, D14: the next start mints a new code on this server.
    func useNewCodeNextTime(relay: String) {
        identity.forgetIdentity(relay: relay)
        lastCode = nil
        summary = nil
    }

    func dismissSummary() {
        summary = nil
    }

    func clearRefusal() {
        refusal = nil
    }

    /// Where frames went so far (the live status and diagnostics).
    func currentCounters() -> BroadcastCounters? {
        broadcaster?.counters()
    }

    /// The capture ended on its own (the system, or an error); the reason
    /// stays on screen after the broadcast stops (D7).
    func noteCaptureEnded(_ reason: String?) {
        if let reason { failure = reason }
    }

    /// D19: the network path changed while live.
    func pathChanged() {
        broadcaster?.pathChanged()
    }

    // MARK: Room actions (D16–D18)

    /// A room chosen in the sheet: the next start joins it, and a live
    /// broadcast joins it now.
    func chooseRoom(_ choice: RoomChoice) {
        pendingRoom = choice
        roomCard = nil
        roomStatus = nil
        guard isActive, let broadcaster else { return }
        roomLeaving = false
        room = nil
        roomAttached = false
        switch choice {
        case .join(let code, let key, let token):
            broadcaster.roomJoin(code: code, attachSecret: key, creatorTokenHex: token)
        case .create:
            broadcaster.roomCreate()
        }
    }

    /// The gated room's key (D16): rejoin presenting it.
    func joinWithKey(_ key: String) {
        guard case .join(let code, _, let token) = pendingRoom else { return }
        if let relay { identity.setRoomCredential(key, .attachKey, relay: relay, code: code) }
        chooseRoom(.join(code: code, attachKey: key, creatorToken: token))
        roomStatus = "Joining with the key…"
    }

    /// Leave (D17, D18): the stream leaves the room, and no room is pending.
    func leaveRoom() {
        roomLeaving = true
        pendingRoom = nil
        room = nil
        roomAttached = false
        roomNeedsKey = false
        broadcaster?.roomLeave()
    }

    /// Remove another stream (D18, creator only).
    func removeFromRoom(_ broadcastId: String) {
        broadcaster?.roomRemove(broadcastId: broadcastId)
    }

    /// End room for everyone (D18, creator only).
    func endRoom() {
        roomLeaving = true
        broadcaster?.roomEnd()
    }

    /// The nickname changed while live: the room hears it.
    func setNickname(_ nickname: String) {
        broadcaster?.roomSetNickname(nickname: nickname)
    }

    /// A pending room's X on Ready (D14).
    func clearPendingRoom() {
        pendingRoom = nil
    }

    // MARK: The core's reports

    func apply(_ status: BroadcastStatus) {
        switch status {
        case .ended(let error, let reclaimStatus):
            if let relay, Self.refusesIdentity(reclaimStatus) {
                identity.forgetIdentity(relay: relay)
            }
            let reason = Self.reason(error: error, reclaimStatus: reclaimStatus)
            if liveSince == nil, phase == .connecting, let reason {
                refusal = (reason, reclaimStatus)
            }
            finish(reason: reason)
        // A stopping broadcast only ends: a report the core sent before it
        // saw the stop must not bring the live screen back.
        case _ where phase == .stopping: break
        case .connecting: phase = .connecting
        case .live(let code, let joinLink):
            let first = liveSince == nil
            let now = Date()
            tracker.live(at: now)
            liveSince = tracker.liveSince
            lastCode = code
            phase = .live(code: code, joinLink: joinLink)
            if first { activity?.started(code: code, link: joinLink, since: now) }
            activity?.update(reconnecting: false, viewers: viewerCount)
        case .resuming(let attempt):
            phase = .resuming(attempt: attempt)
            activity?.update(reconnecting: true, viewers: viewerCount)
        }
    }

    /// A report from the core's listener: only the current broadcast's.
    fileprivate func apply(_ status: BroadcastStatus, generation: Int) {
        guard generation == self.generation else { return }
        apply(status)
    }

    private func finish(reason: String?) {
        media.attach(nil)
        broadcaster = nil
        sampler?.cancel()
        sampler = nil
        summary = tracker.summary(at: Date())
        // A pending room survives the end (D14) unless the room went away.
        room = nil
        roomAttached = false
        roomNeedsKey = false
        onEnded?()
        activity?.ended()
        phase = .ended(reason: reason)
    }

    /// Once a second while a broadcast is up: the counters for the stream
    /// rows, and the upload for the summary.
    private func startSampling() {
        sampler?.cancel()
        sampler = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(1))
                guard let self, let b = self.broadcaster else { return }
                let c = b.counters()
                self.counters = c
                if case .live = self.phase, c.uploadAvailable {
                    self.tracker.upload(bps: c.uploadBps)
                }
            }
        }
    }

    func setViewers(_ n: UInt32) {
        viewerCount = n
        tracker.viewers(n)
        activity?.update(reconnecting: phase.isResuming, viewers: n)
    }

    func applyRoom(_ view: RoomView, attached: Bool, needsKey: Bool) {
        guard !roomLeaving else { return }
        // Your rooms hears of a room once per join, not on every change.
        let first = room?.code != view.code
        room = view
        roomAttached = attached
        roomNeedsKey = needsKey
        if attached { roomStatus = nil }
        if first, let relay { onRoomSeen?(view, relay) }
    }

    func applyRoom(_ event: BroadcastRoomEvent) {
        switch event {
        case .created(let code, let token):
            // The creator grant keeps this room ours across restarts.
            if let relay { identity.setRoomCredential(token, .creatorToken, relay: relay, code: code) }
            pendingRoom = .join(code: code, attachKey: "", creatorToken: token)
        case .attached:
            roomAttached = true
            roomNeedsKey = false
            roomStatus = nil
        case .detached(let reason, let byCreator):
            roomAttached = false
            if roomLeaving {
                room = nil
            } else if byCreator {
                let code = room?.code ?? pendingRoom?.code ?? ""
                room = nil
                pendingRoom = nil
                roomCard = Self.removedCard(code: code)
            } else {
                roomStatus = "Your stream isn't in the room: \(reason)."
            }
        case .ended(let reason):
            let code = room?.code ?? pendingRoom?.code ?? ""
            let joined = room != nil
            room = nil
            roomAttached = false
            roomNeedsKey = false
            if roomLeaving {
                roomLeaving = false
                pendingRoom = nil
            } else if !joined {
                roomStatus = "Couldn't join the room: \(reason)."
            } else {
                // A room that ended can't be joined again on the next start.
                pendingRoom = nil
                roomCard = Self.endedCard(code: code, reason: reason)
            }
        case .rejected(let reason, _):
            roomLeaving = false
            roomStatus = "The server refused: \(reason)."
        case .reconnecting(let attempt):
            roomStatus = attempt > 1 ? "Reconnecting to the room… (attempt \(attempt))" : "Reconnecting to the room…"
        }
    }

    fileprivate func setFailure(_ text: String) { failure = text }

    // MARK: Words

    /// A reclaim the relay will never accept: the code expired (404), the
    /// token was refused (403), the slot is taken (409) or an operator ended
    /// it (451). Forgetting it lets the next Start mint a new code. 401 is a
    /// wrong secret and 429 a full server; the identity may still be good.
    static func refusesIdentity(_ reclaimStatus: UInt16?) -> Bool {
        [403, 404, 409, 451].contains(reclaimStatus)
    }

    /// The user-facing sentence for an end (docs/40 statuses, R17).
    static func reason(error: String?, reclaimStatus: UInt16?) -> String? {
        switch reclaimStatus {
        case 401: return "The server refused the publish secret."
        case 403: return "The server wouldn't resume this broadcast; start again for a new code."
        case 404: return "The code expired; start again for a new one."
        case 409: return "This code is live elsewhere; start again for a new one."
        case 429: return "The server is full right now."
        case 451: return "An operator ended this broadcast."
        default: return error
        }
    }

    /// The desktop's card when the room's creator removes our stream
    /// (`shell.rs` `removed_card`, docs/60 D10).
    static func removedCard(code: String) -> RoomCard {
        RoomCard(
            title: code.isEmpty ? "Your stream was removed from the room" : "Your stream was removed from \(code)",
            body: "The room's creator took your stream out of the room. It is still live on its own code, and people watching through it are still watching.")
    }

    /// The desktop's card when someone else ends a room we're in.
    static func endedCard(code: String, reason: String) -> RoomCard {
        var sentence = reason.prefix(1).uppercased() + reason.dropFirst()
        if !sentence.hasSuffix(".") { sentence += "." }
        return RoomCard(
            title: code.isEmpty ? "The room is over" : "Room \(code) is over",
            body: "\(sentence) Your stream is still live on its own code, and people watching through it are still watching.")
    }

    /// The core's callbacks, hopping to the main actor.
    private final class Listener: BroadcastListener, @unchecked Sendable {
        private weak var session: BroadcastSession?
        private let generation: Int
        private let relay: String
        private let identity: IdentityStore

        @MainActor init(session: BroadcastSession, generation: Int, relay: String, identity: IdentityStore) {
            self.session = session
            self.generation = generation
            self.relay = relay
            self.identity = identity
        }

        /// Runs `f` on the main actor if this listener's broadcast is still
        /// the current one.
        private func current(_ f: @escaping @MainActor (BroadcastSession) -> Void) {
            let generation = generation
            Task { @MainActor [weak session] in
                guard let session, session.generation == generation else { return }
                f(session)
            }
        }

        func onStatus(status: BroadcastStatus) {
            let generation = generation
            Task { @MainActor [weak session] in session?.apply(status, generation: generation) }
        }

        func onIdentity(code: String, resumeTokenHex: String) {
            identity.save(relay: relay, code: code, token: resumeTokenHex)
        }

        func onViewerCount(count: UInt32) {
            current { $0.setViewers(count) }
        }

        func onRoomState(room: RoomView, attached: Bool, needsKey: Bool) {
            current { $0.applyRoom(room, attached: attached, needsKey: needsKey) }
        }

        func onRoomEvent(event: BroadcastRoomEvent) {
            current { $0.applyRoom(event) }
        }

        func onFailure(text: String) {
            current { $0.setFailure(text) }
        }
    }
}

extension BroadcastPhase {
    var isResuming: Bool {
        if case .resuming = self { return true }
        return false
    }
}

/// What the Live Activity needs from a broadcast (D15); `LiveActivity`
/// implements it, tests can stand in.
@MainActor
protocol BroadcastActivity: AnyObject {
    func started(code: String, link: String, since: Date)
    func update(reconnecting: Bool, viewers: UInt32)
    func ended()
}
