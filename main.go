package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"golang.org/x/term"
)

const (
	refreshInterval = 2 * time.Second
	idleThreshold   = 5 * time.Second
	paneLines       = 80

	// promptTailLines is how many trailing non-empty lines are searched for the
	// agent's input. Every agent draws a footer under it — a shortcut hint, a
	// cwd/model line, a context-left gauge — so the input is usually a few lines
	// up rather than the last line of the pane.
	promptTailLines = 5

	// promptScrollback is how deep the one-off capture goes when a pane is first
	// seen, looking for the prompt that started the turn. A pane that has been
	// working for a while has pushed it well past the paneLines window, and
	// without this an agent already running when cc-watch starts shows no prompt
	// until the user types the next one.
	promptScrollback = 400

	// paneFormat is the one list-panes query both the poller and --doctor use.
	paneFormat = "#{window_index}|#{pane_index}|#{pane_pid}|#{pane_current_command}|#{session_name}"

	// statusOption is the tmux user option the agent strip is published to.
	// Reference it as #{@cc_watch_agents} from status-right/status-left.
	statusOption = "@cc_watch_agents"
)

// Config is loaded from ~/.config/cc-watch/config.json.
type Config struct {
	// ShellPrompts are suffixes that identify a bare shell prompt line.
	ShellPrompts []string `json:"shell_prompts"`
	// AgentCommands are agent names to watch. A name is matched against the
	// pane's command (#{pane_current_command}) and, when that command is only
	// an interpreter, against the argv of the processes below the pane.
	AgentCommands []string `json:"agent_commands"`
	// AgentIcons overrides the glyph shown before a session name, per agent.
	// Unlike agent_commands this is merged over the built-in icons rather than
	// replacing them, so naming one agent leaves the others alone. It is also
	// the escape hatch for a terminal that renders the defaults at double
	// width: give the agent an ASCII icon such as "c".
	AgentIcons map[string]string `json:"agent_icons"`
}

// defaultAgentIcons are the marks the agents are known by: Claude Code prints
// U+273B itself, U+2726 is the four-pointed star of the Gemini mark, U+2735 is a
// pinwheel star for Codex, whose own mark — the ">_" of its header — is already
// the dashboard's selection pointer and could not be reused, and U+2738 is an
// eight-pointed star for opencode, which has no ASCII mark of its own to borrow.
//
// All four are deliberate choices. Each is East-Asian-width Neutral and has no
// emoji presentation, so terminals draw them one column wide. The obvious
// alternatives do not: U+2728 SPARKLES is Wide, U+2733 EIGHT SPOKED ASTERISK
// has an emoji form a terminal may draw at double width, and the ambiguous
// width of the triangles is what kept ">" as the selection pointer.
var defaultAgentIcons = map[string]string{
	"claude":   "✻",
	"codex":    "✵",
	"gemini":   "✦",
	"opencode": "✸",
}

// agentIcon is the glyph for an agent: configured, else built in, else the
// agent's initial, which is always one column and tells two custom agents
// apart without any configuration at all.
func agentIcon(agent string) string {
	if icon, ok := lookupFold(cfg.AgentIcons, agent); ok {
		return icon
	}
	if icon, ok := lookupFold(defaultAgentIcons, agent); ok {
		return icon
	}
	for _, r := range agent {
		return string(unicode.ToUpper(r))
	}
	return " "
}

// lookupFold reads m by case-insensitive key, matching how agent names are
// compared everywhere else.
func lookupFold(m map[string]string, key string) (string, bool) {
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

var defaultConfig = Config{
	ShellPrompts:  []string{"$", "#", "%", "❯", "→", "λ"},
	AgentCommands: []string{"claude", "codex", "gemini", "opencode"},
}

// configPath is the optional config file. It is empty if there is no home
// directory to look in.
func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "cc-watch", "config.json")
}

func loadConfig() Config {
	path := configPath()
	if path == "" {
		return defaultConfig
	}
	data, err := os.ReadFile(path)
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
	agent      string // which configured agent this pane is running
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
	serve := flag.Bool("serve", false,
		"run in the background as a daemon, publishing only the tmux status strip (no dashboard)")
	stop := flag.Bool("stop-server", false, "stop the running --serve daemon")
	doctor := flag.Bool("doctor", false,
		"report what cc-watch sees in every tmux pane, and why each was or was not taken for an agent")
	flag.Parse()

	if *serve && *stop {
		fmt.Fprintln(os.Stderr, "cc-watch: --serve and --stop-server are mutually exclusive")
		os.Exit(2)
	}

	cfg = loadConfig()

	switch {
	case *doctor:
		if err := runDoctor(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "cc-watch:", err)
			os.Exit(1)
		}
		return

	case *stop:
		if err := stopServer(); err != nil {
			fmt.Fprintln(os.Stderr, "cc-watch:", err)
			os.Exit(1)
		}
		return

	case *serve:
		// The backgrounded child re-enters here with daemonEnv set, and runs
		// the loop instead of spawning yet another copy of itself.
		run := startDaemon
		if os.Getenv(daemonEnv) != "" {
			run = runServe
		}
		if err := run(); err != nil {
			fmt.Fprintln(os.Stderr, "cc-watch:", err)
			os.Exit(1)
		}
		return
	}

	runTUI()
}

