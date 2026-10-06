#!/bin/bash
# baton supervisor — runs as the container's command in place of its usual
# runner script.
#
# It keeps the container up permanently and moves only the app between
# worktrees. baton writes the worktree it wants into .baton/current-tree on the
# host; this loop notices, stops what is running, and starts it again in the
# requested tree. The container itself never restarts, which is what turns a
# handoff from minutes into seconds.
#
# Everything stack-specific lives behind hooks. The defaults below cover pnpm,
# yarn and pip; a repo overrides any of them by defining the same function in
# .baton/strategy.sh, which is sourced after the defaults. That file runs as
# root in a privileged container, exactly like the runner script it replaces,
# so it belongs in the repo where it gets reviewed.
#
#   baton_fingerprint   identifies the dependency set, for keying shared stores
#   baton_wait_deps     block until other services are reachable
#   baton_prepare       install dependencies, mount caches
#   baton_migrate       bring schemas in line
#   baton_start         exec the server (must not return)
#   baton_health        exit 0 once the server is answering
#
# Hooks run with the tree as their working directory and these in the
# environment: BATON_TREE BATON_CODE BATON_CONTROL BATON_STORE BATON_CACHE
# BATON_PORT BATON_STACK. Helpers available to them: baton_log,
# baton_share_into, baton_cache_into.

set -uo pipefail

# Where the repository is mounted. baton's compose override sets this from what
# it detected; /code is only the fallback for a hand-written setup.
BATON_CODE="${BATON_CODE:-/code}"
BATON_CONTROL="$BATON_CODE/.baton"
CURRENT_FILE="$BATON_CONTROL/current-tree"
SERVING_FILE="$BATON_CONTROL/serving"
STATUS_FILE="$BATON_CONTROL/status"
PORT_FILE="$BATON_CONTROL/port"
RESTART_FILE="$BATON_CONTROL/restart"
NOTES_FILE="$BATON_CONTROL/notes"
LOG_FILE="$BATON_CONTROL/supervisor.log"
STORE_ROOT="$BATON_CONTROL/store"
CACHE_ROOT="$BATON_CONTROL/cache"
TRASH_ROOT="$BATON_CONTROL/trash"
STRATEGY_FILE="$BATON_CONTROL/strategy.sh"

READY_TIMEOUT="${BATON_READY_TIMEOUT:-900}"

mkdir -p "$BATON_CONTROL" "$STORE_ROOT" "$CACHE_ROOT"

child_pid=""
serving_tree=""
serving_fingerprint=""

export BATON_CODE BATON_CONTROL

# ---------------------------------------------------------------- plumbing

# Hooks run with stdout redirected into the log file, so a plain tee would write
# every line twice. Keep the real stdout on fd 3 and send the console copy there.
exec 3>&1
baton_log() {
    local line
    line=$(printf '%s baton-supervisor: %s' "$(date -u +%H:%M:%S)" "$*")
    printf '%s\n' "$line" >>"$LOG_FILE"
    printf '%s\n' "$line" >&3
}

set_status() { printf '%s\n' "$1" >"$STATUS_FILE"; }
set_serving() { printf '%s\n' "$1" >"$SERVING_FILE"; }

# note and warn record something a human should see in `baton status`.
#
# The split matters: "I applied migrations" is worth knowing, while "your schema
# is ahead of this branch" means the results you are about to collect are not
# trustworthy. Colouring both the same would flatten that.
note() { record_note info "$*"; }
warn() { record_note warning "$*"; }

record_note() {
    local level="$1"; shift
    printf '%s\t%s\t%s\n' "$(date -u +%H:%M:%S)" "$level" "$*" >>"$NOTES_FILE"
    baton_log "${level^^}: $*"
}

clear_notes() { : >"$NOTES_FILE"; }

# is_mounted checks /proc rather than shelling out to mountpoint, which is not
# guaranteed to be installed in every base image.
is_mounted() { grep -qs " $1 " /proc/self/mounts; }

