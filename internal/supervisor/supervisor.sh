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

BATON_CODE=/code
BATON_CONTROL="$BATON_CODE/.baton"
CURRENT_FILE="$BATON_CONTROL/current-tree"
SERVING_FILE="$BATON_CONTROL/serving"
STATUS_FILE="$BATON_CONTROL/status"
PORT_FILE="$BATON_CONTROL/port"
NOTES_FILE="$BATON_CONTROL/notes"
LOG_FILE="$BATON_CONTROL/supervisor.log"
STORE_ROOT="$BATON_CONTROL/store"
CACHE_ROOT="$BATON_CONTROL/cache"
STRATEGY_FILE="$BATON_CONTROL/strategy.sh"

READY_TIMEOUT="${BATON_READY_TIMEOUT:-900}"

mkdir -p "$BATON_CONTROL" "$STORE_ROOT" "$CACHE_ROOT"

child_pid=""
serving_tree=""

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

# note records something the human should see in `baton status`. Used for
# conditions that are not failures but change how results should be read.
note() {
    printf '%s\t%s\n' "$(date -u +%H:%M:%S)" "$*" >>"$NOTES_FILE"
    baton_log "NOTE: $*"
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
            pip3 install -r requirements.txt
            [ -f setup.py ] || [ -f pyproject.toml ] && pip3 install -e .
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
        *)
            baton_log "no default start for an unrecognised stack; define baton_start in strategy.sh"
            return 1
            ;;
    esac
}

# baton_health exits 0 once the server is answering.
baton_health() {
    [ -z "$BATON_PORT" ] && return 0
    curl -sf -o /dev/null --max-time 3 "http://127.0.0.1:$BATON_PORT${BATON_HEALTH_PATH:-/}"
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
alembic_migrate() {
    local before after
    before=$(alembic current 2>/dev/null | head -1 | awk '{print $1}')

    if ! alembic upgrade head >>"$LOG_FILE" 2>&1; then
        baton_log "alembic upgrade failed"
        return 1
    fi

    after=$(alembic current 2>/dev/null | head -1 | awk '{print $1}')
    local head
    head=$(alembic heads 2>/dev/null | head -1 | awk '{print $1}')

    if [ -n "$head" ] && [ -n "$after" ] && [ "$after" != "$head" ]; then
        note "database is at $after but this tree expects $head — the schema is ahead of the branch. Migration-dependent results are not trustworthy. Fixing it means a downgrade, which is destructive, so baton will not do it."
    elif [ -n "$before" ] && [ "$before" != "$after" ]; then
        baton_log "migrated $before -> $after (shared database, other trees are affected)"
        note "applied migrations $before -> $after. Other worktrees share this database."
    fi
}

# ---------------------------------------------------------------- state machine

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
    if ! baton_prepare >>"$LOG_FILE" 2>&1; then
        baton_log "prepare failed in $tree"
        set_status failed
        return 1
    fi

    if ! baton_migrate; then
        baton_log "migrate failed in $tree"
        set_status failed
        return 1
    fi

    baton_log "starting the app in $tree"
    setsid bash -c 'cd "$BATON_TREE" && baton_start' &
    child_pid=$!

    wait_ready
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

# baton_start runs in a subshell, so the hooks and helpers it needs must be
# visible there.
export -f baton_start baton_log 2>/dev/null || true

# Default to the main clone so the container behaves normally before anything
# has ever taken the baton.
if [ ! -s "$CURRENT_FILE" ]; then
    printf '%s\n' "$BATON_CODE" >"$CURRENT_FILE"
fi

baton_log "supervisor started, watching $CURRENT_FILE (port ${BATON_PORT:-none})"
start_tree "$(cat "$CURRENT_FILE")"

while true; do
    requested=$(cat "$CURRENT_FILE" 2>/dev/null)

    if [ -n "$requested" ] && [ "$requested" != "$serving_tree" ]; then
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
