# baton

**One Docker dev container, many git worktrees, one at a time.**

If you work on several branches at once — each in its own worktree, each driven
by a person or a coding agent — they all need the same dev container to run
functional tests in. Only one can have it. baton is the queue that decides who,
and hands it over cleanly when they are done.

```
$ baton status web
web
  holder    pr-4821-review         4m12s in, 15m48s left
  serving   pr-4821                ready
  queue     1. feature-search      waiting 2m10s
            2. fix-login           waiting 30s
```

---

## Contents

- [Try it in ten minutes](#try-it-in-ten-minutes)
- [Why this is not just a lock file](#why-this-is-not-just-a-lock-file)
- [Requirements](#requirements)
- [Install](#install)
- [Setting up a container](#setting-up-a-container)
- [Everyday use](#everyday-use)
- [Taking over by hand](#taking-over-by-hand)
- [Using it with Claude Code](#using-it-with-claude-code)
- [Other stacks: strategies](#other-stacks-strategies)
- [Repos with database migrations](#repos-with-database-migrations)
- [Menu bar app (macOS)](#menu-bar-app-macos)
- [Command reference](#command-reference)
- [How it works](#how-it-works)
- [Troubleshooting](#troubleshooting)
- [Known limitations](#known-limitations)

---

## Try it in ten minutes

The fastest way to understand baton is to watch a container hand itself between
two branches. There is a runnable demo that builds a throwaway project, so you
can do that without touching anything you own:

```bash
./examples/walkthrough/setup.sh
```

Then follow [examples/walkthrough/README.md](examples/walkthrough/README.md). It
takes about ten minutes and ends with the failure this tool exists to prevent,
demonstrated rather than described.

---

## Why this is not just a lock file

Two things make this harder than a mutex.

**The container has to follow the baton.** Taking a lock is useless if the dev
server is still serving somebody else's branch — your tests pass against their
code and you never find out. So baton does not report a grant until the
container is actually serving your tree and answering on its port, and
`baton check` re-verifies that against the container rather than trusting its
own bookkeeping.

**Handoffs have to be cheap.** If switching trees costs five minutes, nobody
gives the container back between test runs and the queue seizes up. baton never
restarts the container: a supervisor inside it moves the app between worktrees,
sharing one dependency directory per lockfile and keeping build caches per tree.
Measured on a large React app, a warm handoff is about 30 seconds.

---

## Requirements

| | |
| --- | --- |
| **OS** | macOS or Linux. The menu bar app is macOS 14+ only; the CLI is portable. |
| **Docker** | Any daemon. The container must bind-mount your repository. |
| **Build** | Go 1.22+. Swift 6 / Xcode command line tools for the menu bar app. |
| **Git** | Worktrees, and see the constraint below. |

Three things about your container have to be true.

**1. The repository is bind-mounted into it.** baton finds the mount by looking
for one whose host side is a git repository, so `/code`, `/app`, `/usr/src/app`
and anything else all work. Set `BATON_CODE_MOUNT` to the path inside the
container if the guess is ever wrong.

**2. You can change how the container starts.** baton replaces the container's
`command` with its supervisor, normally through a `docker-compose.override.yml`
that `baton init` writes for you.

**3. Your worktrees live inside the mounted directory.** This is the constraint
people trip on. The container can only see what is under the mount, so a
worktree somewhere else is invisible to it.

```bash
# Good: inside the repo, so the container already sees it. No extra mounts.
git worktree add .worktrees/pr-4821 -b pr-4821-review origin/main

# Bad: outside the mount, invisible to the container.
git worktree add ../pr-4821 -b pr-4821-review origin/main
```

Add `.worktrees/` to `.git/info/exclude` so it stays out of git status.

**Optional but recommended:** `privileged: true` on the container. baton uses
bind mounts inside the container to share one dependency directory across
worktrees with the same lockfile. Without it, each tree installs its own — which
still works, just slower and larger.

---

## Install

```bash
git clone <this-repo> baton
cd baton
make install                  # builds and installs to /usr/local/bin/baton
```

Use `PREFIX=~/.local make install` to install somewhere else.

```bash
make check                    # gofmt, go vet, go test
```

---

## Setting up a container

Look before you leap. This writes nothing:

```bash
baton init web --dry-run
```

```
container   web
code root   /Users/you/projects/app
stack       pnpm
health      port 3000, path /
migrations  false
command     /code/docker/entrypoint.sh

would write /Users/you/projects/app/.baton/strategy.sh:
...
```

Check that the stack, port and code root are right, then:

```bash
baton init web
```

That does four things:

1. installs the supervisor at `<repo>/.baton/supervisor.sh`
2. writes a starter `<repo>/.baton/strategy.sh` for the detected stack
3. adds `.baton/` to `.git/info/exclude` so it stays out of git
4. writes `docker-compose.override.yml` pointing the container at the supervisor

Then **recreate** the container — a restart is not enough, because the `command`
has to change:

```bash
docker compose up -d web      # or your project's wrapper
baton status web
```

After that one recreate, the container stays up permanently and only the app
moves between trees.

---

## Everyday use

```bash
baton take web --wait     # block until it is yours AND serving your tree
# ... run your tests ...
baton check web           # still mine? exit 0 yes, 2 no
baton pass web            # give it back
```

**Take it for the test run, not the whole session.** Writing code does not need
the container. Handoffs are cheap precisely so you can give it back between
iterations and let someone else in.

`--wait` is the one to background. It sits in line, and when it wins it performs
the swap and exits — so its exit *is* the signal that the container is ready and
serving your tree. Without `--wait`, `take` returns immediately: exit 0 got it,
exit 2 did not.

Holds carry a **20 minute lease** by default (`--lease` to change, `renew` to
extend). If your session dies mid-test, the lease lapses and the queue moves on
without anyone having to notice.

### The habit that matters

After every batch of tests, run `baton check`. If it exits 2, **throw those
results away**. You were preempted, or the swap failed, and the tests you just
ran were against somebody else's code. They will have passed or failed
perfectly convincingly.

---

## Taking over by hand

```bash
baton grab web --note "debugging something"
baton drop web
```

`grab` displaces whoever holds it, switches the container to your main clone,
and **pins** it. Sessions in the queue stay in the queue and stop advancing.
Nothing moves until you `drop`.

Human holds never expire. If you took it by hand, only you give it back.

---

## Using it with Claude Code

Two pieces: a skill so sessions *know* about baton, and a hook so they *cannot
skip it*.

### The skill

```bash
baton install-skill
```

Installs to `~/.claude/skills/baton/SKILL.md`. Open it and replace `<container>`
with the container your project shares.

**Why user level and not in the repo.** Claude Code keys both memory and project
settings to the session's working directory. A session started in a git worktree
gets its own, empty one and never sees anything written for the main clone. A
user-level skill is the only placement that reaches every session wherever it
starts — which matters most for exactly the worktree sessions this tool is for.

Skills are read at startup, so start a new session to pick it up.

### The guard hook

A rule that sessions should take the baton first gets followed most of the time
and forgotten the rest, and the failure is silent. `baton guard` reads a
PreToolUse payload on stdin and refuses Playwright calls and container-bound
shell commands from a worktree that does not hold the baton.

Add to **`~/.claude/settings.json`** — merge into the existing `PreToolUse`
array rather than replacing it:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash|mcp__playwright__.*",
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/baton guard --container web",
            "timeout": 10,
            "statusMessage": "Checking the baton"
          }
        ]
      }
    ]
  }
}
```

**Use user settings, not `.claude/settings.local.json`.** That file is commonly
gitignored, so it will never exist in a freshly created worktree — the exact
sessions that most need guarding. User settings load everywhere.

Cost is about 8 ms per Bash call, and it fails open immediately outside a git
repository, so it is a no-op in unrelated projects.

**Reverse proxies.** If you reach your app through a proxy on a different host
or port than the container publishes, baton cannot discover that. Declare it:

```
baton guard --container web --pattern 'myapp\.localhost,:8443'
```

Comma-separated regexes, added to the patterns baton builds from the container's
name and published port.

**What it will and will not block.** It fails **open** on infrastructure
trouble — unreadable payload, unresolvable worktree, Docker down, or a container
nobody has queued for. It denies only when the state file positively says
somebody else holds the baton, or when the holder's own tree is not what the
container is serving. A guard that blocked everything whenever it got confused
would be worse than no guard.

baton's own commands are never matched. Guarding them would deadlock a session
that is being told to take the baton.

### Multiple containers

One hook entry per container:

```json
{ "matcher": "Bash|mcp__playwright__.*",
  "hooks": [{ "type": "command", "command": "/usr/local/bin/baton guard --container web" },
            { "type": "command", "command": "/usr/local/bin/baton guard --container api" }] }
```

---

## Other stacks: strategies

Nothing about the queue is stack-specific — it is keyed by container name and
works for any container today. The *swap* is where projects differ, and that
lives behind hooks in `<repo>/.baton/strategy.sh`, sourced by the supervisor
after its own defaults so anything the file defines wins.

| Hook | Default |
| --- | --- |
| `baton_fingerprint` | hash the lockfile, so matching trees share one dependency store |
| `baton_wait_deps` | nothing |
| `baton_prepare` | pnpm / yarn / npm install with a shared `node_modules`, or pip install |
| `baton_migrate` | `alembic upgrade head`, forward only |
| `baton_start` | the stack's usual start command; must not return |
| `baton_health` | HTTP GET the detected port and path |

Hooks run with the worktree as their working directory and these available:

| Variable | |
| --- | --- |
| `BATON_TREE` | the worktree being served |
| `BATON_CODE` | the repository root inside the container |
| `BATON_CONTROL` | `$BATON_CODE/.baton` |
| `BATON_STORE` | shared dependency directory for this tree's fingerprint |
| `BATON_CACHE` | per-tree cache directory |
| `BATON_PORT` | the port to health-check |
| `BATON_STACK` | detected stack |

| Helper | |
| --- | --- |
| `baton_share_into <path>` | bind-mount the shared store here (dependencies) |
| `baton_cache_into <path>` | bind-mount a per-tree directory here (build state) |
| `baton_log <text>` | write to the supervisor log |
| `note <text>` | surface something to a human in `baton status` |

A Python service with a database and a dependency, for example:

```bash
#!/bin/bash
BATON_PORT=5000
BATON_HEALTH_PATH=/_status

baton_wait_deps() {
    while ! nc -z -w1 postgres 5432; do sleep 1; done
}

baton_prepare() {
    pip3 install -r requirements.txt
    pip3 install -e .
}

baton_start() { exec gunicorn --bind 0.0.0.0:$BATON_PORT myapp.wsgi:app; }
```

`baton init` fills in what it can detect: the stack from the lockfile, the port
and health path from the container's own healthcheck (falling back to `PORT` in
its environment), and the command it is replacing — so anything that runner did
beyond installing and starting is visible and can be ported across.

The strategy file runs **as root inside the container on every switch**, exactly
like the runner script it replaces. Keep it in the repo where it gets reviewed.
`baton init` never overwrites one that already exists.

---

## Repos with database migrations

Read this before pointing baton at one.

The database is shared between every worktree while the schemas are not. Switch
to a branch whose migrations are *behind* the database and `upgrade head` is a
silent no-op — that branch then runs against a schema from the future, and
nothing tells you.

baton's rule is **forward automatically, never backward**. When the tree and the
database disagree it records a note, visible in `baton status`:

```
web
  holder    old-branch             2m10s in, 17m50s left
  serving   old-branch             ready
  note      database is at 4f2a1c but this tree expects 9b3d07 — the schema is
            ahead of the branch. Migration-dependent results are not trustworthy.
```

Fixing it properly means a downgrade, which destroys data, so baton will not do
it for you. Treat migration-dependent results from a noted tree as untrustworthy.

The default handles alembic. For anything else define `baton_migrate` and keep
the same rule.

---

## Menu bar app (macOS)

```bash
make menubar-install          # /Applications/Baton.app
```

An agent app: menu bar only, no Dock icon. Add it to Login Items to keep it
around.

In the bar itself, the holder and how many are waiting behind them:

```
◉ pr-4821-review +2     held, two waiting
⚠ pr-4821-review        holds it, but the container is serving someone else
✋ main                  taken by hand, queue frozen
○ free                  nobody has it
```

The popover puts the holder and what the container is *actually serving* in one
card, side by side, because their disagreement is the failure worth catching and
two adjacent lines contradicting each other is easier to notice than two facts
separated by other content. A bar shows how much of the lease is left and turns
orange near the end. Supervisor notes appear inline, warnings in orange and
information in blue. Anything that only matters when something is wrong stays
hidden until it is.

It shells out to `baton status --json` every two seconds rather than reading the
state file, so there is only ever one implementation of the rules.

If it looks wrong, ask it what it sees:

```bash
make menubar-check                # decoding self-check
cd menubar && swift run baton-probe   # what the menu bar is rendering, same code path
```

To review the layout, render every state to PNGs in both appearances — a menu
bar popover cannot be screenshotted without screen recording permission:

```bash
make snapshots                    # writes menubar/snapshots/
```

---

## Command reference

| Command | |
| --- | --- |
| `baton take <container>` | Take it. `--wait` to queue, `--lease` for how long, `--no-swap` to leave the container alone. |
| `baton pass <container>` | Give it back. |
| `baton renew <container>` | Extend before the lease lapses. |
| `baton check <container>` | Exit 0 if it is still yours *and* the container agrees. |
| `baton status [container]` | Holder, what is served, notes, queue. `--json` for scripts. |
| `baton line [container]` | The queue only. |
| `baton grab <container>` | Take over by hand and pin it. `--note` to say why. |
| `baton drop <container>` | Release a hand-taken container. |
| `baton init <container>` | Install the supervisor and a starter strategy. `--dry-run` to look first. |
| `baton install-skill` | Install the Claude Code skill. |
| `baton guard` | PreToolUse hook. Reads a payload on stdin, allows or denies. |

`--tree` names a worktree explicitly on any command; it defaults to the worktree
containing the current directory.

**Exit codes.** `0` yes. `2` no — a real answer, not a failure. `1` something
broke. Anything scripting baton should treat 2 as a normal outcome.

**Environment.** `BATON_HOME` moves the state directory (default `~/.baton`).
`BATON_CODE_MOUNT` overrides code-mount detection.

---

## How it works

**Identity is the worktree path.** Coding agents have no stable process or
session id across their tool calls, but each works in exactly one worktree, so
the path is both stable and readable. One consequence: two sessions in the *same*
worktree count as one holder. They would already be fighting over the same files,
so this is not a new problem, but baton will not save you from it.

**No daemon.** State is one JSON file at `~/.baton/state.json`, guarded by an
flock, written atomically. Every command opens it, mutates it, writes it back.
Contention is a handful of sessions on one machine, so the simplicity is worth
more than the throughput a server would buy.

**No `docker exec` on the hot path.** The repository is bind-mounted, so baton
writes control files on the host and the supervisor inside the container picks
them up. A handoff is a file write. Everything is inspectable with `cat`:

```
<repo>/.baton/
  supervisor.sh     installed by baton init
  strategy.sh       yours to edit
  current-tree      what baton wants served
  serving           what the supervisor is actually serving
  status            starting | ready | failed | stopped
  port              the port the supervisor bound
  notes             things a human should see
  supervisor.log
  store/            shared dependencies, keyed by lockfile hash
  cache/            per-tree build caches
```

**Failure handling.** A closed session's queue entry names its process, and a
dead process is dropped from the line. A closed session's *hold* lapses with its
lease. A swap that fails hands the baton straight back, so a broken tree cannot
block the queue. With Docker down, queue commands still work; only swaps need it.

**Layout.**

```
cmd/baton              entry point
internal/core          queue rules — pure functions, no I/O
internal/store         state file and locking
internal/tree          worktree resolution
internal/docker        container inspection and the swap
internal/supervisor    the in-container script, its hooks, and the skill
internal/cli           subcommands

menubar/
  Sources/BatonCore    CLI client and the menu bar line, no UI
  Sources/BatonMenuBar the SwiftUI agent app
  Sources/BatonProbe   diagnostics and the self-check
```

`internal/core` holds the interesting decisions and has no dependencies, so the
rules can be read and tested on their own. The Go CLI has no external
dependencies; the Swift app has none beyond system frameworks. `baton status
--json` is the only interface between them.

---

## Troubleshooting

**`no bind mount that looks like a git repository`** — the container does not
mount your repo, or mounts it somewhere baton did not recognise. Check
`docker inspect <container>` and set `BATON_CODE_MOUNT` to the path inside the
container.

**`<tree> is outside <root>, so container cannot see it`** — the worktree is not
under the mounted directory. See [Requirements](#requirements); move it under
`.worktrees/`.

**`has no supervisor installed`** — `baton init` ran but the container was not
recreated, so it is still running its old command. Recreate rather than restart.

**`bind mount failed`** in the supervisor log — the container is not
`privileged`. Either add it, or define `baton_prepare` in your strategy without
`baton_share_into` so each tree installs its own dependencies.

**Swaps time out** — read `<repo>/.baton/supervisor.log`. Usually the health
check is wrong: `BATON_PORT` or `BATON_HEALTH_PATH` in the strategy does not
match what the app actually serves.

**Status disagrees with reality** — `baton status` compares its bookkeeping
against the container and flags drift. Trust the container. `baton take` again
to force a swap.

---

## Known limitations

**Multi-container deadlock.** A change spanning two containers needs both
batons, and two sessions taking them in opposite orders will deadlock until the
leases lapse. There is no atomic multi-take yet. Until there is, agree an order.

**The guard is a net, not a boundary.** Any command mentioning `baton` is
treated as coordinating, so `baton status && npx playwright test` slips through.
Narrowing that would break `baton take --wait && npm run e2e`, which is the
pattern sessions are told to use. It catches forgetfulness, not evasion.

**One holder per worktree, not per session.** Two sessions in the same worktree
are indistinguishable to baton.

**Shared databases are only flagged, not solved.** See
[migrations](#repos-with-database-migrations).

**No Windows support.** The supervisor is bash and assumes a Linux container;
the CLI uses Unix file locking.

---

## Contributing

Issues and pull requests welcome. Before opening a PR:

```bash
make check                          # gofmt, go vet, go test
cd menubar && swift build && ./.build/debug/baton-probe --selftest
```

Two things worth knowing about the codebase. `internal/core` holds the queue
rules as pure functions with no I/O, so behaviour changes belong there and are
cheap to test. And the JSON from `baton status --json` is the contract between
the Go CLI and the Swift app — change its shape and the menu bar's self-check
will tell you.

---

## License

[Apache License 2.0](LICENSE).

Permissive, so anyone can use it at work without a legal review, and it carries
an explicit patent grant and contribution terms that MIT leaves unsaid — which
matters when contributors are employed somewhere.