tree_slug() {
    local slug
    slug=$(printf '%s' "${1#"$BATON_CODE"}" | sed 's#^/##; s#^\.##; s#/#_#g')
    [ -z "$slug" ] && slug="main"
    printf '%s' "$slug"
}

# detect_stack picks a default recipe from what the tree contains. A strategy
# file that defines its own hooks makes this irrelevant.
detect_stack() {
    local tree="$1"
    if [ -f "$tree/pnpm-lock.yaml" ]; then echo pnpm
    elif [ -f "$tree/yarn.lock" ]; then echo yarn
    elif [ -f "$tree/package-lock.json" ]; then echo npm
    elif [ -f "$tree/requirements.txt" ] || [ -f "$tree/setup.py" ]; then echo pip
    else echo unknown
    fi
}

hash_file() {
    if [ -f "$1" ]; then sha256sum "$1" | cut -c1-12; else echo nolock; fi
}

# ---------------------------------------------------------------- helpers for strategies

# baton_share_into bind mounts the shared dependency store for this tree's
# fingerprint at the given path. This is what lets several worktrees with the
# same lockfile avoid a full install each — worth it when the directory is
# large, which is why it is opt-in per strategy rather than automatic.
baton_share_into() {
    local target="$1"

    # Worktrees are often set up by hand with a dependency symlink pointing at
    # the main clone. Those come in relative, /code-absolute and host-absolute
    # flavours, and only the relative one resolves on both sides of the
    # container boundary. They are also frequently wrong, pointing a tree with
    # its own lockfile at the main clone's dependencies. Replace them.
    if [ -L "$target" ]; then
        local previous
        previous=$(readlink "$target")
        baton_log "replacing the symlink at $target (was -> $previous)"
        printf '%s\t%s\n' "$target" "$previous" >>"$BATON_CONTROL/replaced-symlinks.log"
        rm -f "$target"
    fi

    mkdir -p "$target" "$BATON_STORE"
    if is_mounted "$target"; then return 0; fi

    # Anything already in the directory is an install of the tree's own, made
    # outside baton. The mount hides it without freeing it, so a full copy of
    # the dependencies would sit on disk where nobody looks. The main clone is
    # the exception: its directory is the adopted store itself.
    if [ "$(readlink -f "$target")" != "$BATON_STORE" ] && [ -n "$(ls -A "$target" 2>/dev/null)" ]; then
        warn "$target already holds its own install, which the shared store is about to hide." \
            "It is a full copy of the dependencies taking disk space for nothing. Delete it from the host."
    fi

    local error
    baton_log "sharing $BATON_STORE into $target"
    if ! error=$(mount --bind "$BATON_STORE" "$target" 2>&1); then
        baton_log "bind mount failed: $error"
        return 1
    fi
}

# baton_cache_into gives this tree its own persistent build cache at the given
# path, even when that path sits inside a shared store. Without it, trees
# sharing dependencies would also share one cache directory and thrash it.
baton_cache_into() {
    local target="$1"
    mkdir -p "$BATON_CACHE" "$target"
    if is_mounted "$target"; then return 0; fi
    baton_log "mounting per-tree cache at $target"
    mount --bind "$BATON_CACHE" "$target" || baton_log "cache mount failed, continuing with a shared cache"
}

# ---------------------------------------------------------------- default hooks

# baton_fingerprint identifies the dependency set so trees that share one can
# share a store. Defaults to hashing the lockfile for the detected stack.
baton_fingerprint() {
    case "$BATON_STACK" in
        pnpm) hash_file "$BATON_TREE/pnpm-lock.yaml" ;;
        yarn) hash_file "$BATON_TREE/yarn.lock" ;;
        npm) hash_file "$BATON_TREE/package-lock.json" ;;
        pip) hash_file "$BATON_TREE/requirements.txt" ;;
        *) echo nolock ;;
    esac
}

