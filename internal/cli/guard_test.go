package cli

import "testing"

// guardPattern is the pattern for a project named like the one baton was first
// written against, so the existing expectations still describe real behaviour.
var guardPattern = containerCommands("cmp-client", 3301)

func bashCall(command string) hookInput {
	input := hookInput{ToolName: "Bash"}
	input.ToolInput.Command = command
	return input
}

func TestPlaywrightToolsNeedTheContainer(t *testing.T) {
	tools := []string{
		"mcp__playwright__browser_navigate",
		"mcp__playwright__browser_click",
		"mcp__playwright__browser_snapshot",
	}
	for _, tool := range tools {
		if !needsContainer(hookInput{ToolName: tool}, guardPattern) {
			t.Errorf("%s should require the baton", tool)
		}
	}
}

func TestUnrelatedToolsAreNotGuarded(t *testing.T) {
	tools := []string{"Read", "Edit", "Grep", "mcp__atlassian__getJiraIssue"}
	for _, tool := range tools {
		if needsContainer(hookInput{ToolName: tool}, guardPattern) {
			t.Errorf("%s does not touch the container and should not be guarded", tool)
		}
	}
}

func TestContainerBoundShellCommandsAreGuarded(t *testing.T) {
	commands := []string{
		"pnpm run test:e2e",
		"npx playwright test client/foo",
		"PYENV_VERSION=localdev nc-docker exec cmp-client 'pnpm test'",
		"docker exec cmp-client bash -c 'ls'",
		"curl http://localhost:3301/",
	}
	for _, command := range commands {
		if !needsContainer(bashCall(command), guardPattern) {
			t.Errorf("%q needs the container and should be guarded", command)
		}
	}
}

func TestOrdinaryShellCommandsRunFreely(t *testing.T) {
	commands := []string{
		"pnpm run lint",
		"git status",
		"pnpm test --nc-path=client/foo/ --single-run",
		"ls -la",
		"npx tsc --noEmit",
	}
	for _, command := range commands {
		if needsContainer(bashCall(command), guardPattern) {
			t.Errorf("%q does not need the container; guarding it would block ordinary work", command)
		}
	}
}

func TestBatonsOwnCommandsAreNeverGuarded(t *testing.T) {
	// Guarding these would deadlock a session: it could never take the baton it
	// is being told to take.
	commands := []string{
		"baton take cmp-client --wait",
		"/usr/local/bin/baton take cmp-client --wait",
		"baton status cmp-client",
		"baton pass cmp-client",
	}
	for _, command := range commands {
		if needsContainer(bashCall(command), guardPattern) {
			t.Errorf("%q is baton itself and must never be blocked", command)
		}
	}
}

func TestTakeThenTestIsAllowedThrough(t *testing.T) {
	// This is the pattern sessions are told to use, and it has to pass: the
	// take happens before the test in the same command.
	command := "baton take cmp-client --wait && pnpm run test:e2e"
	if needsContainer(bashCall(command), guardPattern) {
		t.Errorf("%q takes the baton before testing and must be allowed", command)
	}
}

func TestChainingOffAnyBatonCommandSlipsThrough(t *testing.T) {
	// Known and accepted: any command mentioning baton is treated as
	// coordinating, so a read-only baton call chained before container work is
	// not caught. Narrowing this would break the take-then-test pattern above,
	// which matters more. The guard is a net for forgetfulness, not a boundary
	// against deliberate evasion.
	command := "baton status cmp-client && npx playwright test"
	if needsContainer(bashCall(command), guardPattern) {
		t.Errorf("behaviour changed: %q is now guarded. That may be an improvement, "+
			"but check the take-then-test pattern still passes", command)
	}
}

func TestPatternsAreBuiltFromTheContainerNotBakedIn(t *testing.T) {
	// baton has to work for projects it was not written against. The container
	// name and its published port are only knowable at runtime.
	pattern := containerCommands("acme-api", 8080)

	guarded := []string{
		"docker exec acme-api pytest",
		"curl http://localhost:8080/health",
		"npx playwright test",
	}
	for _, command := range guarded {
		if !needsContainer(bashCall(command), pattern) {
			t.Errorf("%q targets acme-api and should be guarded", command)
		}
	}

	// Another project's port and container must not be caught by this one.
	free := []string{
		"curl http://localhost:3301/",
		"docker exec other-service ls",
		"pytest tests/unit",
	}
	for _, command := range free {
		if needsContainer(bashCall(command), pattern) {
			t.Errorf("%q has nothing to do with acme-api and should run freely", command)
		}
	}
}

func TestAPatternWithoutAPortStillCatchesTheObviousCases(t *testing.T) {
	// Docker may be down when the guard runs, so the port is unknown. The
	// name-based and tool-based rules have to carry it.
	pattern := containerCommands("acme-api", 0)

	if !needsContainer(bashCall("npx playwright test"), pattern) {
		t.Error("playwright should be caught even with no port known")
	}
	if !needsContainer(bashCall("docker exec acme-api sh"), pattern) {
		t.Error("an exec into the container should be caught even with no port known")
	}
}

func TestExtraPatternsCoverAReverseProxy(t *testing.T) {
	// The URL a project actually hits is often a proxy on a different host and
	// port than the container publishes. Nothing baton can inspect reveals
	// that, so the hook declares it with --pattern.
	pattern := containerCommands("cmp-client", 3301, `cmp-localdev`)

	command := "curl -k https://cmp-localdev.example.com:3443/"
	if !needsContainer(bashCall(command), pattern) {
		t.Errorf("%q reaches the container through the proxy and should be guarded", command)
	}
	if needsContainer(bashCall(command), containerCommands("cmp-client", 3301)) {
		t.Error("without the declared pattern this is undetectable, which is why the flag exists")
	}
}
