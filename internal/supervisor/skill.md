---
name: baton
description: Take the shared dev container before running functional tests, and give it back after. Use whenever about to run Playwright, e2e tests, or anything that hits a running dev server (a local dev URL) — several sessions share one container across git worktrees, and running without holding it produces results against someone else's branch. Also use when a container swap seems needed, when a test result looks wrong for the branch, or when asked who has the container.
---

# baton — sharing one dev container across worktrees

> Replace `<container>` throughout with the container this project shares, for
> example `web` or `api`. If you share more than one, list each.

Several sessions work in different git worktrees but share a single Docker dev
container. Only one worktree can be served at a time. baton is the queue that
decides who gets it.

**If `baton` is not on PATH, this does not apply — carry on normally.**

```bash
command -v baton >/dev/null || echo "baton not installed; ignore this skill"
```

## The rule

Take it for the test window only. Not for the whole session.

```bash
baton take <container> --wait     # blocks until the container serves YOUR tree
# ... run your tests ...
baton check <container>           # exit 0 = still yours, 2 = throw the results away
# ... close the Playwright browser ...
baton pass <container>            # give it back
```

Generating code does not need the container. Hold it while testing, hand it
back while writing. Handoffs cost about 30 seconds, so passing it back between
iterations is cheap and keeps everyone else moving.

The Playwright browser is shared across sessions the same way the container is,
but nothing queues it. Close it in the same breath as passing the baton —
including when you stop early or are told to stop.

## How to wait properly

`--wait` blocks until the baton is yours *and* the container is actually
serving your worktree. That can take a while if others are in line, so run it
as a **background** command and let the completion notification wake you:

```bash
baton take <container> --wait --timeout 30m
```

When it exits successfully the container is ready and serving your tree. Do not
poll it in a loop, and do not start testing before it returns.

If you cannot background it, `baton take <container>` without `--wait` returns
immediately: exit 0 means you got it, exit 2 means you did not. Report the
queue position to the user rather than silently proceeding.

## The part that actually matters

Holding the lock is not the same as the container serving your code. A swap can
fail, or a human can take over mid-run. So:

**After every test batch, run `baton check <container>`. If it exits 2, discard
those results and say so.** They were produced against a different branch. This
is the entire reason the tool exists — the failure is silent otherwise, because
the tests run perfectly well, just on the wrong code.

## When you are refused

A PreToolUse hook blocks Playwright and container-bound commands from a
worktree that does not hold the baton. If you see a refusal, it is telling the
truth. Take the baton and retry; do not try to work around it.

If the message says the container is **pinned by hand**, a human has taken over
deliberately. Wait, or tell the user you are blocked. Never run `baton drop` to
release someone else's hold — only they should do that.

## Reading the room

```bash
baton status <container>    # holder, what is being served, who is waiting
baton line <container>      # just the queue
```

`serving` disagreeing with `holder` means a swap is in flight or has failed.

## Commands

| Command | What it does |
| --- | --- |
| `baton take <container> --wait` | Queue for it, then take it. Run backgrounded. |
| `baton pass <container>` | Give it back. |
| `baton renew <container>` | Extend before the lease lapses (default 20 min). |
| `baton check <container>` | Exit 0 if still yours and the container agrees. |
| `baton status [container]` | Holder, serving, queue. `--json` for scripts. |
| `baton line [container]` | The queue only. |
| `baton grab` / `baton drop` | Human override. **Not for you to run.** |

`--tree` names a worktree explicitly; it defaults to the current directory.
Exit code 2 always means "no". Exit 1 means something broke.

## Do not

- Do not run `baton grab` or `baton drop`. Those are the human's override.
- Do not hold the baton while writing code. Pass it back.
- Do not report test results without a passing `baton check` first.
- Do not disable or bypass the guard hook.

Full documentation: the baton repository's README.