# baton_wait_deps blocks until other services this one needs are reachable.
# No-op by default; repos with dependencies override it.
baton_wait_deps() { :; }

# baton_prepare installs dependencies for the tree.
baton_prepare() {
    case "$BATON_STACK" in
        pnpm | yarn | npm)
            # node_modules is large enough that sharing it across trees with the
            # same lockfile is the difference between a one-second install and a
            # multi-minute one.
            baton_share_into "$BATON_TREE/node_modules" || return 1
            case "$BATON_STACK" in
                pnpm) pnpm install --frozen-lockfile ;;
                yarn) yarn install --frozen-lockfile ;;
                npm) npm ci ;;
            esac || return 1
            # Everything under node_modules/.cache is build state, not
            # dependencies — bundlers, babel, eslint all live there. Sharing
            # node_modules would otherwise make trees thrash each other's
            # caches, so this one directory stays per-tree.
            baton_cache_into "$BATON_TREE/node_modules/.cache"
            ;;
        pip)
            # Python installs into the image's system site-packages, not into
            # the tree, so there is no per-tree directory to share. The editable
            # install is global and points at one tree at a time, which is why
            # it has to be repeated on every switch rather than cached.
            if [ -f requirements.txt ]; then
                pip3 install -r requirements.txt || return 1
            fi
            # Written as an if rather than `[ -f a ] || [ -f b ] && install`,
            # which exits non-zero when neither file is present and would fail
            # prepare for every Python repo that has no packaging metadata.
            if [ -f setup.py ] || [ -f pyproject.toml ]; then
                pip3 install -e . || return 1
            fi
            ;;
        *)
            baton_log "no default prepare for an unrecognised stack; define baton_prepare in strategy.sh"
            return 1
            ;;
    esac
}

# baton_migrate brings schemas in line with the tree. No-op unless the stack
# has migrations. See check_migration_drift for why this is handled carefully.
baton_migrate() {
    if [ "$BATON_STACK" = "pip" ] && [ -f "$BATON_TREE/alembic.ini" ]; then
        alembic_migrate
    fi
}

# baton_start runs the server. It must not return.
baton_start() {
    case "$BATON_STACK" in
        pnpm) exec ./scripts/start.sh ;;
        yarn) exec yarn start ;;
        npm) exec npm start ;;
        pip)
            baton_log "python has no universal start command — define baton_start in $STRATEGY_FILE"
            return 1
            ;;
        *)
            baton_log "no default start for stack '$BATON_STACK' — define baton_start in $STRATEGY_FILE"
            return 1
            ;;
    esac
}

# baton_health exits 0 once the server is answering.
#
# Slim base images frequently ship neither curl nor wget, so falling back to a
# bare TCP probe matters — without it the supervisor never reports ready even
# though the app is up, and every take hangs until it times out. The probe only
# proves the port is open, which is why it is the last resort.
baton_health() {
    [ -z "$BATON_PORT" ] && return 0
    local url="http://127.0.0.1:$BATON_PORT${BATON_HEALTH_PATH:-/}"

    if command -v curl >/dev/null 2>&1; then
        curl -sf -o /dev/null --max-time 3 "$url"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O /dev/null -T 3 "$url"
    else
        (exec 9<>"/dev/tcp/127.0.0.1/$BATON_PORT") 2>/dev/null
    fi
}

# ---------------------------------------------------------------- migrations

# alembic_migrate upgrades the schema, but refuses to move the database
# backwards.
#
# The database is shared between every worktree while the schemas are not. A
# tree whose migrations are behind the database gets a silent no-op from
# `upgrade head` and then runs against a schema from the future, and the only
# way to actually match it would be a downgrade, which destroys data. So the
# rule is: go forward automatically, never backward, and say loudly when the
# tree and the database disagree.

