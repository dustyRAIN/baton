import BatonCore
import Foundation

// Two modes:
//
//   baton-probe              print exactly what the menu bar would render, using
//                            the same code path, for when the app looks wrong
//   baton-probe --selftest   check the decoding against captured CLI output

if CommandLine.arguments.contains("--selftest") {
    exit(Selftest.run())
}

guard let executable = BatonClient.executable else {
    print("baton binary: NOT FOUND")
    print("looked in /usr/local/bin, /opt/homebrew/bin and ~/.local/bin")
    exit(1)
}
print("baton binary: \(executable)")

switch await BatonClient.fetchStatus() {
case .failure(let failure):
    print("status failed: \(failure.message)")
    exit(1)

case .success(let containers):
    let summary = MenuSummary.from(containers: containers, installed: true)
    print("menu bar:     [\(summary.symbol)] \(summary.text)")
    print("")

    if containers.isEmpty {
        print("(nothing tracked yet)")
    }
    for container in containers {
        print(container.container)
        print("  holder    \(container.holderDescription)")
        print("  serving   \(container.servingDescription)")
        if container.queue.isEmpty {
            print("  queue     empty")
        }
        for entry in container.queue {
            print("  queue     \(entry.position). \(entry.label) — waiting \(entry.waiting)")
        }
    }
}
