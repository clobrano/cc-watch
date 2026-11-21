#!/bin/bash
# Builds the world demo.tape records: a private tmux server running four fake
# agents (demo/bin) and the dashboard. It has its own HOME, so its own config,
# and its own runtime directory, so its own daemon: your tmux server, daemon
# and config are not touched. demo/stop.sh tears it down.
set -e
cd "$(dirname "$0")/.."
root=$(mktemp -d "${TMPDIR:-/tmp}/cc-watch-demo.XXXXXX")
go build -o "$root/cc-watch" .
mkdir -p "$root/home/.config/cc-watch" "$root/run"
chmod 700 "$root/run"
# Notify one second after a turn ends, and to nowhere: a desktop popup would
# not be in the recording anyway.
echo '{ "notify_after": 1, "notify_command": ["true"] }' >"$root/home/.config/cc-watch/config.json"
echo "$root" >"${TMPDIR:-/tmp}/cc-watch-demo.root"

export HOME="$root/home" XDG_RUNTIME_DIR="$root/run" PATH="$PWD/demo/bin:$root:$PATH"
unset TMUX
# Every session at the size of this terminal: one created at tmux's default
# size would be resized on the first visit, and the redraw would read as the
# agent producing output.
size=(-x "$(tput cols)" -y "$(($(tput lines) - 1))")
tm() { tmux -L cc-watch-demo -f demo/tmux.conf "$@"; }
tm new-session -d -s dashboard "${size[@]}" cc-watch
tm new-session -d -s api "${size[@]}" claude
tm new-session -d -s docs "${size[@]}" codex
tm new-session -d -s infra "${size[@]}" pi
tm new-session -d -s notes "${size[@]}" gemini
exec tmux -L cc-watch-demo attach -t dashboard
