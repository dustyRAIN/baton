import BatonCore
import SwiftUI

/// The visual language, in one place.
///
/// Everything is derived from system semantic colours rather than fixed values,
/// so the popover follows the user's appearance, accent colour and contrast
/// settings instead of fighting them. A menu bar utility that ignores the system
/// look reads as a foreign object.
enum Design {
    static let popoverWidth: CGFloat = 320

    /// Beyond this the popover scrolls. Chosen to stay comfortably inside the
    /// working area of the smallest laptop display Apple still ships.
    static let maxPopoverHeight: CGFloat = 520
    static let corner: CGFloat = 8
    static let rowSpacing: CGFloat = 6
    static let sectionSpacing: CGFloat = 14
}

/// How a container is doing, in one value.
///
/// Mirrors the `health` field the CLI emits, so the CLI and the UI cannot drift
/// apart on what counts as trouble. An unrecognised value degrades to `.free`
/// rather than crashing, which keeps an older app usable against a newer CLI.
enum Health: String {
    case free, held, pinned, drifted, starting, broken

    init(_ raw: String?) {
        self = Health(rawValue: raw ?? "") ?? .free
    }

    /// The status colour. Only three carry meaning — everything else is grey —
    /// because a palette where every state is coloured says nothing.
    var tint: Color {
        switch self {
        case .drifted, .broken: return .orange
        case .pinned: return .purple
        case .held: return .accentColor
        case .starting: return .secondary
        case .free: return .secondary
        }
    }

    var symbol: String {
        switch self {
        case .free: return "circle.dashed"
        case .held: return "circle.fill"
        case .starting: return "circle.dotted"
        case .pinned: return "hand.raised.fill"
        case .drifted: return "exclamationmark.triangle.fill"
        case .broken: return "xmark.octagon.fill"
        }
    }

    /// Shown in the header beside the container name.
    var word: String {
        switch self {
        case .free: return "free"
        case .held: return "in use"
        case .starting: return "starting"
        case .pinned: return "held by hand"
        case .drifted: return "out of sync"
        case .broken: return "unavailable"
        }
    }

    /// Whether this state needs explaining rather than just labelling.
    var warrantsBanner: Bool { self == .drifted || self == .broken }
}

extension ContainerStatus {
    var healthState: Health { Health(health) }

    /// How much of the lease is gone, 0 to 1. Nil when the hold cannot expire,
    /// which is how a hand-taken container is distinguished from one that is
    /// simply early in its lease.
    var leaseElapsed: Double? {
        guard let holder, !holder.pinned else { return nil }
        let total = holder.heldForSeconds + holder.remainingSeconds
        guard total > 0 else { return nil }
        return min(1, max(0, Double(holder.heldForSeconds) / Double(total)))
    }

    /// True when the lease is nearly up, so the bar can warn before it lapses.
    var leaseRunningOut: Bool {
        guard let holder, !holder.pinned else { return false }
        return holder.remainingSeconds > 0 && holder.remainingSeconds < 120
    }
}

/// A small caps section heading.
struct SectionLabel: View {
    let text: String
    var trailing: String?

    var body: some View {
        HStack(spacing: 6) {
            Text(text.uppercased())
                .font(.system(size: 10, weight: .semibold))
                .tracking(0.6)
                .foregroundStyle(.tertiary)
            if let trailing {
                Text(trailing)
                    .font(.system(size: 10, weight: .medium))
                    .foregroundStyle(.quaternary)
            }
            Spacer(minLength: 0)
        }
    }
}

/// A filled capsule holding a queue position.
struct PositionBadge: View {
    let position: Int

    var body: some View {
        Text("\(position)")
            .font(.system(size: 10, weight: .semibold, design: .rounded))
            .monospacedDigit()
            .foregroundStyle(.secondary)
            .frame(width: 16, height: 16)
            .background(.quaternary, in: Circle())
    }
}

/// How much of a hold is left, as a bar.
///
/// A duration in words tells you the number; the bar tells you the shape of it
/// without reading. It turns orange near the end, which is the only moment the
/// number actually demands attention.
///
/// Drawn with a scaled capsule rather than a GeometryReader over two shapes.
/// A menu bar window sizes itself to its content, and GeometryReader reports
/// the size it is *offered* — inside a self-sizing window that risks a layout
/// feedback loop. scaleEffect needs no measurement, so the bar cannot influence
/// the height of the window that contains it.
struct LeaseBar: View {
    let elapsed: Double
    let runningOut: Bool

    private var remaining: Double { max(0, min(1, 1 - elapsed)) }

    var body: some View {
        Capsule()
            .fill(.quaternary)
            .frame(height: 3)
            .overlay(alignment: .leading) {
                Capsule()
                    .fill(runningOut ? Color.orange : Color.accentColor)
                    .scaleEffect(x: remaining, anchor: .leading)
            }
            .accessibilityLabel("Time remaining on this hold")
            .accessibilityValue("\(Int(remaining * 100)) percent")
    }
}

/// An explanation that needs to be read, not skimmed.
struct Banner: View {
    let symbol: String
    let tint: Color
    let text: String

    var body: some View {
        HStack(alignment: .top, spacing: 7) {
            Image(systemName: symbol)
                .font(.system(size: 11, weight: .semibold))
                .foregroundStyle(tint)
            Text(text)
                .font(.system(size: 11))
                .foregroundStyle(.primary)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
        }
        .padding(.vertical, 7)
        .padding(.horizontal, 9)
        .background(tint.opacity(0.12), in: RoundedRectangle(cornerRadius: Design.corner))
    }
}
