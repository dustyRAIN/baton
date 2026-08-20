# baton

One Docker container, many git worktrees, one at a time.

If you work on several branches at once — each in its own worktree, each with an
agent or a person driving it — they all need the same dev container to run
functional tests in. Only one can have it. baton is the queue that decides who,
and hands it over cleanly when they are done.

```
$ baton status cmp-client
cmp-client
  holder    pr-12254-head          4m12s in, 15m48s left
  serving   pr-12254               ready
  queue     1. pr-12277-review     waiting 2m10s
            2. CCP-18046           waiting 30s
```

## Why it is not just a lock file

Two things make this harder than a mutex.

**The container has to follow the baton.** Taking the lock is useless if the dev
server is still serving somebody else's branch — your tests pass against their
code and you never find out. So a grant is not reported until the container is
actually serving your tree and answering on its port.

**Handoffs have to be cheap.** If switching trees costs five minutes, nobody
gives the baton back between test runs and the queue seizes up. baton avoids
restarting the container at all: a supervisor inside it moves the dev server
between worktrees, sharing one `node_modules` per lockfile and keeping a build
cache per tree.

## Install

```bash
make install            # /usr/local/bin/baton
```

Then, once per container:

```bash
baton init cmp-client --dry-run   # see what it detected, change nothing
baton init cmp-client
```

That installs the supervisor, writes a starter strategy for whatever stack it
detected, hides its files from git, and points the container at the supervisor
via `docker-compose.override.yml`. One restart picks it up; after that the
container stays up and only the app moves.

## Other repos and other stacks

Nothing about the queue is stack-specific — it is keyed by container name and
works for any of them today. The *swap* is where repos differ, and that lives
behind hooks in `.baton/strategy.sh`, sourced by the supervisor after its own
defaults so anything the file defines wins.

| Hook | Default |
| --- | --- |
| `baton_fingerprint` | hash the lockfile, so trees that match can share a store |
| `baton_wait_deps` | nothing |
| `baton_prepare` | pnpm/yarn/npm install with a shared `node_modules`, or pip install |
| `baton_migrate` | `alembic upgrade head`, refusing to go backwards |
| `baton_start` | the stack's usual start command |
| `baton_health` | HTTP GET the detected port and path |

`baton init` fills in what it can detect: the stack from the lockfile, the port
and health path from the container's own healthcheck (falling back to `PORT` in
the environment), and the command it is replacing so anything else that runner
did can be ported over.

Helpers available to a strategy: `baton_share_into <path>` for a dependency
directory shared between trees with the same lockfile, `baton_cache_into <path>`
for build state that must stay per-tree, `baton_log`, and `note` for something a
human should see in `baton status`.

The file runs as root in a privileged container on every switch, exactly like
the runner script it replaces. Keep it in the repo so it gets reviewed.

### Repos with migrations

Worth knowing before you point baton at one. The database is shared between
every worktree while the schemas are not, so switching to a branch whose
migrations are *behind* the database gets a silent no-op from `upgrade head` and
then runs against a schema from the future.

The default goes forward automatically, never backward, and records a note —
visible in `baton status` — when the tree and the database disagree. Treat
migration-dependent results from a noted tree as untrustworthy. Fixing it
properly means a downgrade, which destroys data, so baton will not do it for
you.

## Everyday use

```bash
baton take cmp-client --wait      # block until it is yours, then run tests
baton check cmp-client            # still mine? exit 0 yes, 2 no
baton pass cmp-client             # give it back
```

Take it for the test run, not for the whole session. Generating code does not
need the container — pass it back and take it again when you next need to run
something.

`--wait` is the one to run in the background. It sits in line, and when it wins
it performs the swap and exits, which is your signal that the container is ready
and serving your tree.

## Taking over by hand

```bash
baton grab cmp-client --note "looking at something"
baton drop cmp-client
```

`grab` displaces whoever holds it, switches the container back to your main
clone, and pins it. Sessions in the queue stay in the queue and stop advancing.
Nothing moves until you `drop`.

A session that was displaced finds out the next time it runs `baton check`,
which is why the rule matters: **check after every test batch, and throw the
results away if the answer is no.**

## Making it enforced rather than polite

A rule that sessions should take the baton first gets followed most of the time
and forgotten the rest, and the failure is silent — the tests pass, just against
the wrong branch. `baton guard` closes that gap as a Claude Code PreToolUse hook.
Add to `.claude/settings.local.json`:

