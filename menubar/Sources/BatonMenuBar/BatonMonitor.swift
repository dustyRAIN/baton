import BatonCore
import Foundation
import Observation

/// Polls the baton CLI and publishes what it finds.
///
/// Polling rather than watching: the state file changes rarely, a poll costs one
/// short-lived process, and it means the app needs no privileged access to
/// anything. Two seconds is fast enough that the menu bar never looks stale
/// during a handoff, which takes about half a minute.
///
/// The class is main-actor isolated so SwiftUI can read it directly. Every call
/// into the CLI is pushed onto a detached task, because `baton status` shells
/// out to Docker and a grab waits for the container to switch trees — neither
/// belongs on the main thread.
@MainActor
@Observable
final class BatonMonitor {
    private(set) var containers: [ContainerStatus] = []
    private(set) var lastError: String?
    private(set) var installed = BatonClient.executable != nil

    private var timer: Timer?
    private var busy = false
    private let interval: TimeInterval = 2

    func start() {
        refresh()
        timer = Timer.scheduledTimer(withTimeInterval: interval, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.refresh() }
        }
    }

    func stop() {
        timer?.invalidate()
        timer = nil
    }

    func refresh() {
        installed = BatonClient.executable != nil
        guard installed else {
            containers = []
            lastError = BatonClient.ClientError.notInstalled.errorDescription
            return
        }
        // A grab can outlast several poll ticks. Skipping while one is in flight
        // keeps the menu from flickering between stale and fresh readings.
        guard !busy else { return }

        Task {
            switch await BatonClient.fetchStatus() {
            case .success(let fetched):
                containers = fetched
                lastError = nil
            case .failure(let failure):
                lastError = failure.message
            }
        }
    }

    func grab(_ container: String) {
        perform { await BatonClient.performGrab(container: container, note: "taken from the menu bar") }
    }

    func drop(_ container: String) {
        perform { await BatonClient.performDrop(container: container) }
    }

    private func perform(_ action: @escaping @Sendable () async -> String?) {
        busy = true
        Task {
            let failure = await action()
            busy = false
            if let failure { lastError = failure }
            refresh()
        }
    }

    /// The single line shown in the menu bar itself.
    var summary: MenuSummary {
        MenuSummary.from(containers: containers, installed: installed)
    }
}
