import Foundation
import Network

/// docs/67 D19: tells the broadcaster when the network path changes (Wi-Fi
/// ↔ cellular), so it reclaims at once instead of waiting out idle
/// timeouts (QUIC migration is off, D12), and whether the path is
/// expensive, which picks the Cellular rung at start (never mid-broadcast).
final class PathMonitor: @unchecked Sendable {
    private let monitor = NWPathMonitor()
    private let queue = DispatchQueue(label: "gawk.path")
    private let lock = NSLock()
    private var last: NWPath?
    private var onChange: (@Sendable () -> Void)?

    func start(onChange: @escaping @Sendable () -> Void) {
        lock.withLock { self.onChange = onChange }
        monitor.pathUpdateHandler = { [weak self] path in self?.update(path) }
        monitor.start(queue: queue)
    }

    func stop() {
        monitor.cancel()
    }

    /// Whether the current path is expensive (cellular or a hotspot).
    var isExpensive: Bool {
        lock.withLock { last?.isExpensive ?? false }
    }

    private func update(_ path: NWPath) {
        let (changed, callback): (Bool, (@Sendable () -> Void)?) = lock.withLock {
            defer { last = path }
            guard let prev = last else { return (false, nil) }
            let moved = prev.status != path.status
                || prev.usesInterfaceType(.wifi) != path.usesInterfaceType(.wifi)
                || prev.usesInterfaceType(.cellular) != path.usesInterfaceType(.cellular)
            return (moved && path.status == .satisfied, onChange)
        }
        if changed { callback?() }
    }
}
