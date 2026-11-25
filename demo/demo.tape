# The README's demo. Record it from the repository root:
#
#   vhs demo/demo.tape
#
# demo/start.sh runs everything on a private tmux server with fake agents
# (demo/bin), its own config and its own daemon, so your own tmux sessions and
# cc-watch daemon are left alone.

Output demo/cc-watch.gif

Require tmux
Require go

Set Shell "bash"
Set FontSize 15
Set Width 1200
Set Height 520
Set Padding 12
Set Framerate 24
Set TypingSpeed 0
Set Theme "Catppuccin Mocha"

Hide
Type "demo/start.sh" Enter
# Wait for the daemon to classify the agents: a pane shows "..." for its
# first 5 seconds.
Wait+Screen@90s /waiting/
Sleep 1s
Show

# Four agents: some running, some waiting on you, every one with its bell on.
Sleep 3s

# Silence "api": it will finish without a notification.
Type "b"
Sleep 2s

# The preview follows the selection.
Down
Sleep 1.5s
Down
Sleep 1.5s
Down
Sleep 2s

# When "notes" finishes its turn, the mark fills in, in the list and in the
# tmux status line.
Wait+Screen@60s /◆/
Sleep 3s

# Jump to it, then back: seen, the mark empties again.
Enter
Sleep 3s
Ctrl+B
Type "L"
Sleep 3s

# The preview can be hidden.
Type "p"
Sleep 2s
Type "p"
Sleep 2s

Hide
Ctrl+B
Type "d"
Type "demo/stop.sh" Enter
Sleep 1s
