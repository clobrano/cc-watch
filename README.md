# cc-watch

A terminal dashboard for coding-agent sessions running inside tmux. Claude Code,
Codex, Gemini CLI and opencode are watched out of the box; other agents can be
added.

When you have several agents working in parallel across tmux windows and panes, it
is hard to tell which one is still thinking, which one is waiting for your input,
and which one has quit. `cc-watch` polls tmux, classifies every agent pane, and
lets you jump straight to the one that needs you.

```
  cc-watch  15:04:05

    SESSION                   STATE      LAST PROMPT
    ──────────────────────────────────────────────────────────────────────────
    ✻ notes                   running    rewrite the parser to accept trailing...
  > ✦ api:0.1                 waiting    add rate limiting to the /search endpoint
    ✵ api:1.0                 waiting    why is the integration suite flaky?
    ✻ docs                    idle       document the new --serve flag
    ✦ scratch                 error      $
```

## How it works

Every 2 seconds `cc-watch` runs `tmux list-panes -a` to enumerate every pane in
every session, keeps the ones running a configured agent (`claude`, `codex`,
`gemini` and `opencode` by default), and inspects each of them:

- the pane title, via `tmux display-message -p '#{pane_title}'`
- the last 80 lines of output, via `tmux capture-pane -p`

### Finding the agent behind its process name

A pane's command is not always the agent's name. Claude Code renames its own
process, so tmux reports `claude` and matching it is enough. Gemini CLI does
not: its executable is a plain `#!/usr/bin/env node` script, so every Gemini
pane shows up as `node` — and behind a wrapper script, a pane can report the
wrapper or the shell instead. No `agent_commands` entry can match any of those.

So when a pane's command names no agent, cc-watch takes one `ps` snapshot per
poll and walks the processes below that pane, looking for one whose program
names a configured agent — either as the executable itself (`.../bin/gemini`)
or as the installed package it was exec'd from
(`.../node_modules/@google/gemini-cli/dist/index.js`).

Codex lands on both sides of this. Installed from Homebrew, cargo or the install
script it is a native binary that renames nothing, so its pane already reports
`codex`; installed from npm it is the same `#!/usr/bin/env node` shim story as
Gemini, with node running `.../node_modules/@openai/codex/bin/codex.js` and that
spawning the platform binary underneath
(`.../node_modules/@openai/codex-linux-x64/vendor/<target>/bin/codex`). Both of
those are found by the same walk.

opencode is a native binary whichever way it is installed, so it usually needs
no walk at all: its pane reports `opencode` and matching that is enough. The one
exception is an npm install, which lays the binary down under its own package
directory (`.../node_modules/opencode-ai/bin/opencode.exe`) — that path is
matched the same way an installed package is for the agents above.

An argument only counts if it is an agent's executable or an installed agent
package, so a pane running `node server.js` from a directory named
`codex-experiments`, or passing a `gemini.json` config, is not mistaken for an
agent. The snapshot is lazy — panes that name their own agent never trigger it —
and taken at most once per poll.

From those two signals it derives a state:

| State     | Colour | Meaning                                                                     |
| --------- | ------ | ---------------------------------------------------------------------------- |
| `...`     | grey   | Pane seen for the first time; held until enough output has been observed to classify |
| `running` | green  | The agent is working — a braille spinner in the pane title, or output still changing |
| `waiting` | cyan   | The agent is sitting at its input and nothing has changed for 5 seconds. Two shapes are recognised: the box Gemini CLI and older Claude Code builds draw around the input, and the bare marker line Codex and newer Claude Code builds use instead |
| `idle`    | yellow | Output has been unchanged for 5 seconds with no input on screen               |
| `error`   | red    | The tail of the pane is a bare shell prompt — the agent exited                |
| `unknown` | grey   | The pane is empty                                                            |

Each row is marked with the agent running in it — `✻` for Claude Code, `✵` for
Codex, `✦` for Gemini CLI, `✶` for opencode — so a screen of mixed sessions stays
readable. Codex's own `>_` was not an option: the dashboard already spends `>` on
the selection pointer. The mark sits inside
the session column rather than taking a column of its own, so it costs the
`LAST PROMPT` text nothing. An agent with no icon of its own is marked with its
initial, and `agent_icons` overrides any of them.

