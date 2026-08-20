#!/bin/bash
# Scaffolds a throwaway project to try baton against.
#
# It builds the smallest thing that has the shape baton needs: a git repo, a
# container that bind-mounts it, and two worktrees living inside that mount.
# Nothing here touches anything you already have — it all lands in one
# directory you can delete afterwards.

set -euo pipefail

DEMO_DIR="${1:-/tmp/baton-demo}"
PORT="${BATON_DEMO_PORT:-18080}"

if [ -e "$DEMO_DIR" ]; then
    echo "error: $DEMO_DIR already exists. Remove it or pass another path." >&2
    exit 1
fi

echo "Creating a demo project in $DEMO_DIR"
mkdir -p "$DEMO_DIR/app"
cd "$DEMO_DIR/app"

git init -q
git config user.email demo@example.com
git config user.name "baton demo"

# An empty requirements.txt is enough for baton to detect a pip project.
: >requirements.txt

cat >index.html <<'HTML'
<!doctype html>
<title>baton demo</title>
<h1>main</h1>
<p>You are looking at the main branch.</p>
HTML

cat >serve.py <<'PY'
"""The smallest server that proves which worktree is being served."""
import http.server
import os
import socketserver

PORT = int(os.environ.get("BATON_PORT", "8080"))

# Without this the socket lingers after a stop and the next tree cannot bind.
# Most real servers set it; baton waits for the port either way.
socketserver.TCPServer.allow_reuse_address = True

with socketserver.TCPServer(("", PORT), http.server.SimpleHTTPRequestHandler) as httpd:
    print(f"serving {os.getcwd()} on {PORT}", flush=True)
    httpd.serve_forever()
PY

git add -A
git commit -qm "demo app on main"

# Two branches whose only difference is what the page says, so a handoff is
# visible with curl.
for branch in feature-blue feature-green; do
    git worktree add -q ".worktrees/$branch" -b "$branch"
    colour="${branch#feature-}"
    cat >".worktrees/$branch/index.html" <<HTML
<!doctype html>
<title>baton demo</title>
<h1>$colour</h1>
<p>You are looking at the $branch branch.</p>
HTML
    git -C ".worktrees/$branch" add -A
    git -C ".worktrees/$branch" commit -qm "$branch page"
done

# Worktrees must live inside the bind mount or the container cannot see them.
# They are noise in git status, so hide them.
printf '.worktrees/\n.baton/\n' >>.git/info/exclude

# baton can detect a lot, but not how to start a Python app — there is no
# universal command. Writing the strategy up front keeps the walkthrough about
# baton rather than about debugging a container that will not boot.
mkdir -p .baton
cat >.baton/strategy.sh <<'STRATEGY'
#!/bin/bash
# baton strategy for the demo app.
#
# The supervisor sources this after its own defaults, so anything defined here
# wins. Everything except baton_start is left to the defaults.

baton_start() { exec python3 serve.py; }
STRATEGY

cd "$DEMO_DIR"
cat >docker-compose.yml <<COMPOSE
services:
  demo:
    image: python:3-slim
    container_name: baton-demo
    # The repo is bind mounted, which is what lets the container see every
    # worktree without any extra configuration.
    volumes:
      - $DEMO_DIR/app:/code
    working_dir: /code
    command: python3 serve.py
    environment:
      - BATON_PORT=$PORT
    ports:
      - 127.0.0.1:$PORT:$PORT
    # Not required, but lets baton share one dependency directory between
    # worktrees instead of installing per tree.
    privileged: true
    init: true
COMPOSE

cat <<DONE

Done. A demo project is ready in $DEMO_DIR

  app/                      a git repo with three branches
  app/.worktrees/           feature-blue and feature-green, inside the mount
  docker-compose.yml        one container, port $PORT

Start it:

  cd $DEMO_DIR && docker compose up -d

Then follow the walkthrough in examples/walkthrough/README.md.

Delete everything afterwards with:

  cd $DEMO_DIR && docker compose down && cd / && rm -rf $DEMO_DIR
DONE
