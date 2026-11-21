# Shared by the fake agents in demo/bin, which stand in for real ones while
# demo.tape records. Each prints a short transcript, "works" for a while behind
# an animated spinner, then draws an input area in one of the shapes cc-watch
# recognises and waits there. None of them talks to anything.

spinner=(⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏)

# work SECONDS LABEL: an animated spinner line, as agents show while busy.
work() {
	local end=$((SECONDS + $1)) i=0
	while [ $SECONDS -lt $end ]; do
		printf '\r\e[33m%s\e[0m %s\e[K' "${spinner[i % 10]}" "$2"
		i=$((i + 1))
		sleep 0.1
	done
	printf '\r\e[K'
}

prompt() { printf '\e[1m> %s\e[0m\n\n' "$1"; }
done_() { printf '\e[32m●\e[0m %b\n' "$1"; }

# The three input shapes: a box, a composer line, an editor between rules.
box() {
	printf '\n\e[90m╭──────────────────────────────────────────────────╮\e[0m\n'
	printf '\e[90m│\e[0m > %-47s\e[90m│\e[0m\n' ""
	printf '\e[90m╰──────────────────────────────────────────────────╯\e[0m\n'
	printf '  \e[90m? for shortcuts\e[0m\n'
}
composer() {
	printf '\n› \e[90mAsk anything\e[0m\n'
	printf '  \e[90m? for shortcuts                       92%% context left\e[0m\n'
}
rules() {
	local rule=──────────────────────────────────────────────────
	printf '\n\e[90m%s\e[0m\n \n\e[90m%s\e[0m\n' "$rule" "$rule"
	printf '\e[90m~/project (main)        ↑12k ↓3.4k  $0.12\e[0m\n'
}

# idle: sit at the input until the pane is closed.
idle() { while :; do read -r _ || sleep 3600; done; }
