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
	"golang.org/x/term"
)

const (
	refreshInterval = 2 * time.Second
	idleThreshold   = 5 * time.Second
	paneLines       = 80

	// promptScrollback is how deep the one-off capture goes when a pane is first
	// seen, looking for the prompt that started the turn. A pane that has been
	// working for a while has pushed it well past the paneLines window, and
	// without this an agent already running when cc-watch starts shows no prompt
	// until the user types the next one.
	promptScrollback = 400

	// statusOption is the tmux user option the agent strip is published to.
	// Reference it as #{@cc_watch_agents} from status-right/status-left.
	statusOption = "@cc_watch_agents"
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

// tmuxColor mirrors color() using tmux style names, for the status bar strip.
// colour244 stands in for the TUI's bright-black: tmux only gained "bright*"
// names recently, while the 256-colour numbers work everywhere.
func (s State) tmuxColor() string {
	switch s {
	case StateActive:
		return "green"
	case StateIdle:
		return "yellow"
	case StateWaiting:
		return "cyan"
	case StateError:
		return "red"
	default:
		return "colour244"
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
	prompt     string
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
		clearStatusBar()
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
	updateStatusBar(ctx)
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
			updateStatusBar(ctx)
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
				updateStatusBar(ctx)
				render(selected)
			}
		}
	}
}

