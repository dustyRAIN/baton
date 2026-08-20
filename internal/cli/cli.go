// Package cli wires baton's subcommands to the queue logic underneath.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"baton/internal/core"
	"baton/internal/docker"
	"baton/internal/store"
	"baton/internal/tree"
)

// Version is stamped at build time by the Makefile.
var Version = "dev"

// Exit codes. Anything scripted against baton keys off these, so they are part
// of the interface: 2 always means "the answer is no", never "something broke".
const (
	exitOK     = 0
	exitError  = 1
	exitDenied = 2
)

const (
	defaultLease   = 20 * time.Minute
	defaultTimeout = 2 * time.Hour
	pollInterval   = 750 * time.Millisecond
	swapTimeout    = 20 * time.Minute
)

// Run dispatches a command line and returns the process exit code.
func Run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) == 0 {
		usage(stdout)
		return exitError
	}

	command, rest := arguments[0], arguments[1:]
	handlers := map[string]func([]string, io.Writer, io.Writer) int{
		"take":   runTake,
		"pass":   runPass,
		"status": runStatus,
		"line":   runLine,
		"check":  runCheck,
		"grab":   runGrab,
		"drop":   runDrop,
		"renew":  runRenew,
		"init":   runInit,
		"guard":  runGuard,

		"trees":         runTrees,
		"install-skill": runInstallSkill,
	}

	if handler, found := handlers[command]; found {
		return handler(rest, stdout, stderr)
	}

	switch command {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "baton %s\n", Version)
		return exitOK
	case "help", "--help", "-h":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "baton: unknown command %q\n\n", command)
		usage(stderr)
		return exitError
	}
}

func usage(out io.Writer) {
	fmt.Fprint(out, `baton — hand one Docker container between several worktrees, one at a time.

  baton take <container>    take the baton, optionally waiting in line for it
  baton pass <container>    give the baton back
  baton renew <container>   extend your hold before the lease runs out
  baton check <container>   exit 0 if you still hold it, 2 if you do not

  baton status [container]  who holds what, and who is waiting
  baton line [container]    just the queue

  baton grab <container> [worktree]
                            take over by hand and pin it, on any worktree
  baton trees <container>   list the worktrees it can be switched to
  baton drop <container>    release a hand-taken baton

  baton init <container>    install the supervisor so handoffs skip a restart
  baton guard               PreToolUse hook: refuse container work without the baton
  baton install-skill       install the Claude Code skill that teaches sessions to use this

Every command takes --tree to name a worktree explicitly. It defaults to the
worktree containing the current directory.

`)
}

// parseArguments parses flags that appear on either side of the positional
// arguments. Go's flag package stops at the first non-flag word, which would
// silently drop the flag in `baton take web --wait` — the way anyone
// would naturally type it. Parsing in rounds, peeling off one positional each
// time, accepts both orders.
func parseArguments(flags *flag.FlagSet, arguments []string) ([]string, error) {
	positionals := []string{}
	remaining := arguments

	for {
		if err := flags.Parse(remaining); err != nil {
			return nil, err
		}
		leftover := flags.Args()
		if len(leftover) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, leftover[0])
		remaining = leftover[1:]
	}
}

// firstArgument returns the leading positional, or "" when there is none.
func firstArgument(positionals []string) string {
	if len(positionals) == 0 {
		return ""
	}
	return positionals[0]
}

// resolveTarget works out which worktree and which container a command applies
// to, and checks the two are compatible.
func resolveTarget(containerName, treeFlag string) (*tree.Tree, *docker.Container, error) {
	if containerName == "" {
		return nil, nil, fmt.Errorf("a container name is required")
	}
	worktree, err := tree.Resolve(treeFlag)
	if err != nil {
		return nil, nil, err
	}
	container, err := docker.Inspect(containerName)
	if err != nil {
		return nil, nil, err
	}
	if _, err := container.ContainerPath(worktree.Path); err != nil {
		return nil, nil, err
	}
	return worktree, container, nil
}

func openStore() (*store.Store, error) {
	return store.Open(store.DefaultDir())
}

// ---------------------------------------------------------------- take

