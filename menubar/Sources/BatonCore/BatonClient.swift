import Foundation

/// One container as reported by `baton status --json`.
///
/// The shape mirrors the Report struct in internal/cli/report.go. That JSON is
/// the contract between the two halves of baton, so this app never reads the
/// state file or talks to Docker itself — everything goes through the CLI and
/// there is exactly one implementation of the rules.
public struct ContainerStatus: Decodable, Identifiable, Sendable {
    public let container: String
    public let running: Bool
    public let holder: Holder?
    public let serving: String?
    public let status: String?
    public let drifted: Bool
    public let queue: [QueueEntry]
    public let error: String?

    public var id: String { container }

    public struct Holder: Decodable, Sendable {
        public let label: String
        public let tree: String
        public let kind: String
        public let heldFor: String
        public let remaining: String?
        public let pinned: Bool
        public let note: String?
    }

    public struct QueueEntry: Decodable, Identifiable, Sendable {
        public let position: Int
        public let label: String
        public let tree: String
        public let waiting: String

        public var id: String { tree }
    }

    /// A short description of what the container is doing, for the detail rows.
    public var servingDescription: String {
        if let error, !error.isEmpty {
            return "unknown"
        }
        if !running {
            return "container stopped"
        }
        guard let serving, !serving.isEmpty else {
            return "no supervisor"
        }
        if drifted {
            return "\(serving) — not the holder's tree"
        }
        return "\(serving) (\(status ?? "?"))"
    }

    /// How the holder should be described in the detail rows.
    public var holderDescription: String {
        guard let holder else { return "free" }
        if holder.pinned {
            let note = holder.note.flatMap { $0.isEmpty ? nil : " — \($0)" } ?? ""
            return "\(holder.label) — held by hand for \(holder.heldFor)\(note)"
        }
        let remaining = holder.remaining.map { ", \($0) left" } ?? ""
        return "\(holder.label) — \(holder.heldFor) in\(remaining)"
    }
}

/// What to draw in the menu bar itself.
public struct MenuSummary: Equatable, Sendable {
    public let symbol: String
    public let text: String

    public init(symbol: String, text: String) {
        self.symbol = symbol
        self.text = text
    }

    /// Condenses every container into the one line there is room for. With more
    /// than one container it reports whichever is held, since a busy container
    /// is the one worth knowing about.
    public static func from(containers: [ContainerStatus], installed: Bool) -> MenuSummary {
        guard installed else {
            return MenuSummary(symbol: "exclamationmark.triangle", text: "baton?")
        }
        guard let focus = containers.first(where: { $0.holder != nil }) ?? containers.first else {
            return MenuSummary(symbol: "circle.dashed", text: "free")
        }

        let waiting = focus.queue.count
        let suffix = waiting > 0 ? " +\(waiting)" : ""

        guard let holder = focus.holder else {
            return MenuSummary(symbol: "circle.dashed", text: "free" + suffix)
        }
        if holder.pinned {
            return MenuSummary(symbol: "hand.raised.fill", text: holder.label + suffix)
        }
        if focus.drifted {
            return MenuSummary(symbol: "exclamationmark.triangle.fill", text: holder.label + suffix)
        }
        return MenuSummary(symbol: "circle.fill", text: holder.label + suffix)
    }
}

/// A failure flattened to a plain message.
///
/// Arbitrary Errors are not Sendable, so results crossing back from a detached
/// task carry this instead. Nothing downstream needs to inspect the cause — the
/// menu only ever displays it.
public struct BatonFailure: Error, Sendable {
    public let message: String
}

extension Result where Failure == BatonFailure {
    /// The error message, or nil when the operation succeeded.
    public var failureMessage: String? {
        if case .failure(let failure) = self { return failure.message }
        return nil
    }
}

/// Runs the baton CLI and decodes its output.
public enum BatonClient {
    /// Where to look for the binary. The installed location comes first so the
    /// app keeps working when it is launched from Finder, which does not
    /// inherit a shell PATH.
    private static let candidatePaths = [
        "/usr/local/bin/baton",
        "/opt/homebrew/bin/baton",
        NSHomeDirectory() + "/newscred/baton/bin/baton",
    ]

    public static var executable: String? {
        candidatePaths.first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    public enum ClientError: LocalizedError {
        case notInstalled
        case failed(String)

        public var errorDescription: String? {
            switch self {
            case .notInstalled:
                return "baton is not installed. Run `make install` in the baton repo."
            case .failed(let message):
                return message
            }
        }
    }

    /// Parses `baton status --json` output. Separated from the process call so
    /// the decoding can be tested against captured output.
    public static func decode(_ output: String) throws -> [ContainerStatus] {
        guard let data = output.data(using: .utf8), !data.isEmpty else {
            return []
        }
        return try JSONDecoder().decode([ContainerStatus].self, from: data)
    }

    /// Fetches the status of every tracked container, off the main thread.
    public static func fetchStatus() async -> Result<[ContainerStatus], BatonFailure> {
        await detached { try decode(try run(["status", "--json"])) }
    }

    /// Takes a container by hand, pinning it until it is dropped. Returns a
    /// message on failure, nil on success.
    public static func performGrab(container: String, note: String) async -> String? {
        await detached { _ = try run(["grab", container, "--note", note]) }.failureMessage
    }

    /// Releases a hand-taken container so the queue can move again.
    public static func performDrop(container: String) async -> String? {
        await detached { _ = try run(["drop", container]) }.failureMessage
    }

    /// Runs a throwing body on a background task and flattens the error to a
    /// message, so nothing non-Sendable has to cross back to the main actor.
    private static func detached<Value: Sendable>(
        _ body: @escaping @Sendable () throws -> Value
    ) async -> Result<Value, BatonFailure> {
        await Task.detached(priority: .userInitiated) {
            do {
                return .success(try body())
            } catch {
                return .failure(BatonFailure(message: error.localizedDescription))
            }
        }.value
    }

    @discardableResult
    private static func run(_ arguments: [String]) throws -> String {
        guard let executable else { throw ClientError.notInstalled }

        let process = Process()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = arguments

        let stdout = Pipe()
        let stderr = Pipe()
        process.standardOutput = stdout
        process.standardError = stderr

        try process.run()
        let outputData = stdout.fileHandleForReading.readDataToEndOfFile()
        let errorData = stderr.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()

        // Exit code 2 means "the answer is no", which is an answer rather than a
        // failure. Everything else non-zero means something actually broke.
        if process.terminationStatus != 0 && process.terminationStatus != 2 {
            let message = String(data: errorData, encoding: .utf8) ?? "baton failed"
            throw ClientError.failed(message.trimmingCharacters(in: .whitespacesAndNewlines))
        }
        return String(data: outputData, encoding: .utf8) ?? ""
    }
}
