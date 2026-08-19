package cli

import "testing"

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
		if !needsContainer(hookInput{ToolName: tool}) {
			t.Errorf("%s should require the baton", tool)
		}
	}
}

func TestUnrelatedToolsAreNotGuarded(t *testing.T) {
	tools := []string{"Read", "Edit", "Grep", "mcp__atlassian__getJiraIssue"}
	for _, tool := range tools {
		if needsContainer(hookInput{ToolName: tool}) {
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
		"curl -k https://cmp-localdev.optimizely.com:3443/",
	}
	for _, command := range commands {
		if !needsContainer(bashCall(command)) {
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
		if needsContainer(bashCall(command)) {
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
		if needsContainer(bashCall(command)) {
			t.Errorf("%q is baton itself and must never be blocked", command)
		}
	}
}

func TestTakeThenTestIsAllowedThrough(t *testing.T) {
	// This is the pattern sessions are told to use, and it has to pass: the
	// take happens before the test in the same command.
	command := "baton take cmp-client --wait && pnpm run test:e2e"
	if needsContainer(bashCall(command)) {
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
	if needsContainer(bashCall(command)) {
		t.Errorf("behaviour changed: %q is now guarded. That may be an improvement, "+
			"but check the take-then-test pattern still passes", command)
	}
}
