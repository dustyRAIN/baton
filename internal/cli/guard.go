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

// genericContainerCommands matches shell commands that need a running dev
// server whatever the project is called.
var genericContainerCommands = regexp.MustCompile(
	`(?i)(playwright|cypress|selenium|test:e2e|:e2e\b|e2e:)`,
)

// containerCommands builds the full pattern for one container, adding the
// things only knowable at runtime: its name, and the port it publishes. Without
// this the guard would only recognise the project it was first written for.
// A project often reaches its container through a reverse proxy on a different
// host and port than the container publishes, and no amount of inspection can
// discover that. --pattern lets the hook declare those.
func containerCommands(name string, port int, extra ...string) *regexp.Regexp {
	parts := []string{genericContainerCommands.String()}
	for _, pattern := range extra {
		if trimmed := strings.TrimSpace(pattern); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	if name != "" {
		// Only an actual exec into the container. An earlier rule also matched
		// the container's name anywhere near the word "test", which caught
		// unrelated work — `go test` in a repo whose fixtures mention the
		// container was enough — and that rule added nothing, since every real
		// invocation goes through some form of `docker exec`.
		quoted := regexp.QuoteMeta(name)
		parts = append(parts, `(docker|[\w-]*docker)\s+(exec|compose\s+exec)\s+\S*`+quoted)
	}
	if port != 0 {
		parts = append(parts, fmt.Sprintf(`(localhost|127\.0\.0\.1|0\.0\.0\.0):%d`, port))
	}
	return regexp.MustCompile(`(?i)(` + strings.Join(parts, "|") + `)`)
}

// batonInvocation matches the tool's own commands. Guarding these would
// deadlock the session: it could never take the baton it is being told to take.
//
// Multiline, with newline treated as a separator. A shell block that runs
// `baton take` on one line and the tests on the next is the normal shape of
// this, and recognising only ; & | meant those were blocked despite doing
// exactly the right thing.
var batonInvocation = regexp.MustCompile(`(?m)(^|[;&|\n]\s*)(\S*/)?baton\s`)

func runGuard(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("guard", flag.ContinueOnError)
	flags.SetOutput(stderr)
	containerFlag := flags.String("container", "", "container the guarded tools need (required)")
	treeFlag := flags.String("tree", "", "worktree to check (default: the hook's working directory)")
	patternFlag := flags.String("pattern", "", "extra regexes marking a command as container work, comma separated "+
		"(for a reverse-proxy hostname or port that cannot be detected)")
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

	if *containerFlag == "" {
		// Misconfiguration must not block work. Say nothing and let it through.
		return allow(stdout, "")
	}

	// The port is only knowable from the container, and Docker may be down, in
	// which case a name-only pattern is still useful.
	guardedPort := 0
	if live, err := docker.Inspect(*containerFlag); err == nil {
		guardedPort = live.DevPort()
	}
	extra := []string{}
	if *patternFlag != "" {
		extra = strings.Split(*patternFlag, ",")
	}
	if !needsContainer(input, containerCommands(*containerFlag, guardedPort, extra...)) {
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

	// A caller working somewhere else entirely — a different repository that
	// happens to mention this container — is none of the guard's business.
	// Only worktrees the container can actually serve are governed by it.
	if live, err := docker.Inspect(*containerFlag); err == nil {
		if _, err := live.ContainerPath(worktree.Path); err != nil {
			return allow(stdout, "")
		}
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
func needsContainer(input hookInput, pattern *regexp.Regexp) bool {
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
	return pattern.MatchString(command)
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
			label, containerName, prettyContainerPath(serving, container.CodeMount), containerName)
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
