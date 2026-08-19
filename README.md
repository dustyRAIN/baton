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
baton init cmp-client
```

That installs the supervisor, hides its files from git, and writes a
`docker-compose.override.yml` pointing the container at it. One restart picks it
up; after that the container stays up and only the dev server moves.

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
| `baton init <container>` | Install the supervisor. |

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
internal/supervisor    the script that runs inside the container
```

`internal/core` is where the interesting decisions live and it has no
dependencies, so the rules can be read and tested on their own.
