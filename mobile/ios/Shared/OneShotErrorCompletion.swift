import Foundation

/// Delivers a tunnel-start result at most once, even when cancellation races
/// an asynchronous startup failure.
final class OneShotErrorCompletion: @unchecked Sendable {
    private let lock = NSLock()
    private var handler: ((Error?) -> Void)?

    init(_ handler: @escaping (Error?) -> Void) {
        self.handler = handler
    }

    @discardableResult
    func call(_ error: Error?) -> Bool {
        lock.lock()
        guard let handler else {
            lock.unlock()
            return false
        }
        self.handler = nil
        lock.unlock()
        handler(error)
        return true
    }
}

/// Transfers an Apple lifecycle callback to the provider's serial queue.
final class OneShotVoidCompletion: @unchecked Sendable {
    private let lock = NSLock()
    private var handler: (() -> Void)?

    init(_ handler: @escaping () -> Void) {
        self.handler = handler
    }

    @discardableResult
    func call() -> Bool {
        lock.lock()
        guard let handler else {
            lock.unlock()
            return false
        }
        self.handler = nil
        lock.unlock()
        handler()
        return true
    }
}