# alembic_revision runs an alembic subcommand and returns just the revision.
#
# Taking the first line is not safe. alembic prints the revision on stdout, and
# so does whatever logging the project's env.py installs — one repo here writes
# ANSI-coloured lines around every migration call, so the first line was a log
# timestamp. That never matched the head, and baton warned about a schema drift
# that did not exist, on every single switch.
#
# So: strip colour, then take the first line that is nothing but a revision,
# optionally followed by alembic's "(head)" marker. Anything unrecognised
# yields an empty string, which the caller already treats as "do not compare"
# — a missing warning is recoverable, a false one teaches people to ignore all
# of them.
alembic_revision() {
    alembic "$1" 2>/dev/null | awk -v esc="$(printf '\033')" '
        { gsub(esc "\\[[0-9;]*[a-zA-Z]", "") }
        /^[0-9a-zA-Z_]+( \(head[^)]*\))?[ \t]*$/ { print $1; exit }
    '
}

alembic_migrate() {
    local before after
    before=$(alembic_revision current)

    if ! alembic upgrade head >>"$LOG_FILE" 2>&1; then
        baton_log "alembic upgrade failed"
        return 1
    fi

    after=$(alembic_revision current)
    local head
    head=$(alembic_revision heads)

    if [ -n "$head" ] && [ -n "$after" ] && [ "$after" != "$head" ]; then
        warn "database is at $after but this tree expects $head — the schema is ahead of the branch. Migration-dependent results are not trustworthy. Fixing it means a downgrade, which is destructive, so baton will not do it."
    elif [ -n "$before" ] && [ "$before" != "$after" ]; then
        baton_log "migrated $before -> $after (shared database, other trees are affected)"
        note "applied migrations $before -> $after. Other worktrees share this database."
    fi
}

# ---------------------------------------------------------------- garbage collection

# Stores are keyed by lockfile, and caches by tree, so both outlive what they
# were made for: every lockfile change leaves a full copy of the old
# dependencies behind, and every removed worktree leaves its build cache. Left
# alone they grow until the disk fills, at which point Docker Desktop stops
# the VM. After each successful switch, anything no live tree can use goes.

# live_trees prints every tree the container can serve: the main clone and each
# linked worktree git still knows about.
#
# A worktree records its location as a host path. Inside the container that
# only resolves once rewritten onto BATON_CODE, and without BATON_HOST_CODE the
# rewrite is guesswork — a tree that looks gone may just be unreadable — so it
# fails and the caller keeps everything.
live_trees() {
    [ -d "$BATON_CODE/.git" ] || return 1
    printf '%s\n' "$BATON_CODE"

    local gitdir path
    for gitdir in "$BATON_CODE"/.git/worktrees/*/gitdir; do
        [ -f "$gitdir" ] || continue
        path=$(cat "$gitdir")
        path="${path%/.git}"
        if [[ "$path" == "$BATON_CODE"/* ]]; then
            :
        elif [ -n "${BATON_HOST_CODE:-}" ] && [[ "$path" == "$BATON_HOST_CODE"/* ]]; then
            path="$BATON_CODE${path#"$BATON_HOST_CODE"}"
        elif [ -n "${BATON_HOST_CODE:-}" ]; then
            # Outside the code root, so the container could never serve it.
            continue
        else
            return 1
        fi
        [ -d "$path" ] && printf '%s\n' "$path"
    done
    return 0
}

# release_mounts lazily unmounts everything bound from a store or cache.
#
# Trees keep their mounts after the app moves on, and baton_share_into reuses a
# mount it finds, so a tree still bound to a discarded store would go on using
# it. mountinfo's fourth field is the source path within its filesystem, which
# for a bind of a shared directory ends in the path under the code root.
release_mounts() {
    local suffix="${1#"$BATON_CODE"}" root point
    [ -r /proc/self/mountinfo ] || return 0
    while read -r _ _ _ root point _; do
        root=$(printf '%b' "$root")
        case "$root" in
            *"$suffix" | *"$suffix"/*)
                point=$(printf '%b' "$point")
                baton_log "unmounting $point"
                umount -l "$point" 2>/dev/null
                ;;
        esac
    done </proc/self/mountinfo
}

# discard takes an entry out of use at once and deletes it in the background.
#
# A store is hundreds of thousands of files on a shared filesystem, and
# deleting one in the foreground would hold up the next switch for minutes. A
# rename within the control directory is instant.
discard() {
    local entry="$1" why="$2"
    release_mounts "$entry"
    if [ -L "$entry" ]; then
        # An adopted main clone: drop the link, never what it points at.
        rm -f "$entry" || return 1
    else
        mkdir -p "$TRASH_ROOT"
        mv "$entry" "$TRASH_ROOT/${entry##*/}.$(date +%s).$$" || return 1
    fi
    baton_log "discarded $why"
}

