#!/bin/bash
# Tears down what demo/start.sh built.
f="${TMPDIR:-/tmp}/cc-watch-demo.root"
[ -f "$f" ] || exit 0
root=$(cat "$f")
XDG_RUNTIME_DIR="$root/run" "$root/cc-watch" --stop-server >/dev/null 2>&1
tmux -L cc-watch-demo kill-server 2>/dev/null
rm -rf "$root" "$f"
