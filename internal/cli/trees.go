package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"baton/internal/docker"
	"baton/internal/store"
	"baton/internal/tree"
)

// swapTo switches the container to a worktree and waits for it to be serving.
func swapTo(stateStore *store.Store, container *docker.Container, worktree *tree.Tree, stdout, stderr io.Writer) int {
	wanted, err := container.ContainerPath(worktree.Path)
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}
	if serving, _ := container.Serving(); serving == wanted {
		markServing(stateStore, container.Name, worktree.Path)
		return exitOK
	}
	if !container.Running || !container.Supervised() {
		fmt.Fprintf(stderr, "baton: %s cannot switch trees yet — start it and run `baton init %s`\n",
			container.Name, container.Name)
		return exitError
	}

	fmt.Fprintf(stderr, "baton: switching %s to %s\n", container.Name, worktree.Label)
	if _, err := container.RequestTree(worktree.Path); err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}
	if err := container.WaitReady(wanted, swapTimeout, pollInterval); err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}
	markServing(stateStore, container.Name, worktree.Path)
	fmt.Fprintf(stdout, "baton: serving %s on port %d\n", worktree.Label, container.DevPort())
	return exitOK
}

// TreeReport is one worktree as the menu bar app sees it.
type TreeReport struct {
	Label   string `json:"label"`
	Path    string `json:"path"`
	Branch  string `json:"branch,omitempty"`
	Main    bool   `json:"main"`
	Holding bool   `json:"holding"`
	Serving bool   `json:"serving"`
	Queued  bool   `json:"queued"`
}

// runTrees lists the worktrees a container can be switched to.
func runTrees(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("trees", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "emit machine-readable output")
	positionals, err := parseArguments(flags, arguments)
	if err != nil {
		return exitError
	}

	containerName := firstArgument(positionals)
	if containerName == "" {
		fmt.Fprintf(stderr, "baton: a container name is required\n")
		return exitError
	}
	container, err := docker.Inspect(containerName)
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}
	worktrees, err := tree.Siblings(container.CodeRoot)
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}

	stateStore, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}
	state, err := stateStore.Read()
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}
	tracked := state.Get(containerName)
	serving, _ := container.Serving()

	reports := make([]TreeReport, 0, len(worktrees))
	for _, worktree := range worktrees {
		entry := TreeReport{
			Label:  worktree.Label,
			Path:   worktree.Path,
			Branch: worktree.Branch,
			Main:   worktree.Main,
		}
		entry.Holding = tracked.Holder != nil && tracked.Holder.Tree == worktree.Path
		if inside, err := container.ContainerPath(worktree.Path); err == nil {
			entry.Serving = serving != "" && serving == inside
		}
		for _, waiter := range tracked.Queue {
			if waiter.Tree == worktree.Path {
				entry.Queued = true
			}
		}
		reports = append(reports, entry)
	}

	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(reports); err != nil {
			fmt.Fprintf(stderr, "baton: %v\n", err)
			return exitError
		}
		return exitOK
	}

	for _, entry := range reports {
		marks := ""
		switch {
		case entry.Holding && entry.Serving:
			marks = "holds it"
		case entry.Holding:
			marks = "holds it, not being served"
		case entry.Serving:
			marks = "being served"
		case entry.Queued:
			marks = "waiting"
		}
		if entry.Main {
			marks = trimJoin(marks, "main clone")
		}
		fmt.Fprintf(stdout, "  %-24s %s\n", entry.Label, marks)
	}
	return exitOK
}

func trimJoin(left, right string) string {
	if left == "" {
		return right
	}
	return left + ", " + right
}