empty_trash() {
    [ -d "$TRASH_ROOT" ] || return 0
    [ -n "$(ls -A "$TRASH_ROOT" 2>/dev/null)" ] || return 0
    rm -rf "${TRASH_ROOT:?}"/* &
}

collect_garbage() {
    [ "${BATON_GC:-1}" = "0" ] && return 0

    local trees
    if ! trees=$(live_trees); then
        baton_log "skipping garbage collection: worktree paths cannot be mapped into the container"
        return 0
    fi

    # The serving tree keeps the store it started with even if its lockfile
    # has changed since: the running app is using it.
    local keep_stores=" $serving_fingerprint " keep_caches=" " tree
    while IFS= read -r tree; do
        keep_stores+="$(BATON_TREE="$tree" BATON_STACK="$(detect_stack "$tree")" baton_fingerprint) "
        keep_caches+="$(tree_slug "$tree") "
    done <<<"$trees"

    local entry name
    for entry in "$STORE_ROOT"/*; do
        [ -e "$entry" ] || [ -L "$entry" ] || continue
        name="${entry##*/}"
        [[ "$keep_stores" == *" $name "* ]] && continue
        discard "$entry" "store $name: no tree has that lockfile any more"
    done
    for entry in "$CACHE_ROOT"/*; do
        [ -e "$entry" ] || continue
        name="${entry##*/}"
        [[ "$keep_caches" == *" $name "* ]] && continue
        discard "$entry" "cache $name: that tree is gone"
    done

    empty_trash
}

# ---------------------------------------------------------------- state machine

# wait_for_port_release blocks until nothing is listening on the app port.
#
# A server that does not set SO_REUSEADDR cannot rebind for a moment after the
# previous one exits, so starting the next tree immediately fails with "address
# already in use" and the handoff looks like a broken app. Plenty of servers do
# not set it, so baton waits rather than making that their problem.
wait_for_port_release() {
    [ -z "$BATON_PORT" ] && return 0
    local waited=0
    while [ "$waited" -lt 30 ]; do
        (exec 9<>"/dev/tcp/127.0.0.1/$BATON_PORT") 2>/dev/null || return 0
        exec 9<&- 2>/dev/null
        [ "$waited" -eq 0 ] && baton_log "waiting for port $BATON_PORT to be released"
        sleep 1
        waited=$((waited + 1))
    done
    baton_log "port $BATON_PORT is still held after ${waited}s; starting anyway"
}

stop_child() {
    [ -z "$child_pid" ] && return 0
    if kill -0 "$child_pid" 2>/dev/null; then
        baton_log "stopping the app (pid $child_pid)"
        # Signal the whole group: the start hook may have spawned workers that
        # would otherwise keep the port bound.
        kill -TERM -"$child_pid" 2>/dev/null || kill -TERM "$child_pid" 2>/dev/null
        for _ in $(seq 1 20); do
            kill -0 "$child_pid" 2>/dev/null || break
            sleep 0.5
        done
        kill -KILL -"$child_pid" 2>/dev/null || kill -KILL "$child_pid" 2>/dev/null
    fi
    wait "$child_pid" 2>/dev/null
    child_pid=""
}