```json
"hooks": {
  "PreToolUse": [
    {
      "matcher": "Bash|mcp__playwright__.*",
      "hooks": [
        {
          "type": "command",
          "command": "/usr/local/bin/baton guard --container cmp-client",
          "timeout": 10,
          "statusMessage": "Checking the baton"
        }
      ]
    }
  ]
}
```

It refuses Playwright calls and container-bound shell commands from a worktree
that does not hold the baton, and tells the caller what to run instead.

It fails **open** on infrastructure trouble and **closed** only on a real
violation. An unreadable payload, an unresolvable worktree, an unreachable
Docker daemon, or a container nobody has queued for all let the call through. It
denies only when the state file positively says somebody else holds the baton,
or when the holder's own tree is not what the container is serving. A guard that
blocked everything whenever it got confused would be worse than no guard.

baton's own commands are never matched — guarding them would deadlock a session
that is being told to take the baton.

## Menu bar app

```bash
make menubar-install      # /Applications/Baton.app
```

An agent app: menu bar only, no Dock icon. It shows the current holder and how
many are waiting, and gives you Take over / Release without a terminal.

```
◉ pr-12254-head +2      held, two waiting
⚠ pr-12254-head         holds the lock, container is serving someone else
✋ CCP-17161-re          taken by hand, queue frozen
○ free                  nobody has it
```

It shells out to `baton status --json` every two seconds rather than reading the
state file, so there is only ever one implementation of the rules.

If it looks wrong, ask it what it sees:

```bash
cd menubar
swift run baton-probe              # what the menu bar is rendering, same code path
swift run baton-probe --selftest   # check the decoding against captured CLI output
```

The self-check is a plain binary rather than XCTest so it runs on a machine with
Command Line Tools and no Xcode, where SwiftPM cannot see XCTest.

## Commands

| Command | What it does |
| --- | --- |
| `baton take <container>` | Take the baton. `--wait` to queue, `--lease` to set how long. |
| `baton pass <container>` | Give it back. |
| `baton renew <container>` | Extend your hold before the lease lapses. |
| `baton check <container>` | Exit 0 if you still hold it and the container agrees, 2 otherwise. |
| `baton status [container]` | Holder, what is being served, and the queue. `--json` for scripts. |
| `baton line [container]` | Just the queue. |
| `baton grab <container>` | Take over by hand and pin it. |
| `baton drop <container>` | Release a hand-taken baton. |
| `baton init <container>` | Install the supervisor and a starter strategy. `--dry-run` to look first. |
| `baton guard` | PreToolUse hook. Reads a payload on stdin, allows or denies. |

Every command takes `--tree` to name a worktree. It defaults to the worktree
containing the current directory.

Exit code `2` always means "the answer is no". `1` means something broke.

## How it decides who is who

A worktree path is the identity. Sessions do not have stable process ids across
their tool calls, but each one works in exactly one worktree, so the path is
both stable and readable.

That has a consequence worth knowing: two sessions in the *same* worktree count
as one holder. They would already be fighting over the same files, so this is
not a new problem, but baton will not save you from it.

## What happens when things die

- **A session closed while waiting** — its queue entry names its process, and a
  dead process is dropped from the line.
- **A session closed while holding** — the hold has a lease and lapses on its
  own. Default 20 minutes; `renew` pushes it out.
- **A swap that fails** — the baton is handed straight back, so a broken tree
  cannot block everyone behind it.
- **Docker down** — the queue still reads and writes. Only swaps need Docker.

Human holds never expire. That is deliberate: if you took it by hand, only you
should give it back.

## State

One JSON file at `~/.baton/state.json`, guarded by an flock. No daemon. Set
`BATON_HOME` to point somewhere else.

## Layout

```
cmd/baton              entry point
internal/core          queue rules — pure functions, no I/O
internal/store         state file and locking
internal/tree          worktree resolution
internal/docker        container inspection and the swap
internal/supervisor    the script that runs inside the container, and its hooks
internal/cli           subcommands, including the guard hook

menubar/
  Sources/BatonCore    CLI client and the menu bar line, no UI
  Sources/BatonMenuBar the SwiftUI agent app
  Sources/BatonProbe   diagnostics and the self-check
```

`internal/core` is where the interesting decisions live and it has no
dependencies, so the rules can be read and tested on their own.

The Go CLI has no external dependencies and the Swift app has none beyond the
system frameworks. `baton status --json` is the only interface between them.
