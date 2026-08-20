# Walkthrough

Ten minutes, a throwaway project, and you will have watched a container hand
itself between two branches.

Everything lands in one directory you delete afterwards. Nothing touches
anything you already have.

**You need:** Docker running, `baton` installed (`make install` in the repo
root), and about 50 MB of disk for the `python:3-slim` image.

---

## Set up a demo project

```bash
./examples/walkthrough/setup.sh
```

That creates `/tmp/baton-demo` containing:

```
app/                        a git repo, branch main
app/.worktrees/
  feature-blue/             a worktree, branch feature-blue
  feature-green/            a worktree, branch feature-green
app/serve.py                a five-line HTTP server
app/.baton/strategy.sh      how to start it (see below)
docker-compose.yml          one container on port 18080
```

Each branch serves a page saying its own name, so you can see at a glance which
worktree the container is on.

**Note where the worktrees are.** They live *inside* `app/`, which is what gets
bind-mounted. That is a requirement, not a style choice: the container can only
see what is under the mount, so a worktree elsewhere is invisible to it.

```bash
cd /tmp/baton-demo
docker compose up -d
curl -s localhost:18080 | grep '<h1>'
```

```
<h1>main</h1>
```

An ordinary container serving an ordinary repo. No baton yet.

---

## 1. Look before you leap

```bash
baton init baton-demo --dry-run
```

```
container   baton-demo
code root   /tmp/baton-demo/app
stack       pip
health      port 18080, path /
migrations  false
command     python3 serve.py
```

It found the repo by looking for the bind mount whose host side is a git
repository, worked out the stack from `requirements.txt`, and took the port from
the container's environment. `--dry-run` writes nothing.

### About that strategy file

`setup.sh` wrote `app/.baton/strategy.sh` for you, containing one line:

```bash
baton_start() { exec python3 serve.py; }
```

That is there because **Python has no universal start command**. baton has
defaults for installing (`pip install -r requirements.txt`) and for health
checking, but no honest guess at how your app starts, so it asks. A Node project
would not need this — `pnpm start` and friends are guessable.

This is the whole strategy mechanism: the supervisor sources the file after its
own defaults, so anything you define wins and everything you leave out falls
back.

---

## 2. Install the supervisor

```bash
baton init baton-demo
```

```
baton: installed the supervisor in /tmp/baton-demo/app/.baton
baton: kept the existing strategy at /tmp/baton-demo/app/.baton/strategy.sh
baton: wrote /tmp/baton-demo/docker-compose.override.yml
```

It kept the strategy rather than clobbering it — `init` never overwrites one
that exists. The override it wrote is small:

```yaml
services:
  demo:
    command: /code/.baton/supervisor.sh
    environment:
      - BATON_CODE=/code
```

Note it is keyed by the compose **service** (`demo`), not the container name
(`baton-demo`). Those differ here, as they often do.

Now **recreate** the container. A restart is not enough — the `command` has to
change:

```bash
docker compose up -d --force-recreate
baton status baton-demo
```

```
baton-demo
  holder    free
  serving   main                   ready
  queue     empty
```

This is the only restart. From here the container stays up and only the app
moves.

---

## 3. Take it

```bash
cd app/.worktrees/feature-blue
baton take baton-demo
```

```
baton: switching baton-demo to feature-blue
baton: baton-demo is yours (feature-blue), serving on port 18080
```

```bash
curl -s localhost:18080 | grep '<h1>'
```

```
<h1>blue</h1>
```

The container is now serving the `feature-blue` worktree. It never restarted.

---

## 4. Watch the other branch get refused

```bash
cd ../feature-green
baton take baton-demo
```

```
baton: baton-demo is held by feature-blue; feature-green is not queued (pass --wait to join the line)
```

Exit code `2` — a real answer, not an error. Note it did **not** silently join a
queue it was not going to wait in.

---

## 5. Wait in line properly

```bash
baton take baton-demo --wait --timeout 3m &
sleep 3
baton line baton-demo
```

```
baton-demo
  queue     1. feature-green       waiting 3s
```

`--wait` is the one to background. It sits in the queue and, when it wins, does
the swap and exits — so its exit *is* the signal that the container is ready and
serving your tree.

---

## 6. The handoff

In another shell, give it back:

```bash
cd /tmp/baton-demo/app/.worktrees/feature-blue
baton pass baton-demo
```

```
baton: passed baton-demo; feature-green is up next
```

The backgrounded waiter wakes up on its own:

```
baton: baton-demo is held by feature-blue; feature-green is #1 in line
baton: waiting for baton-demo (timeout 3m0s)
baton: switching baton-demo to feature-green
baton: baton-demo is yours (feature-green), serving on port 18080
```

```bash
curl -s localhost:18080 | grep '<h1>'
```

```
<h1>green</h1>
```

About five seconds end to end on this toy. On a large React app with a warm build cache
it is about thirty. That number is the whole design constraint: if handoffs were
slow nobody would give the container back between test runs.

---

## 7. Take it over by hand

```bash
baton grab baton-demo --note "looking at something"
baton status baton-demo
```

```
baton-demo
  holder    main                   held by hand for 2s — looking at something
  serving   main                   ready
  queue     empty
```

`grab` displaced the holder, switched the container back to your main clone, and
**pinned** it. Anyone in the queue stays in the queue and stops advancing.

The displaced session finds out when it asks:

```bash
cd app/.worktrees/feature-green
baton check baton-demo
```

```
baton: no — baton-demo is held by main; discard any results from this tree
```

Exit `2`. **This is the habit that matters**: run `baton check` after every batch
of tests, and if it says no, throw the results away. They ran fine — against
somebody else's code.

Give it back:

```bash
baton drop baton-demo
```

---

## 8. See the failure it exists to prevent

Take the baton but tell baton not to move the container:

```bash
cd app/.worktrees/feature-blue
baton take baton-demo --no-swap
baton status baton-demo
```

```
baton-demo
  holder    feature-blue           1s in, 19m59s left
  serving   main                   ready  ** not the holder's tree **
```

You hold the lock. The container is serving something else. Any test you run now
passes or fails convincingly and tells you nothing about your branch.

```bash
baton check baton-demo
```

```
baton: no — feature-blue holds the lock but baton-demo is not serving it; discard any results
```

A plain lock file would have said yes. That gap — between holding a lock and the
container actually following it — is the reason this tool is not fifty lines of
`flock`.

---

## Clean up

```bash
cd /tmp/baton-demo && docker compose down
cd / && rm -rf /tmp/baton-demo
baton status                        # the demo entry lingers harmlessly
```

To forget it entirely, delete `~/.baton/state.json`.

---

## What to try next

- Add a third worktree and watch a two-deep queue drain.
- Kill a waiting `baton take --wait` and see its queue entry disappear.
- `baton take baton-demo --lease 20s`, wait, and watch the hold lapse on its own.
- Edit `app/.baton/strategy.sh` to add a `baton_wait_deps` that sleeps, then
  recreate and watch the supervisor log.

The supervisor narrates everything it does:

```bash
tail -f /tmp/baton-demo/app/.baton/supervisor.log
```