# explain_prepare_failure turns a failed install into something actionable.
#
# Reads the log from the byte offset prepare started at. A bare "failed" sends
# people hunting for a bug in baton, when the real reason is sitting in a log
# that is mostly progress bars. These are the failures that are nothing to do
# with the branch being switched to, and that nobody guesses from the outside.
explain_prepare_failure() {
    local excerpt
    # Bounded to the tail: an install can log megabytes of progress, and the
    # error is always at the end.
    excerpt=$(tail -c "+$(($1 + 1))" "$LOG_FILE" 2>/dev/null | tail -c 65536 | tr -d '\0')

    case "$excerpt" in
        *ERR_PNPM_FETCH_401* | *"Unauthorized - 401"* | *"401 Unauthorized"* | *" E401"*)
            warn "the package registry refused a download with 401." \
                "Registry tokens usually come from the environment the container started with," \
                "so a container left up long enough ends up holding an expired one." \
                "Recreate the container to pick up a fresh token."
            ;;
        *ERR_PNPM_FETCH_403* | *"Forbidden - 403"* | *" E403"*)
            warn "the package registry refused a download with 403." \
                "The container's credentials are being accepted but are not allowed to fetch this package."
            ;;
        *ERR_PNPM_OUTDATED_LOCKFILE* | *"frozen-lockfile"* | *"lockfile does not satisfy"*)
            warn "the lockfile does not match this branch's manifest, so a frozen install cannot run." \
                "This one is the branch's fault: commit an updated lockfile."
            ;;
        *ENOSPC* | *"no space left on device"*)
            warn "the container ran out of disk while installing dependencies."
            ;;
        *ETIMEDOUT* | *ENOTFOUND* | *EAI_AGAIN* | *ECONNRESET*)
            warn "the package registry could not be reached. This is a network problem, not the branch."
            ;;
    esac
}

start_tree() {
    local tree="$1"

    if [ ! -d "$tree" ]; then
        baton_log "requested tree $tree does not exist"
        set_status failed
        return 1
    fi

    serving_tree="$tree"
    set_serving "$tree"
    set_status starting
    clear_notes
    baton_log "switching to $tree"

    export BATON_TREE="$tree"
    export BATON_STACK
    BATON_STACK=$(detect_stack "$tree")

    local fingerprint
    fingerprint=$(baton_fingerprint)
    serving_fingerprint="$fingerprint"
    export BATON_STORE="$STORE_ROOT/$fingerprint"
    export BATON_CACHE="$CACHE_ROOT/$(tree_slug "$tree")"
    mkdir -p "$BATON_CACHE"

    # The main clone usually already has a warm dependency directory. Adopting
    # it as the store for its fingerprint avoids paying for a second copy.
    if [ ! -e "$BATON_STORE" ] && [ "$BATON_STACK" != "pip" ]; then
        if [ "$fingerprint" = "$(BATON_TREE=$BATON_CODE baton_fingerprint)" ] \
            && [ -d "$BATON_CODE/node_modules" ] && ! is_mounted "$BATON_CODE/node_modules"; then
            baton_log "adopting the main clone's node_modules as the store for $fingerprint"
            ln -sfn "$BATON_CODE/node_modules" "$BATON_STORE"
        fi
    fi
    [ -e "$BATON_STORE" ] && BATON_STORE=$(readlink -f "$BATON_STORE")
    export BATON_STORE

    cd "$tree" || { set_status failed; return 1; }
    baton_log "stack $BATON_STACK, fingerprint $fingerprint"

    baton_log "waiting for dependencies"
    baton_wait_deps

    baton_log "preparing $tree"
    local prepare_from
    prepare_from=$(wc -c <"$LOG_FILE" 2>/dev/null || echo 0)
    if ! baton_prepare >>"$LOG_FILE" 2>&1; then
        baton_log "prepare failed in $tree"
        explain_prepare_failure "$prepare_from"
        set_status failed
        return 1
    fi

    if ! baton_migrate; then
        baton_log "migrate failed in $tree"
        set_status failed
        return 1
    fi

    wait_for_port_release

    baton_log "starting the app in $tree"
    set -m
    ( cd "$tree" && baton_start ) >>"$LOG_FILE" 2>&1 &
    child_pid=$!
    set +m

    wait_ready && collect_garbage
}