// attach jumps to the named tmux session. Inside tmux it uses switch-client
// (instant, TUI keeps running). Outside tmux it suspends the TUI, runs
// attach-session as a subprocess, then resumes when the user detaches.
func attach(ctx context.Context, fd int, key string, rawState **term.State) {
	target := "=" + key
	if os.Getenv("TMUX") != "" {
		exec.CommandContext(ctx, "tmux", "switch-client", "-t", target).Run()
		return
	}
	if *rawState != nil {
		term.Restore(fd, *rawState)
	}
	fmt.Print("\033[?25h\033[?1049l") // show cursor, exit alt screen → tmux takes over

	cmd := exec.Command("tmux", "attach-session", "-t", target)
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
	// One call lists every pane in every session with its running command.
	//
	// The fields are '|'-separated, not tab-separated: tmux sanitises control
	// characters out of format output (3.4 rewrites a tab to '_'), so a tab
	// delimiter yields one unsplittable field and no pane is ever matched.
	// session_name comes last because it is the only field a user can put a
	// '|' into — SplitN's 4-field limit then keeps such a name intact.
	out, err := tmux(ctx, "list-panes", "-a", "-F",
		"#{window_index}|#{pane_index}|#{pane_current_command}|#{session_name}")
	if err != nil {
		sessions = map[string]*session{}
		return
	}

	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 4)
		if len(fields) != 4 {
			continue
		}
		wIdx, pIdx, command, sname := fields[0], fields[1], fields[2], fields[3]
		if !isAgentCommand(strings.TrimSpace(command)) {
			continue
		}

		key := sname + ":" + wIdx + "." + pIdx // e.g. "notes:0.1"
		target := "=" + key
		seen[key] = true

		s, ok := sessions[key]
		if !ok {
			pane, _ := capture(ctx, target, paneLines)
			s = &session{name: key, lastChange: time.Now(), state: StateStarting, lastPane: pane}
			// Pay for one deep capture here, not on every poll: from now on the
			// prompt is sticky and the paneLines window is enough to notice a
			// newer one.
			if deep, err := capture(ctx, target, promptScrollback); err == nil {
				s.prompt = extractPrompt(deep)
			}
			sessions[key] = s
			continue
		}

		// Hold "..." until we have observed the pane for at least idleThreshold.
		if s.state == StateStarting && time.Since(s.lastChange) < idleThreshold {
			continue
		}

		title, _ := tmux(ctx, "display-message", "-p", "-t", target, "#{pane_title}")
		pane, _ := capture(ctx, target, paneLines)

		if pane != s.lastPane {
			s.lastChange = time.Now()
			s.lastPane = pane
		}

		s.state = classify(pane, strings.TrimSpace(title), s.lastChange)
		s.desc = extractDesc(pane)
		// Only overwrite on a hit: once the turn's output has pushed the prompt
		// out of the captured window we want to keep showing the last one we saw,
		// not blank the column for the rest of the turn.
		if p := extractPrompt(pane); p != "" {
			s.prompt = p
		}
	}

	for key := range sessions {
		if !seen[key] {
			delete(sessions, key)
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

// paneText splits a capture into lines with ANSI escapes and carriage returns
// removed, ready for the scanners below.
func paneText(pane string) []string {
	clean := ansiEscape.ReplaceAllString(pane, "")
	clean = strings.ReplaceAll(clean, "\r", "")
	return strings.Split(strings.TrimRight(clean, " \n\t"), "\n")
}

// promptMarkers are the glyphs Claude Code has used to introduce a prompt line:
// ASCII '>' in the older bordered UI, a chevron in the borderless one. Which one
// you get depends on the version, so match the family rather than betting on a
// single glyph.
var promptMarkers = []rune{'>', '❯', '⟩', '›', '〉'}

// promptMarker reports whether a line is a prompt line, returning its text. The
// marker must sit at column 0 and be followed by a space or nothing: that keeps
// out tool output containing a quoted line, which is always indented under a
// '⎿', and keeps out prose that merely starts with an angle bracket. A line
// carrying a '│' is inside the older UI's input box, not the transcript.
func promptMarker(line string) (string, bool) {
	r := []rune(strings.TrimRight(line, " \t"))
	if len(r) == 0 || strings.ContainsRune(line, '│') {
		return "", false
	}
	if len(r) > 1 && r[1] != ' ' {
		return "", false
	}
	for _, m := range promptMarkers {
		if r[0] == m {
			return strings.TrimSpace(string(r[1:])), true
		}
	}
	return "", false
}

// aboveInput cuts a pane down to its transcript, dropping the live input line
// and everything under it.
//
// The borderless UI marks the input with the same glyph it marks a submitted
// prompt with, so the two cannot be told apart by shape — but the input is
// always the bottom-most of them, which is anchor enough: above it is
// transcript, below it is only the mode-hint footer.
func aboveInput(lines []string) []string {
	for i := len(lines) - 1; i >= 0; i-- {
		// Whichever of the two turns up first scanning up from the footer is the
		// bottom of the input region — a box border in the older UI, a bare
		// prompt glyph in the borderless one. Checking both in one pass is what
		// keeps the older UI's submitted prompts, which are also column-0 marker
		// lines, from being mistaken for its input.
		if strings.ContainsRune(lines[i], '╰') {
			return lines[:i]
		}
		if _, ok := promptMarker(lines[i]); ok {
			return lines[:i]
		}
	}
	return lines
}

// extractPrompt returns the last prompt the user submitted, as Claude Code
// renders it in the transcript: a prompt line, plus the two-space-indented
// continuation lines it wraps long prompts onto.
func extractPrompt(pane string) string {
	// Dropping the live input line first is what stops a half-typed follow-up
	// from being reported as the last prompt.
	lines := aboveInput(paneText(pane))
	for i := len(lines) - 1; i >= 0; i-- {
		text, ok := promptMarker(lines[i])
		if !ok || text == "" {
			continue
		}
		parts := []string{text}
		for _, next := range lines[i+1:] {
			t := strings.TrimRight(next, " \t")
			// A blank line, a new transcript element or any box edge ends the
			// prompt; anything else indented to align under it is continuation.
			if !strings.HasPrefix(t, "  ") || strings.ContainsAny(t, "╭╮╰╯│⏺⎿") {
				break
			}
			parts = append(parts, t)
		}
		return toDisplay(strings.Join(parts, " "))
	}
	return ""
}

func extractDesc(pane string) string {
	// Same cut, for the same reason: without it this returns the mode-hint
	// footer ("accept edits on (shift+tab to cycle)"), which is the last line on
	// screen carrying no box-drawing characters and reads identically for every
	// agent.
	lines := aboveInput(paneText(pane))
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.ContainsAny(l, "╭╮╰╯│─╔╗╚╝═║") {
			continue
		}
		return toDisplay(l)
	}
	return ""
}

// toDisplay keeps printable ASCII plus Latin-1 Supplement and Latin Extended-A/B,
// so accented prompt text survives, and drops everything else — box drawing, the
// transcript glyphs, CJK and emoji — because those render two columns wide in
// most terminals and would break the table alignment. Whitespace runs collapse to
// a single space, which also tidies the gaps left where glyphs were dropped.
func toDisplay(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r <= 0x7E || r >= 0xA0 && r <= 0x24F {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
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
	fmt.Fprintf(&b, "    %s%-24s  %-9s  %s%s\n", bold, "SESSION", "STATE", "LAST PROMPT", reset)
	fmt.Fprintf(&b, "    %s\n", strings.Repeat("─", sepWidth))

	names := sortedNames()
	if len(names) == 0 {
		fmt.Fprintf(&b, "\n  %sno agent sessions found%s\n", dim, reset)
	} else {
		// Count agent panes per session; if >1, display the full "session:W.P" key.
		sessionCount := map[string]int{}
		for _, key := range names {
			sname := strings.SplitN(key, ":", 2)[0]
			sessionCount[sname]++
		}

		for i, key := range names {
			s := sessions[key]
			sname := strings.SplitN(key, ":", 2)[0]
			displayName := sname
			if sessionCount[sname] > 1 {
				displayName = key
			}

			pointer := "  "
			nameStyle := dim
			if i == selected {
				pointer = "> "
				nameStyle = reset
			}
			// Fall back to the last line of output for a pane whose prompt has
			// never been on screen while we were watching — an agent started
			// long before cc-watch, whose prompt is past promptScrollback.
			text := s.prompt
			if text == "" {
				text = s.desc
			}
			// "  " (2) + pointer (2) = 4 chars before name, matches header indent
			fmt.Fprintf(&b, "  %s%s%-24s%s  %s%-9s%s  %s%s%s\n",
				pointer,
				nameStyle, truncate(displayName, 24), reset,
				s.state.color(), s.state.label(), reset,
				dim, truncate(text, descWidth), reset,
			)
		}
	}

	fmt.Fprintf(&b, "\n  %s↑↓ select   ↵ attach   q quit%s\n", dim, reset)
	b.WriteString("\033[J")

	// raw mode clears OPOST/ONLCR so \n no longer implies \r; use \r\n explicitly.
	fmt.Print(strings.ReplaceAll(b.String(), "\n", "\r\n"))
}

// updateStatusBar publishes a numbered, state-coloured strip of the detected
// agents to a tmux user option, e.g. "1 2 3" with 1 green and 2 yellow. The
// numbers are positional over the same sorted list the TUI renders, so agent N
// in the status bar is row N in the dashboard — which also means they shift
// when a pane appears or dies. The option is inert until referenced, so the
// user places it themselves: set -ag status-right ' #{@cc_watch_agents}'.
func updateStatusBar(ctx context.Context) {
	var b strings.Builder
	for i, key := range sortedNames() {
		if i > 0 {
			b.WriteByte(' ')
		}
		// Trailing #[default] stops our colour leaking into whatever the user
		// has placed after the strip in their status format.
		fmt.Fprintf(&b, "#[fg=%s]%d", sessions[key].state.tmuxColor(), i+1)
	}
	if b.Len() > 0 {
		b.WriteString("#[default]")
	}
	setStatusOption(ctx, b.String())
}

var lastStatus string // last value pushed, so unchanged polls cause no redraw

// setStatusOption writes value to the user option and redraws clients, but only
// when the value actually changed: refresh-client -S on every 2s poll would
// force a status redraw for every attached client indefinitely.
func setStatusOption(ctx context.Context, value string) {
	if value == lastStatus {
		return
	}
	lastStatus = value
	if _, err := tmux(ctx, "set-option", "-g", statusOption, value); err != nil {
		return
	}
	tmux(ctx, "refresh-client", "-S")
}

// clearStatusBar unsets the option on exit so a frozen strip does not linger in
// the status bar. It builds its own commands rather than going through tmux()
// because the context is already cancelled by the time this runs.
func clearStatusBar() {
	if err := exec.Command("tmux", "set-option", "-gu", statusOption).Run(); err != nil {
		return
	}
	exec.Command("tmux", "refresh-client", "-S").Run()
}

func sortedNames() []string {
	names := make([]string, 0, len(sessions))
	for name := range sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// truncate cuts to n columns, counting runes rather than bytes: prompt text can
// carry multi-byte characters, and slicing those by byte both overcounts the
// width and can split a rune into garbage.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
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

// capture reads the last n lines of a pane, scrollback included.
func capture(ctx context.Context, target string, n int) (string, error) {
	return tmux(ctx, "capture-pane", "-p", "-t", target, "-S", fmt.Sprintf("-%d", n))
}

func tmux(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.Output()
	return string(out), err
}
