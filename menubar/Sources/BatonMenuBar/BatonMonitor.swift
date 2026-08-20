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
    /// True while a grab or drop is in flight, so the UI can show it.
    private(set) var busy = false

    /// What that work is, in words. A bare spinner on an action that can take
    /// half a minute reads as a hang.
    private(set) var busyMessage: String?
    private let interval: TimeInterval = 2

    init() {}

    /// A monitor with fixed contents and no polling, for rendering snapshots.
    init(preview containers: [ContainerStatus], busy busyMessage: String? = nil) {
        self.containers = containers
        self.installed = true
        self.busy = busyMessage != nil
        self.busyMessage = busyMessage
    }

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

    /// Worktrees each container can be handed to, refreshed when the menu opens
    /// rather than on the poll — the list changes rarely and costs a git call.
    private(set) var worktrees: [String: [WorktreeOption]] = [:]

    func loadWorktrees(for container: String) {
        Task {
            let found = await BatonClient.fetchTrees(container: container)
            if !found.isEmpty { worktrees[container] = found }
        }
    }

    /// Takes the container by hand. With a worktree, pins it to that one, which
    /// is how you get a specific branch in front of you to test yourself.
    func grab(_ container: String, worktree: WorktreeOption? = nil) {
        let target = worktree.map { " on \($0.label)" } ?? ""
        perform("Taking over \(container)\(target)") {
            await BatonClient.performGrab(container: container,
                                          worktree: worktree?.label,
                                          note: "taken from the menu bar")
        }
    }

    func drop(_ container: String) {
        perform("Releasing \(container)") {
            await BatonClient.performDrop(container: container)
        }
    }

    private func perform(_ message: String, _ action: @escaping @Sendable () async -> String?) {
        busy = true
        busyMessage = message
        Task {
            let failure = await action()
            busy = false
            busyMessage = nil
            if let failure { lastError = failure }
            refresh()
        }
    }

    /// The single line shown in the menu bar itself.
    var summary: MenuSummary {
        MenuSummary.from(containers: containers, installed: installed)
    }
}
