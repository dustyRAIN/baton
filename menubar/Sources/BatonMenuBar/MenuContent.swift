import BatonCore
import SwiftUI

/// The popover.
///
/// Ordered by what someone opening this actually wants to know: who has it, is
/// the container really on their branch, who is waiting, what can I do. Anything
/// that only matters when something is wrong stays hidden until it is.
struct MenuContent: View {
    let monitor: BatonMonitor

    var body: some View {
        // Enough containers, queued sessions and notes will eventually outgrow
        // any screen. Capping the height and scrolling keeps the window on
        // screen rather than letting it place itself off the top.
        ScrollView(.vertical) {
            MenuBody(monitor: monitor)
        }
        .frame(width: Design.popoverWidth)
        .frame(maxHeight: Design.maxPopoverHeight)
        .scrollBounceBehavior(.basedOnSize)
    }
}

/// The popover's contents, without the scrolling wrapper.
///
/// Separate because ImageRenderer lays a ScrollView out but does not rasterise
/// what is inside it — snapshots of the whole popover come out blank. The
/// snapshots render this directly, which also keeps them honest about the
/// content rather than the container.
struct MenuBody: View {
    let monitor: BatonMonitor

    var body: some View {
        VStack(alignment: .leading, spacing: Design.sectionSpacing) {
            if let message = monitor.busyMessage {
                BusyBanner(message: message)
            }

            if let error = monitor.lastError {
                Banner(symbol: "exclamationmark.triangle.fill", tint: .orange, text: error)
            }

            if monitor.containers.isEmpty && monitor.lastError == nil {
                EmptyState()
            }

            ForEach(Array(monitor.containers.enumerated()), id: \.element.id) { index, container in
                VStack(alignment: .leading, spacing: Design.sectionSpacing) {
                    if index > 0 {
                        Divider()
                    }
                    ContainerCard(container: container, monitor: monitor)
                }
            }

            Divider()
            Footer(monitor: monitor)
        }
        .padding(14)
        .frame(width: Design.popoverWidth)
        .fixedSize(horizontal: false, vertical: true)
    }
}

private struct EmptyState: View {
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Nothing tracked yet")
                .font(.system(size: 12, weight: .medium))
            Text("Run `baton init <container>` to get started.")
                .font(.system(size: 11))
                .foregroundStyle(.secondary)
        }
        .padding(.vertical, 4)
    }
}

struct ContainerCard: View {
    let container: ContainerStatus
    let monitor: BatonMonitor

    private var health: Health { container.healthState }

    var body: some View {
        VStack(alignment: .leading, spacing: 11) {
            header

            if health.warrantsBanner {
                Banner(symbol: health.symbol, tint: health.tint, text: troubleText)
            }

            StateCard(container: container)

            ForEach(container.notes ?? []) { note in
                Banner(symbol: note.isWarning ? "exclamationmark.triangle.fill" : "info.circle.fill",
                       tint: note.isWarning ? .orange : .blue,
                       text: note.text)
            }

            if !container.queue.isEmpty {
                QueueList(entries: container.queue, frozen: health == .pinned)
            }

            actions
                .padding(.top, 2)
        }
    }

    private var header: some View {
        HStack(spacing: 7) {
            Image(systemName: health.symbol)
                .font(.system(size: 9))
                .foregroundStyle(health.tint)
            Text(container.container)
                .font(.system(size: 13, weight: .semibold))
            Spacer(minLength: 8)
            Text(health.word)
                .font(.system(size: 10, weight: .medium))
                .foregroundStyle(health.tint)
                .padding(.horizontal, 6)
                .padding(.vertical, 2)
                .background(health.tint.opacity(0.14), in: Capsule())
        }
    }

    /// Spelled out rather than labelled, because these two states change whether
    /// the user should believe a test result.
    private var troubleText: String {
        switch health {
        case .drifted:
            let holder = container.holder?.label ?? "someone"
            let serving = container.serving ?? "something else"
            return "\(holder) holds it, but the container is serving \(serving). Results would be against the wrong branch."
        case .broken:
            return container.error.flatMap { $0.isEmpty ? nil : $0 }
                ?? "The container is not running."
        default:
            return ""
        }
    }

    private var actions: some View {
        HStack(spacing: 8) {
            if health == .pinned {
                Button("Release") { monitor.drop(container.container) }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.small)
            } else {
                // "Take over" only makes sense when there is somebody to take
                // it from. Both call grab; the wording follows the situation.
                Button(container.holder == nil ? "Reserve" : "Take over") {
                    monitor.grab(container.container)
                }
                .buttonStyle(.bordered)
                .controlSize(.small)
                .disabled(!container.running)
            }
            Spacer(minLength: 0)
        }
        .disabled(monitor.busy)
    }
}

