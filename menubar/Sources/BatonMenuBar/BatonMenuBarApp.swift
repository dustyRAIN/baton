import BatonCore
import SwiftUI

@main
struct BatonMenuBarApp: App {
    @State private var monitor = BatonMonitor()

    var body: some Scene {
        MenuBarExtra {
            MenuContent(monitor: monitor)
        } label: {
            let summary = monitor.summary
            // Composing the icon and text into one Image keeps the menu bar item
            // to a single view, which is all MenuBarExtra renders reliably.
            HStack(spacing: 4) {
                Image(systemName: summary.symbol)
                Text(summary.text)
            }
        }
        .menuBarExtraStyle(.window)
    }

    init() {
        monitor.start()
    }
}

struct MenuContent: View {
    let monitor: BatonMonitor

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if let error = monitor.lastError {
                Label(error, systemImage: "exclamationmark.triangle")
                    .font(.callout)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }

            if monitor.containers.isEmpty && monitor.lastError == nil {
                Text("Nothing is being tracked yet.")
                    .foregroundStyle(.secondary)
            }

            ForEach(monitor.containers) { container in
                ContainerSection(container: container, monitor: monitor)
            }

            Divider()

            HStack {
                Button("Refresh") { monitor.refresh() }
                Spacer()
                Button("Quit") { NSApplication.shared.terminate(nil) }
            }
            .buttonStyle(.borderless)
            .font(.callout)
        }
        .padding(14)
        .frame(width: 340)
    }
}

struct ContainerSection: View {
    let container: ContainerStatus
    let monitor: BatonMonitor

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(container.container)
                .font(.headline)

            row("holder", container.holderDescription, emphasised: container.holder?.pinned == true)
            row("serving", container.servingDescription, emphasised: container.drifted)

            if container.queue.isEmpty {
                row("queue", "empty")
            } else {
                ForEach(container.queue) { entry in
                    row(entry.position == 1 ? "queue" : "",
                        "\(entry.position). \(entry.label) — waiting \(entry.waiting)")
                }
            }

            HStack {
                if container.holder?.pinned == true {
                    Button("Release") { monitor.drop(container.container) }
                } else {
                    Button("Take over") { monitor.grab(container.container) }
                }
            }
            .buttonStyle(.bordered)
            .controlSize(.small)
            .padding(.top, 2)
        }
    }

    private func row(_ label: String, _ value: String, emphasised: Bool = false) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(label)
                .font(.system(.caption, design: .monospaced))
                .foregroundStyle(.secondary)
                .frame(width: 52, alignment: .leading)
            Text(value)
                .font(.callout)
                .foregroundStyle(emphasised ? .orange : .primary)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
        }
    }
}