func runTake(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("take", flag.ContinueOnError)
	flags.SetOutput(stderr)
	treeFlag := flags.String("tree", "", "worktree to take the baton for (default: current)")
	labelFlag := flags.String("label", "", "name to show in status output (default: branch name)")
	wait := flags.Bool("wait", false, "stay in line until the baton is granted")
	lease := flags.Duration("lease", defaultLease, "how long the hold lasts before it lapses")
	timeout := flags.Duration("timeout", defaultTimeout, "give up waiting after this long")
	noSwap := flags.Bool("no-swap", false, "take the baton but leave the container serving whatever it is serving")
	positionals, err := parseArguments(flags, arguments)
	if err != nil {
		return exitError
	}

	worktree, container, err := resolveTarget(firstArgument(positionals), *treeFlag)
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}
	label := *labelFlag
	if label == "" {
		label = worktree.Label
	}

	stateStore, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}

	// Leaving the queue on interrupt is a courtesy — a dead PID would be reaped
	// anyway — but it keeps status output honest the moment a wait is aborted.
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupted)

	deadline := time.Now().Add(*timeout)
	announced := false

	for {
		var result core.TakeResult
		err := stateStore.Update(func(state *store.State) error {
			var takeErr error
			result, takeErr = core.Take(state, core.TakeRequest{
				Container: container.Name,
				Tree:      worktree.Path,
				Label:     label,
				Lease:     *lease,
				PID:       os.Getpid(),
				Now:       time.Now(),
				Enqueue:   *wait,
			})
			return takeErr
		})
		if err != nil {
			fmt.Fprintf(stderr, "baton: %v\n", err)
			return exitError
		}

		if result.Outcome == core.Granted {
			return finishTake(stateStore, container, worktree, label, *noSwap, stdout, stderr)
		}

		if !*wait {
			describeDenial(stdout, container.Name, label, result)
			return exitDenied
		}

		if !announced {
			describeDenial(stderr, container.Name, label, result)
			fmt.Fprintf(stderr, "baton: waiting for %s (timeout %s)\n", container.Name, *timeout)
			announced = true
		}

		if time.Now().After(deadline) {
			leaveQueue(stateStore, container.Name, worktree.Path)
			fmt.Fprintf(stderr, "baton: gave up after %s waiting for %s\n", *timeout, container.Name)
			return exitDenied
		}

		select {
		case <-interrupted:
			leaveQueue(stateStore, container.Name, worktree.Path)
			fmt.Fprintf(stderr, "baton: left the queue for %s\n", container.Name)
			return exitDenied
		case <-time.After(pollInterval):
		}
	}
}

// finishTake performs the container swap for a freshly granted baton and only
// then reports success. A grant is not useful until the dev server is actually
// serving the holder's tree, so the two are reported as one event.
func finishTake(stateStore *store.Store, container *docker.Container, worktree *tree.Tree, label string, noSwap bool, stdout, stderr io.Writer) int {
	if noSwap {
		fmt.Fprintf(stdout, "baton: %s is yours (%s), container left as-is\n", container.Name, label)
		return exitOK
	}

	serving, _ := container.Serving()
	wanted, err := container.ContainerPath(worktree.Path)
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		leaveQueue(stateStore, container.Name, worktree.Path)
		return exitError
	}

	if serving == wanted {
		markServing(stateStore, container.Name, worktree.Path)
		fmt.Fprintf(stdout, "baton: %s is yours (%s), already serving it\n", container.Name, label)
		return exitOK
	}

	if !container.Running {
		fmt.Fprintf(stderr, "baton: %s is not running — start it, then take again\n", container.Name)
		releaseAfterFailure(stateStore, container.Name, worktree.Path)
		return exitError
	}
	if !container.Supervised() {
		fmt.Fprintf(stderr, "baton: %s has no supervisor installed — run `baton init %s` first\n",
			container.Name, container.Name)
		releaseAfterFailure(stateStore, container.Name, worktree.Path)
		return exitError
	}

	fmt.Fprintf(stderr, "baton: switching %s to %s\n", container.Name, label)
	if _, err := container.RequestTree(worktree.Path); err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		releaseAfterFailure(stateStore, container.Name, worktree.Path)
		return exitError
	}
	if err := container.WaitReady(wanted, swapTimeout, pollInterval); err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		releaseAfterFailure(stateStore, container.Name, worktree.Path)
		return exitError
	}

	markServing(stateStore, container.Name, worktree.Path)
	fmt.Fprintf(stdout, "baton: %s is yours (%s), serving on port %d\n", container.Name, label, container.DevPort())
	return exitOK
}

