#if DEBUG
import AppKit
import BatonCore
import SwiftUI

/// Renders the popover to PNGs so the design can be looked at.
///
/// A menu bar popover cannot be screenshotted without screen recording
/// permission, and reviewing a layout by reading its source is guesswork.
/// `ImageRenderer` draws the same views offscreen, in both appearances, from
/// fixed sample data — which also makes it a cheap regression check that no
/// state renders blank or clipped.
@MainActor
enum Snapshot {

    static func run(into directory: String) -> Int32 {
        let cases: [(String, [ContainerStatus])] = [
            ("01-free", [Samples.free]),
            ("02-held", [Samples.heldWithQueue]),
            ("03-drifted", [Samples.drifted]),
            ("04-pinned", [Samples.pinned]),
            ("05-notes", [Samples.withNotes]),
            ("06-broken", [Samples.broken]),
            ("07-empty", []),
            ("08-two-containers", [Samples.heldWithQueue, Samples.secondFree]),
        ]

        let root = URL(fileURLWithPath: directory)
        try? FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)

        for (name, containers) in cases {
            for scheme in [ColorScheme.light, .dark] {
                let monitor = BatonMonitor(preview: containers)
                let view = MenuContent(monitor: monitor)
                    .environment(\.colorScheme, scheme)
                    .background(scheme == .dark ? Color(white: 0.13) : Color(white: 0.96))

                let renderer = ImageRenderer(content: view)
                renderer.scale = 2

                guard let image = renderer.nsImage,
                      let data = image.tiffRepresentation,
                      let bitmap = NSBitmapImageRep(data: data),
                      let png = bitmap.representation(using: .png, properties: [:])
                else {
                    print("failed to render \(name)")
                    return 1
                }
                let suffix = scheme == .dark ? "dark" : "light"
                try? png.write(to: root.appendingPathComponent("\(name)-\(suffix).png"))
            }
        }
        print("wrote snapshots to \(directory)")
        return 0
    }
}

/// Fixed data for the snapshots, one per state worth looking at.
enum Samples {
    static func holder(
        label: String, kind: String = "session", heldFor: String = "4m12s",
        remaining: String? = "15m48s", pinned: Bool = false, note: String? = nil,
        heldSeconds: Int = 252, remainingSeconds: Int = 948
    ) -> ContainerStatus.Holder {
        ContainerStatus.Holder(
            label: label, tree: "/repo/.worktrees/\(label)", kind: kind,
            heldFor: heldFor, remaining: remaining, pinned: pinned, note: note,
            heldForSeconds: heldSeconds, remainingSeconds: remainingSeconds)
    }

    static let free = ContainerStatus(
        container: "web", running: true, holder: nil, serving: "main",
        status: "ready", drifted: false, queue: [], notes: nil, error: nil, health: "free")

    static let heldWithQueue = ContainerStatus(
        container: "web", running: true,
        holder: holder(label: "pr-4821-review"),
        serving: "pr-4821", status: "ready", drifted: false,
        queue: [
            .init(position: 1, label: "feature-search", tree: "/a", waiting: "2m10s"),
            .init(position: 2, label: "fix-login-redirect", tree: "/b", waiting: "30s"),
        ],
        notes: nil, error: nil, health: "held")

    static let drifted = ContainerStatus(
        container: "web", running: true,
        holder: holder(label: "pr-4821-review", heldFor: "18m02s", remaining: "1m58s",
                       heldSeconds: 1082, remainingSeconds: 118),
        serving: "main", status: "ready", drifted: true,
        queue: [.init(position: 1, label: "feature-search", tree: "/a", waiting: "5m")],
        notes: nil, error: nil, health: "drifted")

    static let pinned = ContainerStatus(
        container: "web", running: true,
        holder: holder(label: "main", kind: "human", heldFor: "3m00s", remaining: nil,
                       pinned: true, note: "debugging the checkout flow",
                       heldSeconds: 180, remainingSeconds: 0),
        serving: "main", status: "ready", drifted: false,
        queue: [
            .init(position: 1, label: "pr-4821-review", tree: "/a", waiting: "4m"),
            .init(position: 2, label: "feature-search", tree: "/b", waiting: "2m"),
        ],
        notes: nil, error: nil, health: "pinned")

    static let withNotes = ContainerStatus(
        container: "api", running: true,
        holder: holder(label: "add-webhooks"),
        serving: "add-webhooks", status: "ready", drifted: false, queue: [],
        notes: [
            .init(level: "info", text: "applied migrations 4f2a1c → 9b3d07. Other worktrees share this database."),
            .init(level: "warning", text: "database is at 9b3d07 but this tree expects 4f2a1c — the schema is ahead of the branch. Migration-dependent results are not trustworthy."),
        ],
        error: nil, health: "held")

    /// A second container, deliberately named differently: identical ids would
    /// collapse in ForEach and the snapshot would silently show one twice.
    static let secondFree = ContainerStatus(
        container: "api", running: true, holder: nil, serving: "main",
        status: "ready", drifted: false, queue: [], notes: nil, error: nil, health: "free")

    static let broken = ContainerStatus(
        container: "web", running: false, holder: nil, serving: nil, status: nil,
        drifted: false, queue: [], notes: nil,
        error: "docker inspect web: exit status 1 (is the container created?)",
        health: "broken")
}
#endif
