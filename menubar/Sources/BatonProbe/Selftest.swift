import BatonCore
import Foundation

/// Checks the decoding and the menu bar line against captured `baton status
/// --json` output.
///
/// This is a hand-rolled runner rather than XCTest because the package has to
/// build on a machine with Command Line Tools and no Xcode, where SPM cannot
/// see XCTest. The JSON is real CLI output kept verbatim, so if the Go side
/// changes shape these fail loudly instead of the menu bar going quietly blank.
enum Selftest {

    static func run() -> Int32 {
        var failures: [String] = []

        func check<Value: Equatable>(_ label: String, _ actual: Value, _ expected: Value) {
            if actual != expected {
                failures.append("\(label): got \(actual), want \(expected)")
            }
        }

        func checkTrue(_ label: String, _ condition: Bool, _ detail: @autoclosure () -> String = "") {
            if !condition {
                failures.append("\(label): \(detail())")
            }
        }

        do {
            // A held container with two sessions waiting behind it.
            let held = try BatonClient.decode(Fixtures.heldWithQueue)
            check("held count", held.count, 1)
            if let container = held.first {
                check("held label", container.holder?.label ?? "", "pr-4821-review")
                check("held remaining", container.holder?.remaining ?? "", "15m48s")
                check("queue labels", container.queue.map(\.label).joined(separator: ","),
                      "feature-search,fix-login")
            }

            let heldSummary = MenuSummary.from(containers: held, installed: true)
            check("held summary text", heldSummary.text, "pr-4821-review +2")
            check("held summary symbol", heldSummary.symbol, "circle.fill")

            // Empty output must decode to nothing rather than throw.
            checkTrue("empty decode", try BatonClient.decode("").isEmpty, "expected no containers")

            // Free.
            let freeSummary = MenuSummary.from(containers: try BatonClient.decode(Fixtures.free),
                                               installed: true)
            check("free summary text", freeSummary.text, "free")
            check("free summary symbol", freeSummary.symbol, "circle.dashed")

            // Pinned by hand.
            let pinned = try BatonClient.decode(Fixtures.pinnedByHand)
            let pinnedSummary = MenuSummary.from(containers: pinned, installed: true)
            check("pinned symbol", pinnedSummary.symbol, "hand.raised.fill")
            check("pinned text", pinnedSummary.text, "main")
            if let container = pinned.first {
                checkTrue("pinned holder text", container.holderDescription.contains("held by hand"),
                          container.holderDescription)
                checkTrue("pinned note", container.holderDescription.contains("debugging by hand"),
                          container.holderDescription)
            }

            // Drift: holding the lock while the container serves someone else is
            // the failure the whole tool exists to prevent, so it has to be
            // visible in the menu bar without opening the menu.
            let drifted = try BatonClient.decode(Fixtures.drifted)
            let driftSummary = MenuSummary.from(containers: drifted, installed: true)
            check("drift symbol", driftSummary.symbol, "exclamationmark.triangle.fill")
            if let container = drifted.first {
                checkTrue("drift wording",
                          container.servingDescription.contains("not the holder's tree"),
                          container.servingDescription)
            }

            // Docker unreachable: still renders, does not crash.
            let down = try BatonClient.decode(Fixtures.dockerDown)
            if let container = down.first {
                check("docker down serving", container.servingDescription, "unknown")
                check("docker down holder", container.holderDescription, "free")
            }

            // Missing binary.
            check("missing binary", MenuSummary.from(containers: [], installed: false).text, "baton?")

        } catch {
            failures.append("threw: \(error.localizedDescription)")
        }

        if failures.isEmpty {
            print("selftest: all checks passed")
            return 0
        }
        print("selftest: \(failures.count) failure(s)")
        for failure in failures {
            print("  - \(failure)")
        }
        return 1
    }
}

private enum Fixtures {
    static let heldWithQueue = """
        [
          {
            "container": "web",
            "running": true,
            "holder": {
              "label": "pr-4821-review",
              "tree": "/repo/.worktrees/pr-4821",
              "kind": "session",
              "heldFor": "4m12s",
              "remaining": "15m48s",
              "pinned": false
            },
            "serving": "pr-4821",
            "status": "ready",
            "drifted": false,
            "queue": [
              {"position": 1, "label": "feature-search", "tree": "/t/a", "waiting": "2m10s"},
              {"position": 2, "label": "fix-login", "tree": "/t/b", "waiting": "30s"}
            ]
          }
        ]
        """

    static let free = """
        [{"container":"web","running":true,"holder":null,
          "serving":"main","status":"ready","drifted":false,"queue":[]}]
        """

    static let pinnedByHand = """
        [{"container":"web","running":true,
          "holder":{"label":"main","tree":"/t/main","kind":"human","heldFor":"3m00s",
                    "pinned":true,"note":"debugging by hand"},
          "serving":"main","status":"ready","drifted":false,"queue":[]}]
        """

    static let drifted = """
        [{"container":"web","running":true,
          "holder":{"label":"pr-4821-review","tree":"/t/a","kind":"session","heldFor":"10s",
                    "remaining":"19m50s","pinned":false},
          "serving":"main","status":"ready","drifted":true,"queue":[]}]
        """

    static let dockerDown = """
        [{"container":"web","running":false,"holder":null,"drifted":false,"queue":[],
          "error":"docker inspect web: exit status 1"}]
        """
}