func describeDenial(out io.Writer, containerName, label string, result core.TakeResult) {
	holder := "nobody"
	if result.Holder != nil {
		holder = result.Holder.Label
		if result.Holder.Kind == store.KindHuman {
			holder += " (held by hand)"
		}
	}

	// Position is zero for a one-shot attempt, which did not join the line.
	// Saying so plainly matters: a caller that thinks it has a place in the
	// queue will wait for a turn that is never coming.
	if result.Position > 0 {
		fmt.Fprintf(out, "baton: %s is held by %s; %s is #%d in line\n",
			containerName, holder, label, result.Position)
	} else {
		fmt.Fprintf(out, "baton: %s is held by %s; %s is not queued (pass --wait to join the line)\n",
			containerName, holder, label)
	}

	if len(result.Ahead) > 0 {
		fmt.Fprintf(out, "       waiting: %s\n", strings.Join(result.Ahead, ", "))
	}
	if result.Outcome == core.Blocked {
		fmt.Fprintf(out, "       the queue is paused until it is dropped by hand\n")
	}
}

func leaveQueue(stateStore *store.Store, containerName, treePath string) {
	stateStore.Update(func(state *store.State) error {
		core.Pass(state, containerName, treePath)
		return nil
	})
}

// releaseAfterFailure hands the baton straight back when a swap fails, so a
// broken tree cannot block everyone else behind it.
func releaseAfterFailure(stateStore *store.Store, containerName, treePath string) {
	leaveQueue(stateStore, containerName, treePath)
}

func markServing(stateStore *store.Store, containerName, treePath string) {
	stateStore.Update(func(state *store.State) error {
		state.Get(containerName).Serving = treePath
		return nil
	})
}

// ---------------------------------------------------------------- pass

func runPass(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pass", flag.ContinueOnError)
	flags.SetOutput(stderr)
	treeFlag := flags.String("tree", "", "worktree giving the baton back (default: current)")
	positionals, err := parseArguments(flags, arguments)
	if err != nil {
		return exitError
	}

	containerName := firstArgument(positionals)
	if containerName == "" {
		fmt.Fprintf(stderr, "baton: a container name is required\n")
		return exitError
	}
	worktree, err := tree.Resolve(*treeFlag)
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}

	stateStore, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}

	released := false
	var next string
	err = stateStore.Update(func(state *store.State) error {
		released = core.Pass(state, containerName, worktree.Path)
		queue := state.Get(containerName).Queue
		if len(queue) > 0 {
			next = queue[0].Label
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}

	if !released {
		fmt.Fprintf(stdout, "baton: %s did not hold %s, nothing to pass\n", worktree.Label, containerName)
		return exitOK
	}
	if next != "" {
		fmt.Fprintf(stdout, "baton: passed %s; %s is up next\n", containerName, next)
	} else {
		fmt.Fprintf(stdout, "baton: passed %s; nobody is waiting\n", containerName)
	}
	return exitOK
}

// ---------------------------------------------------------------- check

func runCheck(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	treeFlag := flags.String("tree", "", "worktree to check (default: current)")
	quiet := flags.Bool("quiet", false, "say nothing, just set the exit code")
	positionals, err := parseArguments(flags, arguments)
	if err != nil {
		return exitError
	}

	containerName := firstArgument(positionals)
	if containerName == "" {
		fmt.Fprintf(stderr, "baton: a container name is required\n")
		return exitError
	}
	worktree, err := tree.Resolve(*treeFlag)
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

	holds := core.Holds(state, containerName, worktree.Path)

	// Bookkeeping is not enough. Ask the container what it is actually serving,
	// because a stale answer here means test results are about to be attributed
	// to the wrong branch.
	drifted := false
	if holds {
		if container, err := docker.Inspect(containerName); err == nil {
			if wanted, err := container.ContainerPath(worktree.Path); err == nil {
				serving, status := container.Serving()
				if serving != wanted || status != "ready" {
					drifted = true
				}
			}
		}
	}

	if *quiet {
		if holds && !drifted {
			return exitOK
		}
		return exitDenied
	}

	switch {
	case holds && !drifted:
		fmt.Fprintf(stdout, "baton: yes — %s holds %s\n", worktree.Label, containerName)
		return exitOK
	case holds && drifted:
		fmt.Fprintf(stdout, "baton: no — %s holds the lock but %s is not serving it; discard any results\n",
			worktree.Label, containerName)
		return exitDenied
	default:
		holder := "nobody"
		if container := state.Get(containerName); container.Holder != nil {
			holder = container.Holder.Label
		}
		fmt.Fprintf(stdout, "baton: no — %s is held by %s; discard any results from this tree\n",
			containerName, holder)
		return exitDenied
	}
}

// ---------------------------------------------------------------- renew

func runRenew(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("renew", flag.ContinueOnError)
	flags.SetOutput(stderr)
	treeFlag := flags.String("tree", "", "worktree renewing its hold (default: current)")
	lease := flags.Duration("lease", defaultLease, "how much longer to hold it")
	positionals, err := parseArguments(flags, arguments)
	if err != nil {
		return exitError
	}

	containerName := firstArgument(positionals)
	worktree, err := tree.Resolve(*treeFlag)
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}
	stateStore, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}

	err = stateStore.Update(func(state *store.State) error {
		return core.Renew(state, containerName, worktree.Path, *lease, time.Now())
	})
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitDenied
	}
	fmt.Fprintf(stdout, "baton: %s held for another %s\n", containerName, *lease)
	return exitOK
}

