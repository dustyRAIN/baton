package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"baton/internal/docker"
)

// stackKind is the dependency toolchain a repo uses. It decides which defaults
// the supervisor applies and what starter strategy `baton init` writes.
type stackKind string

const (
	stackPnpm    stackKind = "pnpm"
	stackYarn    stackKind = "yarn"
	stackNpm     stackKind = "npm"
	stackPip     stackKind = "pip"
	stackUnknown stackKind = "unknown"
)

// detectStack reads the lockfiles in a code root. It mirrors detect_stack in
// the supervisor, and the two must agree — the supervisor picks the defaults at
// runtime, this picks what the starter strategy documents.
func detectStack(codeRoot string) stackKind {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(codeRoot, name))
		return err == nil
	}
	switch {
	case exists("pnpm-lock.yaml"):
		return stackPnpm
	case exists("yarn.lock"):
		return stackYarn
	case exists("package-lock.json"):
		return stackNpm
	case exists("requirements.txt"), exists("setup.py"):
		return stackPip
	default:
		return stackUnknown
	}
}

// hasMigrations reports whether a repo carries schema migrations, which is the
// one thing a tree switch can break in a way that outlives the switch.
func hasMigrations(codeRoot string) bool {
	for _, marker := range []string{"alembic.ini", "manage.py"} {
		if _, err := os.Stat(filepath.Join(codeRoot, marker)); err == nil {
			return true
		}
	}
	return false
}

// writeStrategy creates a starter strategy file, filled in from what could be
// detected. It never overwrites an existing one — that file is hand-maintained
// and re-running init should not silently discard someone's edits.
func writeStrategy(container *docker.Container) (path string, written bool, err error) {
	path = container.ControlPath("strategy.sh")
	if _, err := os.Stat(path); err == nil {
		return path, false, nil
	}

	stack := detectStack(container.CodeRoot)
	contents := strategyTemplate(stack, container)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		return path, false, fmt.Errorf("write %s: %w", path, err)
	}
	return path, true, nil
}

func strategyTemplate(stack stackKind, container *docker.Container) string {
	builder := &strings.Builder{}

	fmt.Fprintf(builder, `#!/bin/bash
# baton strategy for %s
#
# Sourced by the supervisor after its built-in defaults, so anything defined
# here wins. Delete a function to fall back to the default for the detected
# stack (%s).
#
# Runs as root inside the container on every tree switch, exactly like the
# runner script it replaces. Keep it in the repo so it gets reviewed.
#
# Available: BATON_TREE BATON_CODE BATON_CONTROL BATON_STORE BATON_CACHE
#            BATON_PORT BATON_STACK
# Helpers:   baton_log, baton_share_into <path>, baton_cache_into <path>, note <text>

`, container.Name, stack)

	if container.HealthPort != 0 {
		fmt.Fprintf(builder, "BATON_PORT=%d\nBATON_HEALTH_PATH=%s\n\n",
			container.HealthPort, container.HealthPath)
	} else {
		fmt.Fprintf(builder, "# BATON_PORT=      # not detected; set it or baton skips the host-side health check\n")
		fmt.Fprintf(builder, "# BATON_HEALTH_PATH=/\n\n")
	}

	// Once baton is installed the container's command is baton's own supervisor,
	// which tells the reader nothing. Only report a command baton did not set.
	original := strings.Join(container.Command, " ")
	if len(container.Command) > 0 && !strings.Contains(original, docker.ControlDir+"/supervisor.sh") {
		fmt.Fprintf(builder, "# The command baton replaced was:\n#   %s\n", original)
		fmt.Fprintf(builder, "# Anything it did beyond installing and starting — waiting on other\n")
		fmt.Fprintf(builder, "# services, one-time setup — needs porting into the hooks below.\n\n")
	}

	switch stack {
	case stackPnpm, stackYarn, stackNpm:
		fmt.Fprintf(builder, `# The defaults already share node_modules between trees with the same
# lockfile and install with %s. Uncomment to customise.
#
# baton_prepare() {
#     baton_share_into "$BATON_TREE/node_modules" || return 1
#     %s
#     baton_cache_into "$BATON_TREE/node_modules/.cache/mybundler"
# }
#
# baton_start() { exec ./scripts/start.sh; }
`, stack, installCommand(stack))

	case stackPip:
		builder.WriteString(`# Python installs into the image's system site-packages, not into the tree,
# so there is nothing per-tree to share and the editable install has to be
# repeated on every switch. The default already does this:
#
# baton_prepare() {
#     pip3 install -r requirements.txt
#     pip3 install -e .
# }
#
# baton_start() { exec uwsgi --wsgi yourapp:app --http-socket :$BATON_PORT; }
`)

	default:
		builder.WriteString(`# No stack was detected, so there are no useful defaults. Define at least
# baton_prepare, baton_start and baton_health.
#
# baton_prepare() { : ; }
# baton_start()   { exec your-server; }
# baton_health()  { curl -sf -o /dev/null "http://127.0.0.1:$BATON_PORT/"; }
`)
	}

	builder.WriteString(`
# Uncomment and list anything this service needs before it will start.
# baton_wait_deps() {
#     for service in mysql-8:3306; do
#         while ! nc -z -w1 "${service%%:*}" "${service##*:}"; do sleep 1; done
#     done
# }
`)

	if hasMigrations(container.CodeRoot) {
		builder.WriteString(`
# ---------------------------------------------------------------------------
# This repo has migrations, and the database is shared between every worktree
# while the schemas are not.
#
# Switching to a branch whose migrations are BEHIND the database gets a silent
# no-op from ` + "`upgrade head`" + `, and that branch then runs against a schema from
# the future. The only real fix is a downgrade, which destroys data, so the
# default refuses to do it and records a note instead — visible in
# ` + "`baton status`" + `. Treat migration-dependent results from a noted tree as
# untrustworthy.
#
# The default handles alembic. For anything else, define baton_migrate and
# keep the same rule: go forward automatically, never backward, and ` + "`note`" + `
# loudly when the tree and the database disagree.
#
# baton_migrate() {
#     python manage.py migrate --noinput
# }
`)
	}

	return builder.String()
}

func installCommand(stack stackKind) string {
	switch stack {
	case stackPnpm:
		return "pnpm install --frozen-lockfile"
	case stackYarn:
		return "yarn install --frozen-lockfile"
	case stackNpm:
		return "npm ci"
	default:
		return "# install here"
	}
}
