import AppKit
import BatonCore
import SwiftUI

@main
struct BatonMenuBarApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        // The app has no windows of its own; the status item owns everything.
        // Settings is the smallest scene that satisfies the App protocol.
        Settings { EmptyView() }
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private var monitor: BatonMonitor?
    private var statusItem: StatusItemController?

    func applicationDidFinishLaunching(_ notification: Notification) {
        #if DEBUG
        // --snapshot <dir> renders the popover to PNGs and exits, so the layout
        // can be reviewed without screen recording permission.
        let arguments = CommandLine.arguments
        if let flag = arguments.firstIndex(of: "--snapshot"), flag + 1 < arguments.count {
            exit(Snapshot.run(into: arguments[flag + 1]))
        }
        #endif

        let monitor = BatonMonitor()
        monitor.start()
        self.monitor = monitor
        statusItem = StatusItemController(monitor: monitor)
    }
}
