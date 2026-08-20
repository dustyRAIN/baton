import AppKit
import BatonCore
import SwiftUI

/// Owns the menu bar item and its popover.
///
/// Written against AppKit rather than SwiftUI's MenuBarExtra, after the
/// `.window` style proved unusable. That style hosts the popover in a window
/// SwiftUI resizes as the content changes, and a macOS window is positioned by
/// its bottom-left corner — so the window grew upward and walked its top off
/// the screen every time a banner appeared or a hold was released. Pinning the
/// height, hiding the slack and re-anchoring the window after each resize all
/// failed to hold it in place.
///
/// NSPopover has no such problem. It is anchored to the status item, and the
/// system keeps it attached and on screen as it resizes. It is also what every
/// other menu bar app of this shape uses.
@MainActor
final class StatusItemController: NSObject {
    private let statusItem: NSStatusItem
    private let monitor: BatonMonitor
    private let presenter: PanelPresenter
    private var summaryObservation: Task<Void, Never>?
    private var resizeWhileOpen: Task<Void, Never>?

    init(monitor: BatonMonitor) {
        self.monitor = monitor
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        presenter = PanelPresenter(content: MenuContent(monitor: monitor))
        super.init()

        statusItem.button?.target = self
        statusItem.button?.action = #selector(toggle)
        render()
        watchSummary()

        #if DEBUG
        // Opens the popover shortly after launch so its placement can be
        // observed from the log without anyone having to click.
        if ProcessInfo.processInfo.environment["BATON_DEBUG_OPEN"] != nil {
            Task { @MainActor in
                try? await Task.sleep(for: .seconds(1))
                self.toggle()
                try? await Task.sleep(for: .seconds(2))
                Diagnostics.log("PANEL placed")
                for screen in NSScreen.screens {
                    Diagnostics.log("SCREEN full=\(screen.frame) visible=\(screen.visibleFrame)")
                }
            }
        }
        #endif
    }

    deinit { summaryObservation?.cancel() }

    @objc private func toggle() {
        if presenter.isVisible {
            presenter.hide()
            resizeWhileOpen?.cancel()
            return
        }
        monitor.refresh()
        presenter.show(under: statusItem.button)
        Diagnostics.log("opened anchor=\(statusItem.button?.window?.frame.debugDescription ?? "none")")
        followContentSize()
    }

    /// Keeps the panel matched to its content while it is open.
    ///
    /// The content changes underneath it — a busy banner appears, a queue
    /// drains — and nothing re-measures on its own.
    private func followContentSize() {
        resizeWhileOpen?.cancel()
        resizeWhileOpen = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .milliseconds(400))
                guard let self, self.presenter.isVisible else { return }
                self.presenter.resizeKeepingTop()
            }
        }
    }

    /// Redraws the menu bar item whenever the summary changes.
    ///
    /// Polling the derived summary rather than observing the monitor keeps this
    /// to one comparison every second; the status item only needs to change
    /// when the words or the symbol do.
    private func watchSummary() {
        summaryObservation = Task { [weak self] in
            var previous: MenuSummary?
            while !Task.isCancelled {
                guard let self else { return }
                let current = self.monitor.summary
                if current != previous {
                    previous = current
                    self.render()
                }
                try? await Task.sleep(for: .seconds(1))
            }
        }
    }

    private func render() {
        guard let button = statusItem.button else { return }
        let summary = monitor.summary
        button.image = NSImage(systemSymbolName: summary.symbol, accessibilityDescription: summary.text)
        button.image?.isTemplate = true
        button.title = " " + summary.text
        button.imagePosition = .imageLeading
        button.toolTip = "baton — \(summary.text)"
    }
}