/// Shown while a grab or drop is running.
///
/// Taking over means switching the container to another worktree, which is
/// tens of seconds of real work — installing dependencies and restarting the
/// app. Saying so, with a number, is the difference between waiting and
/// wondering whether it has hung.
struct BusyBanner: View {
    let message: String

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            ProgressView()
                .controlSize(.small)
                .scaleEffect(0.6)
                .frame(width: 12, height: 12)
            VStack(alignment: .leading, spacing: 2) {
                Text(message + "…")
                    .font(.system(size: 11, weight: .medium))
                Text("Switching the container usually takes about 30 seconds, "
                     + "or up to a minute the first time a branch is used.")
                    .font(.system(size: 10))
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Spacer(minLength: 0)
        }
        .padding(.vertical, 8)
        .padding(.horizontal, 9)
        .background(Color.accentColor.opacity(0.12), in: RoundedRectangle(cornerRadius: Design.corner))
    }
}

/// Who holds it, and what the container is actually serving.
///
/// These two live in one card on purpose. Their disagreement is the failure the
/// whole tool exists to catch, and it is far easier to notice two adjacent lines
/// contradicting each other than two facts separated by other content.
struct StateCard: View {
    let container: ContainerStatus

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let holder = container.holder {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(holder.label)
                        .font(.system(size: 12, weight: .medium, design: .monospaced))
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Spacer(minLength: 8)
                    Text(remainingText(holder))
                        .font(.system(size: 11, weight: .medium))
                        .monospacedDigit()
                        .foregroundStyle(container.leaseRunningOut ? Color.orange : .secondary)
                }

                if let elapsed = container.leaseElapsed {
                    LeaseBar(elapsed: elapsed, runningOut: container.leaseRunningOut)
                }

                if let note = holder.note, !note.isEmpty {
                    Text(note)
                        .font(.system(size: 11))
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }

                Divider().opacity(0.5)
            }

            HStack(spacing: 6) {
                Text("serving")
                    .font(.system(size: 10, weight: .medium))
                    .foregroundStyle(.tertiary)
                Spacer(minLength: 8)
                if container.drifted {
                    Image(systemName: "arrow.triangle.branch")
                        .font(.system(size: 9, weight: .semibold))
                        .foregroundStyle(.orange)
                }
                Text(servingText)
                    .font(.system(size: 11, weight: .medium, design: .monospaced))
                    .foregroundStyle(container.drifted ? Color.orange : .primary)
                    .lineLimit(1)
                    .truncationMode(.head)
            }
        }
        .padding(10)
        .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: Design.corner))
    }

    private func remainingText(_ holder: ContainerStatus.Holder) -> String {
        if holder.pinned { return "no expiry" }
        guard let remaining = holder.remaining else { return holder.heldFor }
        return "\(remaining) left"
    }

    private var servingText: String {
        guard container.running else { return "container stopped" }
        guard let serving = container.serving, !serving.isEmpty else { return "no supervisor" }
        if let status = container.status, status != "ready" { return "\(serving) · \(status)" }
        return serving
    }
}

struct QueueList: View {
    let entries: [ContainerStatus.QueueEntry]
    var frozen = false

    var body: some View {
        VStack(alignment: .leading, spacing: Design.rowSpacing) {
            SectionLabel(text: frozen ? "Waiting · paused" : "Waiting",
                         trailing: "\(entries.count)")
            ForEach(entries) { entry in
                HStack(spacing: 8) {
                    PositionBadge(position: entry.position)
                    Text(entry.label)
                        .font(.system(size: 11, design: .monospaced))
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Spacer(minLength: 8)
                    Text(entry.waiting)
                        .font(.system(size: 10))
                        .monospacedDigit()
                        .foregroundStyle(.tertiary)
                }
            }
        }
    }
}

struct Footer: View {
    let monitor: BatonMonitor

    var body: some View {
        HStack(spacing: 12) {
            FooterButton(symbol: "arrow.clockwise", title: "Refresh") { monitor.refresh() }
            Spacer(minLength: 0)
            FooterButton(symbol: "power", title: "Quit") { NSApplication.shared.terminate(nil) }
        }
    }
}

/// A quiet text button that lights up on hover, like the rest of the system's
/// secondary actions.
struct FooterButton: View {
    let symbol: String
    let title: String
    let action: () -> Void

    @State private var hovering = false

    var body: some View {
        Button(action: action) {
            HStack(spacing: 4) {
                Image(systemName: symbol).font(.system(size: 9, weight: .medium))
                Text(title).font(.system(size: 11))
            }
            .foregroundStyle(hovering ? AnyShapeStyle(.primary) : AnyShapeStyle(.secondary))
            .padding(.horizontal, 6)
            .padding(.vertical, 3)
            .background(
                RoundedRectangle(cornerRadius: 5)
                    .fill(hovering ? AnyShapeStyle(.quaternary) : AnyShapeStyle(.clear))
            )
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
    }
}
