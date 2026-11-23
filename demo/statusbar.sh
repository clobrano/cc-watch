#!/bin/bash
# Cuts the right end of the tmux status line, where the cc-watch strip sits,
# out of demo/cc-watch.gif and enlarges it, for the README's status bar
# section. The crop box (300x32 at 888,480) fits demo.tape's 1200x520 terminal
# with 12px of padding: change it along with them.
set -e
cd "$(dirname "$0")"
ffmpeg -v error -y -i cc-watch.gif -filter_complex \
	"crop=300:32:888:480,scale=600:-1:flags=lanczos,split[a][b];[a]palettegen[p];[b][p]paletteuse" \
	statusbar.gif
