// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "BatonMenuBar",
    platforms: [.macOS(.v14)],
    targets: [
        // Everything that talks to the baton CLI, with no UI attached, so the
        // probe and the tests exercise exactly the code the menu bar runs.
        .target(
            name: "BatonCore",
            path: "Sources/BatonCore"
        ),
        .executableTarget(
            name: "BatonMenuBar",
            dependencies: ["BatonCore"],
            path: "Sources/BatonMenuBar"
        ),
        // Prints what the menu bar would show, and self-checks the decoding
        // against captured fixtures. This machine has Command Line Tools but no
        // Xcode, so SPM has no XCTest; folding the checks into a runnable binary
        // keeps them executable in CI and by hand.
        .executableTarget(
            name: "baton-probe",
            dependencies: ["BatonCore"],
            path: "Sources/BatonProbe"
        ),
    ]
)
