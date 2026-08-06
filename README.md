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

| State     | Meaning                                                                            |
| --------- | ---------------------------------------------------------------------------------- |
| `...`     | Pane seen for the first time; held until enough output has been observed to classify |
| `running` | The agent is working — a braille spinner in the pane title, or output still changing |
| `waiting` | The Claude Code prompt box is on screen and nothing has changed for 5 seconds        |
| `idle`    | Output has been unchanged for 5 seconds with no prompt box on screen                 |
| `error`   | The tail of the pane is a bare shell prompt — the agent exited                       |
| `unknown` | The pane is empty                                                                    |

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
