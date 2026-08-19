// Command baton hands a single Docker container between several git worktrees,
// one at a time, so parallel sessions can share one dev environment without
// running each other's tests against the wrong branch.
package main

import (
	"os"

	"baton/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