// ---------------------------------------------------------------- grab / drop

func runGrab(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("grab", flag.ContinueOnError)
	flags.SetOutput(stderr)
	treeFlag := flags.String("tree", "", "worktree to switch to (default: the main clone)")
	note := flags.String("note", "", "why you took over, shown in status")
	noSwap := flags.Bool("no-swap", false, "take it without switching the container")
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

	// Which worktree to pin it to may be named as a second word or with
	// --tree, by branch name, directory name or path. It defaults to the main
	// clone, since the commonest reason to take over is getting your own
	// working copy back in front of you — but testing somebody else's branch by
	// hand is just as good a reason, and that needs a name.
	target := *treeFlag
	if target == "" && len(positionals) > 1 {
		target = positionals[1]
	}

	var worktree *tree.Tree
	if target == "" {
		worktree, err = tree.Resolve(container.CodeRoot)
	} else {
		worktree, err = tree.Find(container.CodeRoot, target)
	}
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}
	if _, err := container.ContainerPath(worktree.Path); err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}

	stateStore, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}

	var displaced *store.Holder
	var waiting int
	err = stateStore.Update(func(state *store.State) error {
		displaced = core.Grab(state, containerName, worktree.Path, worktree.Label, *note, time.Now())
		waiting = len(state.Get(containerName).Queue)
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}

	if displaced != nil {
		fmt.Fprintf(stdout, "baton: took %s from %s\n", containerName, displaced.Label)
	}
	fmt.Fprintf(stdout, "baton: %s is pinned to %s until you run `baton drop %s`\n",
		containerName, worktree.Label, containerName)
	if waiting > 0 {
		fmt.Fprintf(stdout, "       %d session(s) waiting behind you\n", waiting)
	}

	if *noSwap {
		return exitOK
	}
	return swapTo(stateStore, container, worktree, stdout, stderr)
}

func runDrop(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("drop", flag.ContinueOnError)
	flags.SetOutput(stderr)
	positionals, err := parseArguments(flags, arguments)
	if err != nil {
		return exitError
	}
	containerName := firstArgument(positionals)
	if containerName == "" {
		fmt.Fprintf(stderr, "baton: a container name is required\n")
		return exitError
	}

	stateStore, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}

	dropped := false
	next := ""
	err = stateStore.Update(func(state *store.State) error {
		dropped = core.Drop(state, containerName)
		queue := state.Get(containerName).Queue
		if len(queue) > 0 {
			next = queue[0].Label
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}
	if !dropped {
		fmt.Fprintf(stdout, "baton: %s was not held by hand\n", containerName)
		return exitOK
	}
	if next != "" {
		fmt.Fprintf(stdout, "baton: dropped %s; %s is up next\n", containerName, next)
	} else {
		fmt.Fprintf(stdout, "baton: dropped %s; nobody is waiting\n", containerName)
	}
	return exitOK
}