wait_ready() {
    local waited=0
    while [ "$waited" -lt "$READY_TIMEOUT" ]; do
        if ! kill -0 "$child_pid" 2>/dev/null; then
            baton_log "the app exited during startup"
            set_status failed
            return 1
        fi
        if baton_health; then
            baton_log "ready: $serving_tree"
            set_status ready
            return 0
        fi
        sleep 2
        waited=$((waited + 2))
    done
    baton_log "timed out waiting for the app in $serving_tree"
    set_status failed
    return 1
}

shutdown() {
    baton_log "supervisor shutting down"
    set_status stopped
    stop_child
    exit 0
}
trap shutdown TERM INT

# ---------------------------------------------------------------- boot

# A strategy file is sourced after the defaults, so anything it defines wins.
if [ -f "$STRATEGY_FILE" ]; then
    baton_log "loading strategy from $STRATEGY_FILE"
    # shellcheck disable=SC1090
    . "$STRATEGY_FILE"
else
    baton_log "no strategy file, using built-in defaults"
fi

# The port is whatever the strategy declares, or PORT from the environment.
# Written out so the Go side can health-check without knowing the stack.
BATON_PORT="${BATON_PORT:-${PORT:-}}"
export BATON_PORT BATON_HEALTH_PATH="${BATON_HEALTH_PATH:-/}"
printf '%s\n' "$BATON_PORT" >"$PORT_FILE"

# Default to the main clone so the container behaves normally before anything
# has ever taken the baton.
if [ ! -s "$CURRENT_FILE" ]; then
    printf '%s\n' "$BATON_CODE" >"$CURRENT_FILE"
fi

# A linked worktree's .git is a file holding an absolute host path, which does
# not exist inside the container — so git fails in every worktree, and with it
# anything that shells out to git: lint, changed-file detection, tooling that
# reads the branch. Making the host path resolve to the same code fixes all of
# it at once, and costs one symlink.
if [ -n "${BATON_HOST_CODE:-}" ] && [ "$BATON_HOST_CODE" != "$BATON_CODE" ]; then
    mkdir -p "$(dirname "$BATON_HOST_CODE")"
    ln -sfn "$BATON_CODE" "$BATON_HOST_CODE"
    baton_log "git in worktrees enabled: $BATON_HOST_CODE -> $BATON_CODE"
fi

baton_log "supervisor started, watching $CURRENT_FILE (port ${BATON_PORT:-none})"
# A delete cut short by a container restart.
empty_trash
start_tree "$(cat "$CURRENT_FILE")"

while true; do
    requested=$(cat "$CURRENT_FILE" 2>/dev/null)

    # An explicit restart, used when the app died and the tree has not changed.
    # Rewriting current-tree with the same value would not be noticed.
    if [ -f "$RESTART_FILE" ]; then
        rm -f "$RESTART_FILE"
        baton_log "restart requested for $serving_tree"
        stop_child
        start_tree "${requested:-$serving_tree}"
    elif [ -n "$requested" ] && [ "$requested" != "$serving_tree" ]; then
        stop_child
        start_tree "$requested"
    elif [ -n "$child_pid" ] && ! kill -0 "$child_pid" 2>/dev/null; then
        # The app died on its own. Report it and leave the container up so the
        # next take can recover without a container restart.
        if [ "$(cat "$STATUS_FILE" 2>/dev/null)" != "failed" ]; then
            baton_log "the app for $serving_tree exited unexpectedly"
            set_status failed
        fi
    fi

    sleep 1
done
