import AppKit
import SwiftUI

/// Shows the popover in a panel this code positions itself.
///
/// Neither SwiftUI's MenuBarExtra nor NSPopover could be relied on to place it.
/// MenuBarExtra's `.window` style resizes its window without re-anchoring, so
/// the window grew upward off the top of the screen. NSPopover looked like the
/// fix, but it anchors to the status item, and on this setup the status item
/// reports a position no display contains — measured at (2808, 1153) with the
/// tallest screen ending at 1117. Given an anchor it cannot resolve, NSPopover
/// gave up and put the window at x=0 with its top above the screen.
///
/// So the anchor is treated as a hint, not a fact: the panel is placed under
/// the status item when that position is real, under the menu bar of the main
/// screen when it is not, and always clamped so every edge stays on screen.
@MainActor
final class PanelPresenter {
    private let panel: NSPanel
    private let hosting: NSHostingView<AnyView>
    private var outsideClickMonitor: Any?

    /// Called whenever the panel is hidden, however that happened. Clicking
    /// away closes it without going through the status item, so an owner that
    /// only watched its own toggle would think it was still open.
    var onHide: (() -> Void)?

    var isVisible: Bool { panel.isVisible }

    init<Content: View>(content: Content) {
        hosting = NSHostingView(rootView: AnyView(content))

        panel = NSPanel(
            contentRect: NSRect(x: 0, y: 0, width: Design.popoverWidth, height: 200),
            // Not titled, and non-activating so clicking it does not pull focus
            // away from whatever the user is working in.
            styleMask: [.borderless, .nonactivatingPanel],
            backing: .buffered,
            defer: false)

        panel.contentView = hosting
        panel.isOpaque = false
        panel.backgroundColor = .clear
        panel.hasShadow = true
        panel.level = .popUpMenu
        panel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        panel.hidesOnDeactivate = false
        panel.becomesKeyOnlyIfNeeded = true
        panel.animationBehavior = .utilityWindow
    }

    /// Shows the panel under `anchor`, or somewhere sensible if that is not a
    /// place that exists.
    func show(under anchor: NSStatusBarButton?) {
        let size = fittingSize()
        panel.setContentSize(size)
        let placement = origin(for: size, under: anchor)
        panel.setFrameOrigin(placement)
        panel.orderFrontRegardless()
        watchForOutsideClick()
        Diagnostics.log("PANEL show size=\(size) origin=\(placement) frame=\(panel.frame) "
            + "visible=\(panel.isVisible) screens=\(NSScreen.screens.map(\.frame))")
    }

    func hide() {
        guard panel.isVisible else { return }
        stopWatching()
        panel.orderOut(nil)
        onHide?()
    }

    func toggle(under anchor: NSStatusBarButton?) {
        isVisible ? hide() : show(under: anchor)
    }

    /// Re-measures and keeps the panel's top edge where it is.
    ///
    /// A window is positioned by its bottom-left corner, so resizing alone
    /// would move the top. Holding the top still is what stops the panel
    /// walking off the screen as content appears and disappears.
    func resizeKeepingTop() {
        guard panel.isVisible else { return }
        let size = fittingSize()
        guard size != panel.frame.size else { return }

        let top = panel.frame.maxY
        panel.setContentSize(size)
        var frame = panel.frame
        frame.origin.y = top - frame.height
        panel.setFrame(clamped(frame, to: screen(containing: frame.origin)), display: true)
    }

    private func fittingSize() -> NSSize {
        hosting.layoutSubtreeIfNeeded()
        var size = hosting.fittingSize
        if size.width < 2 { size.width = Design.popoverWidth }
        if size.height < 2 { size.height = 200 }
        return size
    }

    private func origin(for size: NSSize, under anchor: NSStatusBarButton?) -> NSPoint {
        let anchorRect = anchor?.window?.frame
        let host = screen(containing: anchorRect?.origin)

        // Three cases, best first.
        //
        // The anchor is entirely believable: centre under it.
        //
        // The anchor's y is nonsense but its x lands on a real screen — which
        // is what happens here, the item reporting a y above every display.
        // Its horizontal position is still the truth about where the icon
        // appears, so keep the x and hang the panel from that screen's menu
        // bar.
        //
        // Nothing usable at all: top-right of the main screen, where a menu bar
        // item would have been.
        var x: CGFloat
        var y: CGFloat
        if let anchorRect, host.frame.contains(NSPoint(x: anchorRect.midX, y: anchorRect.midY)) {
            x = anchorRect.midX - size.width / 2
            y = anchorRect.minY - size.height - 4
        } else if let anchorRect, let sideways = screenSpanning(x: anchorRect.midX) {
            x = anchorRect.midX - size.width / 2
            y = sideways.visibleFrame.maxY - size.height - 4
            return clamped(NSRect(x: x, y: y, width: size.width, height: size.height),
                           to: sideways).origin
        } else {
            x = host.visibleFrame.maxX - size.width - 12
            y = host.visibleFrame.maxY - size.height - 4
        }

        var frame = NSRect(x: x, y: y, width: size.width, height: size.height)
        frame = clamped(frame, to: host)
        return frame.origin
    }

    /// Pushes a frame back inside a screen's usable area.
    private func clamped(_ frame: NSRect, to screen: NSScreen) -> NSRect {
        var result = frame
        let bounds = screen.visibleFrame

        result.origin.x = min(max(result.minX, bounds.minX + 8), bounds.maxX - result.width - 8)
        // The top matters more than the bottom: clipping the header hides the
        // container name and its status, which is the whole point of opening it.
        if result.maxY > bounds.maxY { result.origin.y = bounds.maxY - result.height }
        if result.minY < bounds.minY { result.origin.y = bounds.minY + 8 }
        return result
    }

    /// The screen whose horizontal range covers x, ignoring y entirely.
    private func screenSpanning(x: CGFloat) -> NSScreen? {
        NSScreen.screens.first { $0.frame.minX <= x && x <= $0.frame.maxX }
    }

    private func screen(containing point: NSPoint?) -> NSScreen {
        if let point, let match = NSScreen.screens.first(where: { $0.frame.contains(point) }) {
            return match
        }
        return NSScreen.main ?? NSScreen.screens[0]
    }

    /// Closes the panel when the user clicks anywhere else, which is what a
    /// transient popover would have done for free.
    private func watchForOutsideClick() {
        stopWatching()
        outsideClickMonitor = NSEvent.addGlobalMonitorForEvents(
            matching: [.leftMouseDown, .rightMouseDown]
        ) { [weak self] _ in
            MainActor.assumeIsolated { self?.hide() }
        }
    }

    private func stopWatching() {
        if let outsideClickMonitor { NSEvent.removeMonitor(outsideClickMonitor) }
        outsideClickMonitor = nil
    }
}
