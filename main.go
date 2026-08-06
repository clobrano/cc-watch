package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

const (
	refreshInterval = 2 * time.Second
	idleThreshold   = 5 * time.Second
	paneLines       = 80
)

// Config is loaded from ~/.config/cc-watch/config.json.
type Config struct {
	// ShellPrompts are suffixes that identify a bare shell prompt line.
	ShellPrompts []string `json:"shell_prompts"`
	// AgentCommands are process names (#{pane_current_command}) to watch.
	AgentCommands []string `json:"agent_commands"`
}

var defaultConfig = Config{
	ShellPrompts:  []string{"$", "#", "%", "❯", "→", "λ"},
	AgentCommands: []string{"claude"},
}

func loadConfig() Config {
	home, err := os.UserHomeDir()
	if err != nil {
		return defaultConfig
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "cc-watch", "config.json"))
	if err != nil {
		return defaultConfig
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return defaultConfig
	}
	if len(c.ShellPrompts) == 0 {
		c.ShellPrompts = defaultConfig.ShellPrompts
	}
	if len(c.AgentCommands) == 0 {
		c.AgentCommands = defaultConfig.AgentCommands
	}
	return c
}

type State int

const (
	StateUnknown State = iota
	StateStarting // first poll, not yet classified
	StateActive
	StateIdle
	StateWaiting
	StateError
)