All four default glyphs are one terminal column wide and carry no emoji
presentation, so they do not disturb the column alignment. If yours renders
them at double width, set an ASCII icon:

```json
{ "agent_icons": { "claude": "c", "codex": "x", "gemini": "g", "opencode": "o" } }
```

The `LAST PROMPT` column shows the last thing **you** asked that agent to do. A
pane's tail tells you little — mid-turn it is a spinner, and at rest it is the
mode-hint footer, which reads the same for every agent — whereas the prompt is
what actually distinguishes one session from another when four of them are idle
at once.

It is read from the transcript, where a submitted prompt is a line opening with a
prompt glyph at column 0 — ASCII `>` in older Claude Code builds, a chevron in
the newer borderless UI, `›` in Codex — so a small family of glyphs is matched
rather than one. The two-space-indented continuations both Claude Code and Codex
wrap long prompts onto are joined back on, so a multi-line prompt survives as far
as the column width.

Text typed but not yet sent is deliberately excluded. In the borderless UI the
live input carries the same glyph as a submitted prompt, so shape cannot separate
them — the same is true of Codex, whose composer is a `›` carrying placeholder
text — but it is always the bottom-most one on screen, which is anchor enough:
above it is transcript, below it is only the footer. The older UI walls its input
off inside a box instead, so whichever turns up first scanning up from the
footer, a box border or a bare glyph, marks the bottom of the input region.

Once seen, a prompt is kept: a long turn pushes it out of the captured window
well before the agent is done, and blanking the column for the rest of the turn
would defeat the point. When a pane is first noticed cc-watch does one deeper
capture (`promptScrollback`, 400 lines) looking for the prompt of a turn already
in flight, so agents that were running before you started the dashboard are not
blank either.

If no prompt can be found at all, the column falls back to the last
non-decorative line of the pane, with ANSI escapes and box-drawing characters
stripped.

Panes that disappear are dropped from the list on the next refresh. When a single
tmux session contains more than one agent pane, the display switches from the bare
session name to the full `session:window.pane` key so the rows stay unambiguous.

## Requirements

