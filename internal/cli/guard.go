package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"baton/internal/core"
	"baton/internal/docker"
	"baton/internal/store"
	"baton/internal/tree"
)

// hookInput is the subset of Claude Code's PreToolUse payload that the guard
// reads. Unknown fields are ignored, so new payload keys cannot break it.
type hookInput struct {
	ToolName  string `json:"tool_name"`
	CWD       string `json:"cwd"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// hookOutput is a PreToolUse decision.
type hookOutput struct {
	HookSpecificOutput hookDecision `json:"hookSpecificOutput"`
}

type hookDecision struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

// containerCommands matches shell commands that only make sense against the
// shared dev container. Anything matching needs the baton first.
var containerCommands = regexp.MustCompile(
	`(?i)(playwright|test:e2e|nc-docker\s+(exec|test)|docker\s+exec\s+cmp-|localhost:3301|127\.0\.0\.1:3301|cmp-localdev)`,
)

// batonInvocation matches the tool's own commands. Guarding these would
// deadlock the session: it could never take the baton it is being told to take.
var batonInvocation = regexp.MustCompile(`(^|[;&|]\s*)(\S*/)?baton\s`)

func runGuard(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("guard", flag.ContinueOnError)
	flags.SetOutput(stderr)
	containerFlag := flags.String("container", "cmp-client", "container the guarded tools need")
	treeFlag := flags.String("tree", "", "worktree to check (default: the hook's working directory)")
	if _, err := parseArguments(flags, arguments); err != nil {
		return exitError
	}

	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		return allow(stdout, "")
	}

	input := hookInput{}
	if err := json.Unmarshal(payload, &input); err != nil {
		// A payload we cannot read is not evidence of a problem. Say nothing and
		// let the call through rather than blocking on our own parsing bug.
		return allow(stdout, "")
	}

	if !needsContainer(input) {
		return allow(stdout, "")
	}

	workingDir := *treeFlag
	if workingDir == "" {
		workingDir = input.CWD
	}
	worktree, err := tree.Resolve(workingDir)
	if err != nil {
		return allow(stdout, "")
	}

	stateStore, err := openStore()
	if err != nil {
		return allow(stdout, "")
	}
	state, err := stateStore.Read()
	if err != nil {
		return allow(stdout, "")
	}

	container := state.Get(*containerFlag)

	// Nobody is using baton for this container yet. Staying quiet here is what
	// lets the guard be installed before every session has adopted it.
	if container.Holder == nil && len(container.Queue) == 0 {
		return allow(stdout, "")
	}

	if core.Holds(state, *containerFlag, worktree.Path) {
		if reason := servingMismatch(*containerFlag, worktree.Path, worktree.Label); reason != "" {
			return deny(stdout, reason)
		}
		return allow(stdout, "")
	}

	return deny(stdout, denialReason(*containerFlag, worktree.Label, container.Holder))
}

// needsContainer decides whether a tool call would touch the shared dev server.
func needsContainer(input hookInput) bool {
	if strings.HasPrefix(input.ToolName, "mcp__playwright__") {
		return true
	}
	if input.ToolName != "Bash" {
		return false
	}
	command := input.ToolInput.Command
	if batonInvocation.MatchString(command) {
		return false
	}
	return containerCommands.MatchString(command)
}

// servingMismatch reports why a holder should not trust the container, or "" if
// everything lines up. This catches the case where the lock says yes but the
// dev server is still on somebody else's tree.
func servingMismatch(containerName, treePath, label string) string {
	container, err := docker.Inspect(containerName)
	if err != nil {
		// Docker being unreachable is an infrastructure problem, not a queue
		// violation. Let the call through; it will fail on its own terms.
		return ""
	}
	wanted, err := container.ContainerPath(treePath)
	if err != nil {
		return ""
	}
	serving, status := container.Serving()
	if serving == "" {
		return ""
	}
	if serving != wanted {
		return fmt.Sprintf(
			"%s holds the baton for %s, but the container is serving %s. Results would belong to the wrong branch. Run `baton take %s --wait` to switch it.",
			label, containerName, prettyContainerPath(serving), containerName)
	}
	if status != "ready" {
		return fmt.Sprintf(
			"%s is switching to your tree (status %q). Wait for `baton take %s --wait` to report ready.",
			containerName, status, containerName)
	}
	return ""
}

func denialReason(containerName, label string, holder *store.Holder) string {
	if holder == nil {
		return fmt.Sprintf(
			"%s does not hold the baton for %s. Run `baton take %s --wait` in the background first, then retry.",
			label, containerName, containerName)
	}
	if holder.Pinned() {
		return fmt.Sprintf(
			"%s is held by hand (%s) and the queue is paused. Wait for `baton drop %s`.",
			containerName, holder.Label, containerName)
	}
	return fmt.Sprintf(
		"%s is held by %s, not %s. Any results would be against their branch. Run `baton take %s --wait` in the background and retry when it reports ready.",
		containerName, holder.Label, label, containerName)
}

func allow(stdout io.Writer, reason string) int {
	emit(stdout, "allow", reason)
	return exitOK
}

func deny(stdout io.Writer, reason string) int {
	emit(stdout, "deny", reason)
	return exitOK
}

func emit(stdout io.Writer, decision, reason string) {
	output := hookOutput{HookSpecificOutput: hookDecision{
		HookEventName:            "PreToolUse",
		PermissionDecision:       decision,
		PermissionDecisionReason: reason,
	}}
	encoded, err := json.Marshal(output)
	if err != nil {
		return
	}
	fmt.Fprintln(stdout, string(encoded))
}