func (s State) label() string {
	switch s {
	case StateStarting:
		return "..."
	case StateActive:
		return "running"
	case StateIdle:
		return "idle"
	case StateWaiting:
		return "waiting"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

func (s State) color() string {
	switch s {
	case StateStarting:
		return "\033[90m"
	case StateActive:
		return "\033[32m"
	case StateIdle:
		return "\033[33m"
	case StateWaiting:
		return "\033[36m"
	case StateError:
		return "\033[31m"
	default:
		return "\033[90m"
	}
}

var (
	ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-9;]*[a-zA-Z]|\][^\x07]*\x07|[PX^_][^\x1b]*\x1b\\|.)`)
	termWidth  = 80 // updated from terminal on start and on SIGWINCH
)

type session struct {
	name       string
	state      State
	desc       string
	lastChange time.Time
	lastPane   string
}

type keyEvent int

const (
	keyUp    keyEvent = -1
	keyDown  keyEvent = -2
	keyEnter keyEvent = -3
	keyQuit  keyEvent = -4
)

var (
	cfg      Config
	sessions = map[string]*session{}
)

func main() {
	cfg = loadConfig()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// Use /dev/tty for terminal operations so they work regardless of how
	// stdin/stdout are connected (e.g. inside tmux, under wrappers, etc.).
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		tty = os.Stdin // best-effort fallback
	}
	fd := int(tty.Fd())

	var rawState *term.State
	if term.IsTerminal(fd) {
		rawState, _ = term.MakeRaw(fd)
	}
	termWidth = queryWidth(fd)

	defer func() {
		cancel()
		if rawState != nil {
			term.Restore(fd, rawState)
		}
		fmt.Print("\033[?25h\033[?1049l") // show cursor, exit alternate screen
	}()

	keys := make(chan keyEvent, 4)
	if rawState != nil {
		go readKeys(tty, keys)
	}

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)

	fmt.Print("\033[?1049h\033[?25l") // enter alternate screen, hide cursor

	selected := 0
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	update(ctx)
	render(selected)

	for {
		select {
		case <-ctx.Done():
			return

		case <-winch:
			termWidth = queryWidth(fd)
			fmt.Print("\033[2J")
			render(selected)

		case <-ticker.C:
			update(ctx)
			names := sortedNames()
			if selected >= len(names) && len(names) > 0 {
				selected = len(names) - 1
			}
			render(selected)

		case k := <-keys:
			switch k {
			case keyQuit:
				return

			case keyUp:
				if selected > 0 {
					selected--
					render(selected)
				}

			case keyDown:
				names := sortedNames()
				if selected < len(names)-1 {
					selected++
					render(selected)
				}

			case keyEnter:
				names := sortedNames()
				if len(names) == 0 || selected >= len(names) {
					break
				}
				attach(ctx, fd, names[selected], &rawState)
				update(ctx)
				render(selected)
			}
		}
	}
}

// attach jumps to the named tmux session. Inside tmux it uses switch-client
// (instant, TUI keeps running). Outside tmux it suspends the TUI, runs
// attach-session as a subprocess, then resumes when the user detaches.
func attach(ctx context.Context, fd int, name string, rawState **term.State) {
	if os.Getenv("TMUX") != "" {
		exec.CommandContext(ctx, "tmux", "switch-client", "-t", name).Run()
		return
	}
	if *rawState != nil {
		term.Restore(fd, *rawState)
	}
	fmt.Print("\033[?25h\033[?1049l") // show cursor, exit alt screen → tmux takes over

	cmd := exec.Command("tmux", "attach-session", "-t", "="+name)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	if *rawState != nil {
		*rawState, _ = term.MakeRaw(fd)
	}
	fmt.Print("\033[?1049h\033[?25l") // re-enter alt screen, hide cursor
}

// queryWidth tries multiple fds to get the terminal width reliably.
func queryWidth(fd int) int {
	for _, f := range []int{fd, int(os.Stdout.Fd()), int(os.Stderr.Fd())} {
		if w, _, err := term.GetSize(f); err == nil && w > 10 {
			return w
		}
	}
	return 80
}

func readKeys(tty *os.File, ch chan<- keyEvent) {
	buf := make([]byte, 16)
	for {
		n, err := tty.Read(buf)
		if err != nil || n == 0 {
			return
		}
		b := buf[:n]
		// Arrow keys arrive as ESC [ A (up) / ESC [ B (down).
		if len(b) >= 3 && b[0] == 0x1b && b[1] == '[' {
			switch b[2] {
			case 'A':
				ch <- keyUp
			case 'B':
				ch <- keyDown
			}
			continue
		}
		for _, c := range b {
			switch c {
			case '\r', '\n':
				ch <- keyEnter
			case 'q', 'Q', 0x03 /* Ctrl-C */ :
				ch <- keyQuit
			}
		}
	}
}

func update(ctx context.Context) {
	out, err := tmux(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		sessions = map[string]*session{}
		return
	}

	seen := map[string]bool{}
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if name == "" {
			continue
		}

		command, _ := tmux(ctx, "display-message", "-p", "-t", exactPane(name), "#{pane_current_command}")
		if !isAgentCommand(strings.TrimSpace(command)) {
			continue
		}
		seen[name] = true

		s, ok := sessions[name]
		if !ok {
			s = &session{name: name, lastChange: time.Now(), state: StateStarting}
			sessions[name] = s
			continue // show "..." for one cycle before classifying
		}

		title, _ := tmux(ctx, "display-message", "-p", "-t", exactPane(name), "#{pane_title}")
		pane, _ := tmux(ctx, "capture-pane", "-p", "-t", exactPane(name), "-S", fmt.Sprintf("-%d", paneLines))

		if pane != s.lastPane {
			s.lastChange = time.Now()
			s.lastPane = pane
		}

		s.state = classify(pane, strings.TrimSpace(title), s.lastChange)
		s.desc = extractDesc(pane)
	}

	for name := range sessions {
		if !seen[name] {
			delete(sessions, name)
		}
	}
}

func classify(pane, title string, lastChange time.Time) State {
	// Braille spinner in the OSC title = Claude Code actively working.
	if t := strings.TrimSpace(title); t != "" {
		if r := []rune(t)[0]; r >= 0x2800 && r <= 0x28FF {
			return StateActive
		}
	}

	trimmed := strings.TrimRight(pane, " \n\t")
	if trimmed == "" {
		return StateUnknown
	}

	tail := lastNonEmptyLine(trimmed)
	switch {
	case looksLikeClaudePrompt(tail):
		if time.Since(lastChange) >= idleThreshold {
			return StateWaiting
		}
		return StateActive
	case looksLikeShellPrompt(tail):
		return StateError
	default:
		if time.Since(lastChange) >= idleThreshold {
			return StateIdle
		}
		return StateActive
	}
}

func looksLikeClaudePrompt(line string) bool {
	if !strings.ContainsAny(line, "╭╮╰╯") {
		return false
	}
	hits := 0
	for _, ch := range "╭╮╰╯│─>" {
		if strings.ContainsRune(line, ch) {
			hits++
		}
	}
	return hits >= 2
}

func looksLikeShellPrompt(line string) bool {
	t := strings.TrimSpace(line)
	if looksLikeClaudePrompt(t) {
		return false
	}
	for _, suffix := range cfg.ShellPrompts {
		if strings.HasSuffix(t, suffix) {
			return true
		}
	}
	return false
}

func isAgentCommand(cmd string) bool {
	for _, ac := range cfg.AgentCommands {
		if strings.EqualFold(cmd, ac) {
			return true
		}
	}
	return false
}

func extractDesc(pane string) string {
	clean := ansiEscape.ReplaceAllString(pane, "")
	clean = strings.ReplaceAll(clean, "\r", "")
	lines := strings.Split(strings.TrimRight(clean, " \n\t"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.ContainsAny(l, "╭╮╰╯│─╔╗╚╝═║") {
			continue
		}
		return l // caller truncates to fit the terminal
	}
	return ""
}

// layout constants: chars consumed before the description column.
// "  " (2) + "> " (2) + name (24) + "  " (2) + state (9) + "  " (2) = 41
// Using ASCII ">" for the pointer — unicode triangles render as 2-column wide
// glyphs in most terminals, which breaks column alignment.
const prefixWidth = 41

func render(selected int) {
	const (
		reset = "\033[0m"
		bold  = "\033[1m"
		dim   = "\033[2m"
	)

	// Re-query on every render so a wrong initial value self-corrects within one cycle.
	if fresh := queryWidth(int(os.Stdout.Fd())); fresh > 10 {
		termWidth = fresh
	}
	w := termWidth
	if w < prefixWidth+10 {
		w = prefixWidth + 10
	}
	descWidth := w - prefixWidth - 3 // 3-char right margin keeps lines from touching the edge
	sepWidth := w - 5                // indent(4) + 1 right margin

	var b strings.Builder
	b.WriteString("\033[2J\033[H") // clear entire screen, go home

	ts := time.Now().Format("15:04:05")
	fmt.Fprintf(&b, "%s  cc-watch%s  %s%s%s\n\n", bold, reset, dim, ts, reset)
	// "    " (4) = 2 spaces + pointer slot (2) — same as row prefix
	fmt.Fprintf(&b, "    %s%-24s  %-9s  %s%s\n", bold, "SESSION", "STATE", "LAST OUTPUT", reset)
	fmt.Fprintf(&b, "    %s\n", strings.Repeat("─", sepWidth))

	names := sortedNames()
	if len(names) == 0 {
		fmt.Fprintf(&b, "\n  %sno agent sessions found%s\n", dim, reset)
	} else {
		for i, name := range names {
			s := sessions[name]
			pointer := "  "
			nameStyle := dim
			if i == selected {
				pointer = "> "
				nameStyle = reset
			}
			// "  " (2) + pointer (2) = 4 chars before name, matches header indent
			fmt.Fprintf(&b, "  %s%s%-24s%s  %s%-9s%s  %s%s%s\n",
				pointer,
				nameStyle, truncate(name, 24), reset,
				s.state.color(), s.state.label(), reset,
				dim, runewidth.Truncate(s.desc, descWidth, "…"), reset,
			)
		}
	}

	fmt.Fprintf(&b, "\n  %s↑↓ select   ↵ attach   q quit%s\n", dim, reset)
	b.WriteString("\033[J")

	fmt.Print(b.String())
}

func sortedNames() []string {
	names := make([]string, 0, len(sessions))
	for name := range sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func exactPane(name string) string { return "=" + name + ":" }

func truncate(s string, n int) string {
	return runewidth.Truncate(s, n, "…")
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

func tmux(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.Output()
	return string(out), err
}