func runTUI() {
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
		// Leave the strip alone if a daemon is also publishing it: unsetting
		// the option here would blank the status bar until the daemon's next
		// *change* of value, which may be minutes away.
		if !daemonRunning() {
			clearStatusBar()
		}
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
	// '|' into — SplitN's 5-field limit then keeps such a name intact.
	out, err := tmux(ctx, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		sessions = map[string]*session{}
		return
	}

	seen := map[string]bool{}
	// procs is taken lazily, at most once per poll, and only once some pane has
	// failed to name its agent: a screen of self-naming agents never pays for it.
	var procs *procTable
	procsScanned := false

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 5)
		if len(fields) != 5 {
			continue
		}
		wIdx, pIdx, panePID, command, sname := fields[0], fields[1], fields[2], fields[3], fields[4]

		agent := agentForCommand(strings.TrimSpace(command))
		if agent == "" {
			// The pane command names no agent, but it may still be running one:
			// tmux reports whatever the foreground process calls itself, which
			// for Gemini CLI and an npm-installed Codex is "node", and for an
			// agent behind a wrapper is the wrapper. Ask the process tree
			// instead of trusting the name.
			if !procsScanned {
				procs, procsScanned = scanProcesses(ctx), true
			}
			pid, err := strconv.Atoi(strings.TrimSpace(panePID))
			if err != nil {
				continue
			}
			if agent = procs.agentInTree(pid); agent == "" {
				continue
			}
		}

		key := sname + ":" + wIdx + "." + pIdx // e.g. "notes:0.1"
		target := "=" + key
		seen[key] = true

		s, ok := sessions[key]
		if !ok {
			pane, _ := capture(ctx, target, paneLines)
			s = &session{name: key, agent: agent, lastChange: time.Now(), state: StateStarting, lastPane: pane}
			// Pay for one deep capture here, not on every poll: from now on the
			// prompt is sticky and the paneLines window is enough to notice a
			// newer one.
			if deep, err := capture(ctx, target, promptScrollback); err == nil {
				s.prompt = extractPrompt(deep)
			}
			sessions[key] = s
			continue
		}
		s.agent = agent

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

	// The shell prompt is checked first: an agent that has exited leaves its
	// box on screen above the shell prompt that replaced it, and that pane is
	// dead, not waiting.
	switch {
	case looksLikeShellPrompt(lastNonEmptyLine(trimmed)):
		return StateError
	case hasAgentPrompt(trimmed):
		if time.Since(lastChange) >= idleThreshold {
			return StateWaiting
		}
		return StateActive
	default:
		if time.Since(lastChange) >= idleThreshold {
			return StateIdle
		}
		return StateActive
	}
}

// hasAgentPrompt reports whether the agent is sitting at its input, looking at
// the last few non-empty lines rather than only the last one.
//
// Two shapes count, because the agents no longer agree on one: a box drawn
// around the input, and a bare marker line with nothing around it. Codex has
// only ever drawn the second — its composer is a "› " at column 0 above a
// context-left footer — and newer Claude Code builds have moved to it too, which
// is why a settled pane of either used to be reported idle rather than waiting.
func hasAgentPrompt(pane string) bool {
	// paneText, not a plain split: the marker on a live input line is styled,
	// and the escape sequence in front of it would push it off column 0.
	lines := paneText(pane)
	for i, checked := len(lines)-1, 0; i >= 0 && checked < promptTailLines; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		checked++
		if looksLikeAgentPrompt(l) {
			return true
		}
		// promptMarker is deliberately strict — column 0, a space or nothing
		// after the glyph, no box border on the line — because down here a
		// transcript line quoting an angle bracket is all that could be mistaken
		// for an input.
		if _, ok := promptMarker(lines[i]); ok {
			return true
		}
	}
	return false
}

// looksLikeAgentPrompt reports whether a line is part of an agent's input box.
// Claude Code's older UI and Gemini CLI both draw one with the same rounded
// box-drawing characters, so one test covers both.
func looksLikeAgentPrompt(line string) bool {
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
	if looksLikeAgentPrompt(t) {
		return false
	}
	for _, suffix := range cfg.ShellPrompts {
		if strings.HasSuffix(t, suffix) {
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

// promptMarkers are the glyphs an agent introduces a prompt line with: ASCII '>'
// in Claude Code's older bordered UI, a chevron in the borderless one, U+203A in
// Codex, which renders a submitted prompt and its wrapped continuations in
// exactly that shape. Which one you get depends on the agent and its version, so
// match the family rather than betting on a single glyph.
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

// extractPrompt returns the last prompt the user submitted, as the agents render
// it in the transcript: a prompt line, plus the two-space-indented continuation
// lines a long prompt wraps onto — a shape Claude Code and Codex share.
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

// Column widths for the dashboard. prefixWidth is everything consumed before
// the description column, derived from the others so that adding or resizing a
// column cannot leave the description misaligned.
//
// Using ASCII ">" for the pointer — unicode triangles render as 2-column wide
// glyphs in most terminals, which breaks column alignment.
const (
	nameWidth   = 24
	stateWidth  = 9
	gap         = 2
	prefixWidth = 2 + 2 + nameWidth + gap + stateWidth + gap
)

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
	fmt.Fprintf(&b, "    %s%-*s  %-*s  %s%s\n", bold,
		nameWidth, "SESSION", stateWidth, "STATE", "LAST PROMPT", reset)
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
			// The icon rides inside the session column rather than taking a
			// column of its own, which would cost the prompt column ten chars.
			displayName = agentIcon(s.agent) + " " + displayName

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
			fmt.Fprintf(&b, "  %s%s%-*s%s  %s%-*s%s  %s%s%s\n",
				pointer,
				nameStyle, nameWidth, truncate(displayName, nameWidth), reset,
				s.state.color(), stateWidth, s.state.label(), reset,
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
// carry multi-byte characters and every session name is prefixed with a
// three-byte agent icon, so slicing by byte would both overcount the width and
// split a rune into garbage.
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