- [tmux](https://github.com/tmux/tmux)
- Go 1.25 or newer (to build)

## Install

```sh
go install github.com/clobrano/cc-watch@latest
```

Or build from a clone:

```sh
git clone https://github.com/clobrano/cc-watch
cd cc-watch
go build -o cc-watch .
```

## Usage

```sh
cc-watch
```

| Key             | Action                    |
| --------------- | ------------------------- |
| `↑` / `↓`       | Move the selection        |
| `↵` (Enter)     | Attach to the selected pane's session |
| `q` / `Ctrl-C`  | Quit                      |

Attaching adapts to where you are running from:

- **Inside tmux**, it uses `tmux switch-client`, so the jump is instant and
  `cc-watch` keeps running in the pane you left.
- **Outside tmux**, it suspends the TUI, runs `tmux attach-session`, and resumes
  the dashboard once you detach.

The TUI uses the alternate screen buffer, so your scrollback is left intact, and it
adapts to terminal resizes.

### Flags

| Flag            | Action                                                                  |
| --------------- | ----------------------------------------------------------------------- |
| _(none)_        | Run the interactive dashboard                                           |
| `--serve`       | Start the background daemon: [tmux status bar](#tmux-status-bar) only, no dashboard |
| `--stop-server` | Stop the running daemon                                                 |
| `--doctor`      | Report what cc-watch sees in every pane, then exit — see [Troubleshooting](#troubleshooting) |

## tmux status bar

While it runs, cc-watch publishes a compact strip of every agent it has found to
the tmux user option `@cc_watch_agents` — one number per agent, coloured by the
same states as the dashboard. It is nothing but an option until you reference it,
so add it wherever you want in your `.tmux.conf`:

```tmux
set -ag status-right ' #{@cc_watch_agents}'
set -g  status-right-length 60
```

`-a` appends rather than replacing, so your existing clock and hostname survive.
The length bump matters: `status-right-length` defaults to 40 and tmux truncates
past it silently, so on a busy status line the agent digits are exactly what falls
off the end — which looks like the feature not working. Put it on the left instead
if you prefer, but raise the limit there too, as `status-left-length` defaults to
just 10:

```tmux
set -ag status-left ' #{@cc_watch_agents}'
set -g  status-left-length 40
```

Reload with `tmux source-file ~/.tmux.conf`. You do not need to touch
`status-interval` — cc-watch redraws clients itself when the strip changes.

You then get an at-a-glance indicator from any session:

```
                                              AGENTS 1 2 3
                                                     │ │ └─ cyan:   waiting on you
                                                     │ └─── yellow: idle
                                                     └───── green:  running
```

The numbers are positional over the same sorted list the dashboard renders, so
agent `2` in the status bar is row 2 in the TUI — the TUI is the legend that tells
you which session that is. Because they are positional, they renumber whenever an
agent pane appears or dies: a number is a pointer to a row, not a durable handle
on a session.

The strip is pushed on each 2-second poll, but the option is only rewritten (and
clients only redrawn, via `refresh-client -S`) when the rendered string actually
changes. On exit cc-watch unsets the option, so a frozen strip never lingers in
your status bar — which also means the indicator is live only while cc-watch is
running. To keep it live without keeping the dashboard open, run the daemon
described in [Daemon mode](#daemon-mode).

To check the strip is live while cc-watch is running, read the raw option:

```sh
$ tmux show-options -gqv @cc_watch_agents
#[fg=green]1 #[fg=yellow]2#[default]
```

Empty output means either cc-watch is not running or it found no agents.

> Why it has to work this way: classification is history-dependent. `idle` and
> `waiting` mean "unchanged for 5 seconds", which is only knowable by comparing
> consecutive polls. A one-shot `#(cc-watch --status)` invoked by tmux would start
> cold every time and could only ever distinguish `running` from `error`.

## Daemon mode

The status strip needs a long-running process behind it, but it does not need the
dashboard. `--serve` starts cc-watch in the background with the tmux support only:
the same 2-second poll and the same `@cc_watch_agents` option, no TUI, no terminal
of its own.

```sh
$ cc-watch --serve
cc-watch daemon started (pid 48213)
$ cc-watch --stop-server
cc-watch daemon stopped (pid 48213)
```

`--serve` re-executes cc-watch in a new session (`setsid`), so the daemon outlives
the shell that started it, and returns only once the child is actually up — if it
fails to start you get an error instead of a silent no-op. `--stop-server` sends
`SIGTERM` and waits for it to exit; the daemon unsets `@cc_watch_agents` on the way
out, so the strip disappears with it.

Only one daemon runs at a time. A second `--serve` reports the running pid and
exits non-zero rather than starting a rival poller.

Put it in your `.tmux.conf` to have the strip come up with tmux itself:

```tmux
run-shell -b 'cc-watch --serve'
```

`-b` keeps tmux from blocking on it, and starting it twice is harmless — the
second call sees the first daemon and exits.

The daemon and the dashboard can run at the same time — they compute the same
strip from the same tmux state, so they agree, and quitting the TUI leaves the
daemon's strip alone rather than clearing it.

State lives in `$XDG_RUNTIME_DIR/cc-watch/` (falling back to
`$TMPDIR/cc-watch-$UID/`):

| File         | Contents                                                          |
| ------------ | ----------------------------------------------------------------- |
| `daemon.pid` | The daemon's pid, and the lock file that makes "running" knowable  |
| `daemon.log` | Daemon start/stop lines — the first place to look if `--serve` fails |

Liveness is an advisory lock (`flock`) on the pid file, not the pid itself. The
kernel drops the lock when the holder dies, so a daemon that is `SIGKILL`ed leaves
nothing stale to clean up: the next `--serve` just starts, and `--stop-server`
correctly reports that nothing is running instead of signalling whatever process
happens to have inherited that pid.

### systemd

The daemon is per-user and needs `$XDG_RUNTIME_DIR`, so it belongs in a **user**
unit, not a system one. Write `~/.config/systemd/user/cc-watch.service`:

```ini
[Unit]
Description=cc-watch tmux status daemon

[Service]
Type=forking
PIDFile=%t/cc-watch/daemon.pid
ExecStart=%h/go/bin/cc-watch --serve
ExecStop=%h/go/bin/cc-watch --stop-server
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now cc-watch
```

`Type=forking` is the right description: `--serve` starts the daemon and exits,
leaving the child behind. `%t` expands to `$XDG_RUNTIME_DIR`, which is exactly
where the pid file lands, so systemd tracks the daemon rather than the launcher.
Adjust `%h/go/bin/cc-watch` if the binary lives elsewhere.

## Troubleshooting

### An agent does not show up

Run `cc-watch --doctor`. It lists every tmux pane with the command tmux reports
for it and the agent cc-watch resolved, and for each pane it did *not* recognise
it prints the processes running underneath — so the program that should have
been matched can be read straight off:

```
$ cc-watch --doctor
cc-watch doctor

config:  /home/u/.config/cc-watch/config.json (absent) — built-in defaults
agents:  claude, codex, gemini, opencode

PANE                      COMMAND         PID       AGENT
------------------------------------------------------------------------
work:0.0                  node            1855      gemini (process tree)
work:1.0                  claude          1864      claude (pane command)
work:2.0                  node            1874      -
work:3.0                  codex           1902      codex (pane command)
work:4.0                  opencode        1918      opencode (pane command)

4 pane(s) recognised as agents.

Processes under the panes that were not recognised. ...

  work:2.0 (node):
    1874     -bash
    2301     node /home/u/app/server.js
```

The two usual causes:

- **A config file from before the agent was supported.** `agent_commands`
  *replaces* the defaults rather than adding to them, so a file listing only
  `["claude"]` keeps Codex, Gemini and opencode panes out no matter what. `--doctor` prints
  the file in force and the agents it yields, and names any built-in agent the
  file has dropped.
- **An install layout that is not matched.** If the doctor shows the agent's
  process under an unrecognised pane but does not name it, its path is one
  cc-watch does not know: open an issue with that line.

## Configuration

Configuration is optional. To override the defaults, create
`~/.config/cc-watch/config.json`:

```json
{
  "agent_commands": ["claude", "codex", "gemini", "opencode", "aider"],
  "agent_icons": { "aider": "a" },
  "shell_prompts": ["$", "#", "%", "❯", "→", "λ"]
}
```

| Key              | Default                              | Description                                                                                                                    |
| ---------------- | ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------ |
| `agent_commands` | `["claude", "codex", "gemini", "opencode"]`      | Agents to watch. Matched case-insensitively against the pane command (`#{pane_current_command}`) and, for interpreter panes, against the program running below the pane. Add entries to watch other agent CLIs. |
| `agent_icons`    | `{"claude": "✻", "codex": "✵", "gemini": "✦", "opencode": "✶"}` | The mark shown before a session name, per agent. Unlike the other keys this is *merged over* the defaults rather than replacing them, so naming one agent leaves the rest alone. An agent with no icon gets its initial. |
| `shell_prompts`  | `["$", "#", "%", "❯", "→", "λ"]`     | Line suffixes that identify a bare shell prompt. Used to detect that an agent has exited into the shell (`error` state).         |

Any key may be omitted; a missing or empty list falls back to its default. If
the file is absent or cannot be parsed, all defaults are used.

## Tuning

The polling and classification constants are compile-time values at the top of
`main.go`:

```go
refreshInterval  = 2 * time.Second  // how often tmux is polled
idleThreshold    = 5 * time.Second  // unchanged output before idle/waiting
paneLines        = 80               // lines of scrollback captured per pane
promptScrollback = 400              // one-off deeper capture, to find the prompt
                                    // of a turn already running when a pane is
                                    // first seen
```

## License

See the repository for license information.
