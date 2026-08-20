import Foundation

/// Appends a line to ~/.baton/menubar.log.
///
/// The popover cannot be screenshotted and printouts from a background agent
/// go nowhere visible, so this is how its behaviour gets observed rather than
/// guessed at.
enum Diagnostics {
    private static let path: URL = {
        let home = FileManager.default.homeDirectoryForCurrentUser
        let directory = home.appendingPathComponent(".baton")
        try? FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        return directory.appendingPathComponent("menubar.log")
    }()

    /// Past this the log is started fresh. It exists to diagnose the last few
    /// times the panel was opened, not to accumulate for months.
    private static let sizeLimit = 64 * 1024

    static func log(_ message: String) {
        let stamp = ISO8601DateFormatter().string(from: Date())
        let line = "\(stamp) \(message)\n"
        guard let data = line.data(using: .utf8) else { return }

        let size = (try? FileManager.default.attributesOfItem(atPath: path.path)[.size] as? Int) ?? 0
        if (size ?? 0) > sizeLimit {
            try? FileManager.default.removeItem(at: path)
        }

        if let handle = try? FileHandle(forWritingTo: path) {
            defer { try? handle.close() }
            _ = try? handle.seekToEnd()
            try? handle.write(contentsOf: data)
        } else {
            try? data.write(to: path)
        }
    }
}
