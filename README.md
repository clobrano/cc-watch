# cc-watch

A terminal dashboard for Claude Code sessions running inside tmux.

When you have several agents working in parallel across tmux windows and panes, it
is hard to tell which one is still thinking, which one is waiting for your input,
and which one has quit. `cc-watch` polls tmux, classifies every agent pane, and
lets you jump straight to the one that needs you.

```
  cc-watch  15:04:05

    SESSION                   STATE      LAST OUTPUT
    ─────────────────────────────────────────────────────────────────────
    notes                     running    Editing src/parser.go
  > api:0.1                   waiting    Do you want to make this edit?
    api:1.0                   idle       Done. Tests pass.
    scratch                   error      $
```

## How it works

Every 2 seconds `cc-watch` runs `tmux list-panes -a` to enumerate every pane in
every session, keeps the ones whose current command matches a configured agent
command (`claude` by default), and inspects each of them:

- the pane title, via `tmux display-message -p '#{pane_title}'`
- the last 80 lines of output, via `tmux capture-pane -p`

From those two signals it derives a state:

| State     | Colour | Meaning                                                                     |
| --------- | ------ | ---------------------------------------------------------------------------- |
| `...`     | grey   | Pane seen for the first time; held until enough output has been observed to classify |
| `running` | green  | The agent is working — a braille spinner in the pane title, or output still changing |
| `waiting` | cyan   | The Claude Code prompt box is on screen and nothing has changed for 5 seconds |
| `idle`    | yellow | Output has been unchanged for 5 seconds with no prompt box on screen          |
| `error`   | red    | The tail of the pane is a bare shell prompt — the agent exited                |
| `unknown` | grey   | The pane is empty                                                            |

The `LAST OUTPUT` column shows the last non-decorative line of the pane, with ANSI
escapes and box-drawing characters stripped, so you can see what each agent is
doing at a glance.

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
| `--foreground`  | With `--serve`, run the daemon loop in this process instead of detaching |

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

`--foreground` runs the loop in the current process, which is what a service
manager wants:

```ini
[Service]
ExecStart=%h/go/bin/cc-watch --serve --foreground
Restart=on-failure
```

## Configuration

Configuration is optional. To override the defaults, create
`~/.config/cc-watch/config.json`:

```json
{
  "agent_commands": ["claude", "aider"],
  "shell_prompts": ["$", "#", "%", "❯", "→", "λ"]
}
```

| Key              | Default                              | Description                                                                                                                    |
| ---------------- | ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------ |
| `agent_commands` | `["claude"]`                         | Pane commands (`#{pane_current_command}`) to treat as agents. Matched case-insensitively. Add entries to watch other agent CLIs. |
| `shell_prompts`  | `["$", "#", "%", "❯", "→", "λ"]`     | Line suffixes that identify a bare shell prompt. Used to detect that an agent has exited into the shell (`error` state).         |

Either key may be omitted; a missing or empty list falls back to its default. If
the file is absent or cannot be parsed, all defaults are used.

## Tuning

The polling and classification constants are compile-time values at the top of
`main.go`:

```go
refreshInterval = 2 * time.Second  // how often tmux is polled
idleThreshold   = 5 * time.Second  // unchanged output before idle/waiting
paneLines       = 80               // lines of scrollback captured per pane
```

## License

See the repository for license information.
