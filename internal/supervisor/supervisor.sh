#!/bin/bash
# baton supervisor — runs as the container's command in place of the usual
# runner script.
#
# It keeps the container up permanently and moves only the dev server between
# worktrees. baton writes the worktree it wants into .baton/current-tree on the
# host; this loop notices, stops the current dev server, and starts a new one in
# the requested tree. The container itself never restarts, which is what turns a
# handoff from minutes into seconds.
#
# Two costs are amortised here:
#   node_modules  — shared between every worktree with the same pnpm lockfile,
#                   bind mounted into each tree rather than installed per tree.
#   rspack cache  — kept per tree, bind mounted over the shared node_modules so
#                   trees do not invalidate each other's build cache.

set -uo pipefail

CODE=/code
CONTROL="$CODE/.baton"
CURRENT_FILE="$CONTROL/current-tree"
SERVING_FILE="$CONTROL/serving"
STATUS_FILE="$CONTROL/status"
LOG_FILE="$CONTROL/supervisor.log"
NM_STORE="$CONTROL/node_modules"
CACHE_STORE="$CONTROL/rspack-cache"

DEV_PORT="${PORT:-3301}"
READY_TIMEOUT="${BATON_READY_TIMEOUT:-900}"

mkdir -p "$CONTROL" "$NM_STORE" "$CACHE_STORE"

child_pid=""
serving_tree=""

log() {
    printf '%s baton-supervisor: %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG_FILE"
}

set_status() { printf '%s\n' "$1" >"$STATUS_FILE"; }
set_serving() { printf '%s\n' "$1" >"$SERVING_FILE"; }

# is_mounted checks /proc rather than shelling out to mountpoint, which is not
# guaranteed to be installed in the node base image.
is_mounted() { grep -qs " $1 " /proc/self/mounts; }

lockfile_hash() {
    if [ -f "$1/pnpm-lock.yaml" ]; then
        sha256sum "$1/pnpm-lock.yaml" | cut -c1-12
    else
        echo "nolock"
    fi
}

# tree_slug turns /code/.worktrees/pr-12254 into worktrees_pr-12254 for use as a
# cache directory name.
tree_slug() {
    local slug
    slug=$(printf '%s' "${1#"$CODE"}" | sed 's#^/##; s#^\.##; s#/#_#g')
    [ -z "$slug" ] && slug="main"
    printf '%s' "$slug"
}

# link_node_modules points a tree at the shared node_modules for its lockfile.
# The main clone usually already has a warm 3 GB install, so the first tree with
# a matching lockfile adopts that directory as the store instead of paying for a
# second copy.
link_node_modules() {
    local tree="$1"
    local hash store target
    hash=$(lockfile_hash "$tree")
    store="$NM_STORE/$hash"

    if [ ! -e "$store" ]; then
        if [ "$hash" = "$(lockfile_hash "$CODE")" ] && [ -d "$CODE/node_modules" ] && ! is_mounted "$CODE/node_modules"; then
            log "adopting the main clone's node_modules as the store for $hash"
            ln -sfn "$CODE/node_modules" "$store"
        else
            mkdir -p "$store"
        fi
    fi
    store=$(readlink -f "$store")

    target="$tree/node_modules"
    if [ "$target" = "$store" ]; then
        return 0
    fi

    # Worktrees are often set up by hand with a node_modules symlink pointing at
    # the main clone. Those come in three flavours — relative, /code-absolute,
    # and host-absolute — and only the relative one resolves on both sides of
    # the container boundary. They are also frequently wrong: a tree with its
    # own lockfile ends up resolving the main clone's dependencies. Replace any
    # such link with a real mount point for the store that matches this tree's
    # lockfile, recording what was there in case it needs putting back.
    if [ -L "$target" ]; then
        local previous
        previous=$(readlink "$target")
        log "replacing the node_modules symlink in $tree (was -> $previous)"
        printf '%s\t%s\n' "$tree" "$previous" >>"$CONTROL/replaced-symlinks.log"
        rm -f "$target"
    fi

    mkdir -p "$target"
    if ! is_mounted "$target"; then
        local error
        log "mounting shared node_modules ($hash) into $tree"
        if ! error=$(mount --bind "$store" "$target" 2>&1); then
            log "bind mount failed: $error"
            return 1
        fi
    fi
}

# link_rspack_cache gives each tree its own persistent rspack cache. Without
# this, trees sharing a node_modules would also share the cache directory that
# rspack.dev.js places inside it, and would thrash each other's builds.
link_rspack_cache() {
    local tree="$1"
    local cache_dir target
    cache_dir="$CACHE_STORE/$(tree_slug "$tree")"
    target="$tree/node_modules/.cache/rspack"

    mkdir -p "$cache_dir" "$target"
    if ! is_mounted "$target"; then
        log "mounting per-tree rspack cache for $(tree_slug "$tree")"
        mount --bind "$cache_dir" "$target" || log "cache bind mount failed, continuing with a shared cache"
    fi
}

stop_child() {
    [ -z "$child_pid" ] && return 0
    if kill -0 "$child_pid" 2>/dev/null; then
        log "stopping dev server (pid $child_pid)"
        # Signal the whole group: start.sh execs rspack, but plugins may have
        # spawned workers that would otherwise keep the port bound.
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
        log "requested tree $tree does not exist"
        set_status failed
        return 1
    fi

    serving_tree="$tree"
    set_serving "$tree"
    set_status starting
    log "switching to $tree"

    link_node_modules "$tree" || { set_status failed; return 1; }
    link_rspack_cache "$tree"

    cd "$tree" || { set_status failed; return 1; }

    log "installing dependencies in $tree"
    if ! pnpm install --frozen-lockfile >>"$LOG_FILE" 2>&1; then
        log "pnpm install failed in $tree"
        set_status failed
        return 1
    fi

    log "starting dev server in $tree"
    setsid ./scripts/start.sh &
    child_pid=$!

    wait_ready
}

wait_ready() {
    local waited=0
    while [ "$waited" -lt "$READY_TIMEOUT" ]; do
        if ! kill -0 "$child_pid" 2>/dev/null; then
            log "dev server exited during startup"
            set_status failed
            return 1
        fi
        if curl -sf -o /dev/null --max-time 3 "http://127.0.0.1:$DEV_PORT/"; then
            log "ready: $serving_tree"
            set_status ready
            return 0
        fi
        sleep 2
        waited=$((waited + 2))
    done
    log "timed out waiting for the dev server in $serving_tree"
    set_status failed
    return 1
}

shutdown() {
    log "supervisor shutting down"
    set_status stopped
    stop_child
    exit 0
}
trap shutdown TERM INT

# Default to the main clone so the container behaves normally before anything
# has ever taken the baton.
if [ ! -s "$CURRENT_FILE" ]; then
    printf '%s\n' "$CODE" >"$CURRENT_FILE"
fi

log "supervisor started, watching $CURRENT_FILE"
start_tree "$(cat "$CURRENT_FILE")"

while true; do
    requested=$(cat "$CURRENT_FILE" 2>/dev/null)

    if [ -n "$requested" ] && [ "$requested" != "$serving_tree" ]; then
        stop_child
        start_tree "$requested"
    elif [ -n "$child_pid" ] && ! kill -0 "$child_pid" 2>/dev/null; then
        # The dev server died on its own. Report it and leave the container up
        # so the next take can recover without a container restart.
        if [ "$(cat "$STATUS_FILE" 2>/dev/null)" != "failed" ]; then
            log "dev server for $serving_tree exited unexpectedly"
            set_status failed
        fi
    fi

    sleep 1
done
