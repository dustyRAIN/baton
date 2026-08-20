import BatonCore
import SwiftUI

@main
struct BatonMenuBarApp: App {
    @State private var monitor = BatonMonitor()

    init() {
        #if DEBUG
        // --snapshot <dir> renders the popover to PNGs and exits, so the layout
        // can be reviewed without screen recording permission.
        let arguments = CommandLine.arguments
        if let flag = arguments.firstIndex(of: "--snapshot"), flag + 1 < arguments.count {
            exit(MainActor.assumeIsolated { Snapshot.run(into: arguments[flag + 1]) })
        }
        #endif
        monitor.start()
    }

    var body: some Scene {
        // The title-and-symbol initialiser rather than a custom label view:
        // MenuBarExtra only reliably renders Text or Image in the bar itself,
        // and a composed view can silently come out blank.
        MenuBarExtra(monitor.summary.text, systemImage: monitor.summary.symbol) {
            MenuContent(monitor: monitor)
        }
        .menuBarExtraStyle(.window)
    }
}
