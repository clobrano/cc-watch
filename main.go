package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/term"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
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
	// The fifth field is 1 when the pane is on someone's screen: the active
	// pane of the active window of a session with a client attached.
	paneFormat = "#{window_index}|#{pane_index}|#{pane_pid}|#{pane_current_command}|" +
		"#{&&:#{pane_active},#{&&:#{window_active},#{session_attached}}}|#{session_name}"

	// statusOption is the tmux user option the agent strip is published to.
	// Reference it as #{@cc_watch_agents} from status-right/status-left.
	statusOption = "@cc_watch_agents"

	// tailLines is how many trailing lines of each pane the daemon ships to
	// dashboards for the preview. It is the ceiling for preview_lines.
	tailLines = 60

	// defaultPreviewLines is how much of the selected pane the preview shows
	// when preview_lines is not configured (and the terminal is tall enough).
	defaultPreviewLines = 30
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
	// PreviewLines is how many of the selected pane's last lines are shown
	// below the list. The terminal height can show fewer; 0 means the default.
	PreviewLines int `json:"preview_lines"`
	// NotifyAfter is how many seconds a session with the bell on must stay
	// waiting after running before the notification is sent; 0 means the
	// default.
	NotifyAfter float64 `json:"notify_after"`
	// NotifyCommand replaces the built-in notifier: argv with {title} and
	// {body} substituted, e.g. ["ntfy", "publish", "mytopic", "{title}"].
	NotifyCommand []string `json:"notify_command"`
	// Icons picks the symbols the bell column is drawn with: "unicode"
	// (default), "nerd" or "ascii". See iconSets.
	Icons string `json:"icons"`
}

// defaultAgentIcons are the two-letter marks the agents are known by: CL for
// Claude Code, CX for Codex, GM for Gemini CLI, OC for opencode and PI for pi.
//
// These are ASCII on purpose. A single dingbat per agent reads well in one font
// and badly in the next: the "agent star" glyphs are drawn at different weights,
// some terminals render them East-Asian Wide, and four near-identical stars are
// hard to tell apart anyway. A two-letter ASCII code is exactly two columns in
// every monospace terminal, so the marks always line up and each names its agent
// outright. Override any of them with agent_icons for a glyph you prefer.
var defaultAgentIcons = map[string]string{
	"claude":   "CL",
	"codex":    "CX",
	"gemini":   "GM",
	"opencode": "OC",
	"pi":       "PI",
}

// agentIcon is the mark for an agent: configured, else built in, else the
// agent's uppercased initial, which tells two custom agents apart without any
// configuration at all.
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
	AgentCommands: []string{"claude", "codex", "gemini", "opencode", "pi"},
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

// previewLines is the configured preview depth, defaulted and capped at what
// the daemon sends.
func (c Config) previewLines() int {
	switch {
	case c.PreviewLines <= 0:
		return defaultPreviewLines
	case c.PreviewLines > tailLines:
		return tailLines
	}
	return c.PreviewLines
}

type State int

const (
	StateUnknown  State = iota
	StateStarting       // first poll, not yet classified
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
		return "yellow"
	case StateWaiting:
		return "green"
	case StateIdle:
		return "colour244"
	case StateStarting:
		return "blue"
	case StateError:
		return "red"
	default:
		return "colour244"
	}
}

func (s State) color() string {
	switch s {
	case StateStarting:
		return "\033[34m" // blue
	case StateActive:
		return "\033[33m" // yellow
	case StateIdle:
		return "\033[90m" // bright-black (gray)
	case StateWaiting:
		return "\033[32m" // green
	case StateError:
		return "\033[31m" // red
	default:
		return "\033[90m"
	}
}

var (
	ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-9;:?]*[a-zA-Z]|\][^\x07]*\x07|[PX^_][^\x1b]*\x1b\\|.)`)
	termWidth  = 80 // updated from terminal on start and on SIGWINCH
	termHeight = 24
)

type session struct {
	name       string
	agent      string // which configured agent this pane is running
	state      State
	desc       string
	prompt     string
	lastChange time.Time
	lastPane   string
	colorPane  string // lastPane as captured, colours included, for the preview
	lastTitle  string
	tail       []string // last lines of the pane, for the dashboard preview

	// Notifications, see notify.go.
	bell         bool      // the user asked to be told when this turn ends
	armed        bool      // seen running since the last time it settled waiting
	waitingSince time.Time // start of the current waiting stretch, if any
	unread       bool      // notified, and the user has not been to it since
	visible      bool      // on someone's screen in tmux right now
}

type keyEvent int

const (
	keyUp      keyEvent = -1
	keyDown    keyEvent = -2
	keyEnter   keyEvent = -3
	keyQuit    keyEvent = -4
	keyPreview keyEvent = -5
	keyBell    keyEvent = -6
)

var (
	cfg      Config
	sessions = map[string]*session{}
)

func main() {
	serve := flag.Bool("serve", false,
		"start the background daemon that polls tmux, publishes the status strip and serves dashboards (no dashboard of its own)")
	stop := flag.Bool("stop-server", false, "stop the running --serve daemon")
	doctor := flag.Bool("doctor", false,
		"report what cc-watch sees in every tmux pane, and why each was or was not taken for an agent")
	notifyTest := flag.Bool("notify-test", false,
		"send a test desktop notification the way the daemon would, and report any error")
	flag.Parse()

	if *serve && *stop {
		fmt.Fprintln(os.Stderr, "cc-watch: --serve and --stop-server are mutually exclusive")
		os.Exit(2)
	}

	cfg = loadConfig()

	switch {
	case *notifyTest:
		if err := runNotifyTest(); err != nil {
			fmt.Fprintln(os.Stderr, "cc-watch:", err)
			os.Exit(1)
		}
		return

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

	if err := runTUI(); err != nil {
		fmt.Fprintln(os.Stderr, "cc-watch:", err)
		os.Exit(1)
	}
}

// runTUI is the dashboard. It does no polling of its own: it connects to the
// daemon, starting one if none is running, and renders the snapshots the
// daemon streams — so it shows the current state on its first frame, and any
// number of dashboards share one poller.
func runTUI() error {
	conn, err := connectDaemon()
	if err != nil {
		return err
	}
	defer conn.Close()
	snaps := make(chan snapshot, 1)
	go readSnapshots(conn, snaps)
	// The daemon sends its latest snapshot on connect; wait for it so the first
	// frame is the real state rather than a flash of "no agent sessions".
	select {
	case snap, ok := <-snaps:
		if !ok {
			return errors.New("daemon closed the connection")
		}
		applySnapshot(snap)
	case <-time.After(stopTimeout):
		return errors.New("daemon sent no status")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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
	termWidth, termHeight = querySize(fd)

	// The status strip is left alone on the way out: the daemon publishes it,
	// and keeps running after the dashboard quits.
	defer func() {
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
	preview := true
	render := func(selected int) { renderDashboard(selected, preview) }
	render(selected)

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-winch:
			termWidth, termHeight = querySize(fd)
			fmt.Print("\033[2J")
			render(selected)

		case snap, ok := <-snaps:
			if !ok {
				return errors.New("lost connection to the daemon")
			}
			applySnapshot(snap)
			names := sortedNames()
			if selected >= len(names) && len(names) > 0 {
				selected = len(names) - 1
			}
			render(selected)

		case k := <-keys:
			switch k {
			case keyQuit:
				return nil

			case keyPreview:
				preview = !preview
				render(selected)

			case keyBell:
				names := sortedNames()
				if selected < len(names) {
					// The daemon answers with a fresh snapshot, which redraws.
					if err := sendCommand(conn, command{Bell: names[selected]}); err != nil {
						return fmt.Errorf("sending to the daemon: %w", err)
					}
				}

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
				// Going to the session reads its notification.
				if sessions[names[selected]].unread {
					sendCommand(conn, command{Seen: names[selected]})
				}
				attach(ctx, fd, names[selected], &rawState)
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

// querySize tries multiple fds to get the terminal size reliably.
func querySize(fd int) (int, int) {
	for _, f := range []int{fd, int(os.Stdout.Fd()), int(os.Stderr.Fd())} {
		if w, h, err := term.GetSize(f); err == nil && w > 10 {
			if h <= 0 {
				h = 24
			}
			return w, h
		}
	}
	return 80, 24
}

func readKeys(tty *os.File, ch chan<- keyEvent) {
	buf := make([]byte, 16)
	for {
		n, err := tty.Read(buf)
		if err != nil || n == 0 {
			return
		}
		b := buf[:n]
		// A read can hold several keys — typed quickly, pasted, or sent by
		// tmux send-keys — so walk all of it rather than only its first key.
		for i := 0; i < len(b); i++ {
			// Arrow keys arrive as ESC [ A (up) / ESC [ B (down).
			if b[i] == 0x1b && i+2 < len(b) && b[i+1] == '[' {
				switch b[i+2] {
				case 'A':
					ch <- keyUp
				case 'B':
					ch <- keyDown
				}
				i += 2
				continue
			}
			switch c := b[i]; c {
			case '\r', '\n':
				ch <- keyEnter
			case 'q', 'Q', 0x03 /* Ctrl-C */ :
				ch <- keyQuit
			case 'p', 'P':
				ch <- keyPreview
			case 'b', 'B':
				ch <- keyBell
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
	// '|' into — parsePane's field limit then keeps such a name intact.
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
		p, ok := parsePane(line)
		if !ok {
			continue
		}

		agent := agentForCommand(p.command)
		if agent == "" {
			// The pane command names no agent, but it may still be running one:
			// tmux reports whatever the foreground process calls itself, which
			// for Gemini CLI, pi and an npm-installed Codex is "node", and for an
			// agent behind a wrapper is the wrapper. Ask the process tree
			// instead of trusting the name.
			if !procsScanned {
				procs, procsScanned = scanProcesses(ctx), true
			}
			if agent = procs.agentInTree(p.pid); agent == "" {
				continue
			}
		}

		key := p.key // e.g. "notes:0.1"
		target := "=" + key
		seen[key] = true

		s, ok := sessions[key]
		if !ok {
			color, _ := captureColor(ctx, target, paneLines)
			s = &session{name: key, agent: agent, lastChange: time.Now(), state: StateStarting,
				lastPane: stripANSI(color), colorPane: color}
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
		s.visible = p.visible

		// Hold "..." until we have observed the pane for at least idleThreshold.
		if s.state == StateStarting && time.Since(s.lastChange) < idleThreshold {
			continue
		}

		title, _ := tmux(ctx, "display-message", "-p", "-t", target, "#{pane_title}")
		// The colours are only for the preview: everything below reads the
		// plain text, as it did before colours were captured.
		color, _ := captureColor(ctx, target, paneLines)
		pane := stripANSI(color)
		s.colorPane = color

		// The title joins the change check because classify keys on it: a live
		// spinner animates its frames, a stale one freezes, and only a changing
		// title keeps that pane Active.
		if pane != s.lastPane || title != s.lastTitle {
			s.lastChange = time.Now()
			s.lastPane = pane
			s.lastTitle = title
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
	// Braille spinner in the OSC title = Claude Code actively working. A live
	// spinner animates its frames, so a recent change (pane or title) backs the
	// claim; a spinner frozen past idleThreshold is a stale frame left after a
	// hang or crash, and the pane rules below decide instead.
	if t := strings.TrimSpace(title); t != "" {
		if r := []rune(t)[0]; r >= 0x2800 && r <= 0x28FF {
			if time.Since(lastChange) < idleThreshold {
				return StateActive
			}
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
		// pi's editor is neither shape: no box corners and no marker, just a
		// full-width rule above and below the input.
		if isRule(l) {
			return true
		}
	}
	return false
}

// isRule reports whether a line is a horizontal rule: box-drawing dashes end to
// end, with anything between them (a scroll hint such as "─── ↑ 2 more ───")
// allowed as long as no box corner or side makes it part of a box instead.
func isRule(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "─") || !strings.HasSuffix(t, "─") {
		return false
	}
	return !strings.ContainsAny(t, "╭╮╰╯│┌┐└┘")
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
	// Neither found: pi draws its editor between two horizontal rules, with a
	// cwd line and a token/cost/model statusline under it. Cut at the upper rule
	// of the bottom-most pair, so that neither the statusline nor a half-typed
	// draft is read as transcript. This runs only when the shapes above found
	// nothing, because Claude Code's borderless UI also draws rules around its
	// input and is already handled by its prompt marker.
	bottom := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if !isRule(lines[i]) {
			continue
		}
		if bottom < 0 {
			bottom = i
			continue
		}
		return lines[:i]
	}
	if bottom >= 0 {
		return lines[:bottom]
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

// Column widths for the dashboard.
//
// The session column grows to fit the longest name, from minNameWidth up to
// maxNameWidth, but never so far that the prompt column drops below
// minDescWidth — on a narrow terminal it gives the columns back, down to
// minNameWidth.
//
// Using ASCII ">" for the pointer — unicode triangles render as 2-column wide
// glyphs in most terminals, which breaks column alignment.
const (
	minNameWidth = 24
	maxNameWidth = 40
	minDescWidth = 30
	stateWidth   = 9
	gap          = 2
	rightMargin  = 3 // keeps lines from touching the edge
)

// prefixWidth is everything consumed before the description column, derived
// from the others so that adding or resizing a column cannot leave the
// description misaligned.
func prefixWidth(nameWidth int) int {
	return 2 + 2 + nameWidth + gap + stateWidth + gap
}

// nameColumnWidth sizes the session column for the given display names on a
// terminal w columns wide.
func nameColumnWidth(names []string, w int) int {
	nw := minNameWidth
	for _, n := range names {
		if l := len([]rune(n)); l > nw {
			nw = l
		}
	}
	if nw > maxNameWidth {
		nw = maxNameWidth
	}
	if fit := w - prefixWidth(0) - rightMargin - minDescWidth; nw > fit {
		nw = fit
	}
	if nw < minNameWidth {
		nw = minNameWidth
	}
	return nw
}

// displayNames are the session-column labels, in sortedNames order: the agent
// icon, then the tmux session name, or the full "session:W.P" key when that
// session has more than one agent pane.
func displayNames(names []string) []string {
	out := sessionNames(names)
	for i, key := range names {
		// The icon rides inside the session column rather than taking a
		// column of its own, which would cost the prompt column ten chars.
		out[i] = agentIcon(sessions[key].agent) + " " + out[i]
	}
	return out
}

// sessionNames are the names the user knows the panes by, in the order given:
// the tmux session name, or the full "session:W.P" key when that session has
// more than one agent pane.
func sessionNames(names []string) []string {
	sessionCount := map[string]int{}
	for _, key := range names {
		sessionCount[strings.SplitN(key, ":", 2)[0]]++
	}
	out := make([]string, len(names))
	for i, key := range names {
		name := strings.SplitN(key, ":", 2)[0]
		if sessionCount[name] > 1 {
			name = key
		}
		out[i] = name
	}
	return out
}

// iconSet is the pair of symbols drawn in the left margin of a row: one for a
// session with the bell on, one for a session whose notification has not
// been seen yet. Each is a single column, followed by a space.
type iconSet struct {
	bell, unread string
}

// iconSets are the choices for the icons setting.
//
// The app cannot bring a font with it — the terminal draws with the fonts of
// the machine it runs on — and no terminal reports whether a glyph actually
// rendered, so the default must be symbols every font can be relied on for.
// The geometric shapes below are in DejaVu Sans Mono (which Hack, Konsole's
// default, and Menlo derive from), JetBrains Mono, Liberation Mono and
// FreeMono, and DejaVu is the fallback font on nearly every Linux system.
//
// The Nerd Font glyphs are Font Awesome's bell and bell-outline, from the part
// of the Nerd Fonts range that kept its codepoints from v2 to v3; most of
// the Material Design icons were moved in v3.
var iconSets = map[string]iconSet{
	"unicode": {bell: "◇", unread: "◆"},
	"nerd":    {bell: "\uf0a2", unread: "\uf0f3"},
	"ascii":   {bell: "o", unread: "!"},
}

const defaultIcons = "unicode"

// icons is the configured icon set; an unknown name falls back to the default
// (and --doctor says so).
func icons() iconSet {
	if set, ok := iconSets[strings.ToLower(cfg.Icons)]; ok {
		return set
	}
	return iconSets[defaultIcons]
}

func renderDashboard(selected int, preview bool) {
	const (
		reset = "\033[0m"
		bold  = "\033[1m"
		dim   = "\033[2m"
	)

	// Re-query on every render so a wrong initial value self-corrects within one cycle.
	if fw, fh := querySize(int(os.Stdout.Fd())); fw > 10 {
		termWidth, termHeight = fw, fh
	}
	names := sortedNames()
	labels := displayNames(names)

	w := termWidth
	nameWidth := nameColumnWidth(labels, w)
	if w < prefixWidth(nameWidth)+10 {
		w = prefixWidth(nameWidth) + 10
	}
	descWidth := w - prefixWidth(nameWidth) - rightMargin
	sepWidth := w - 5 // indent(4) + 1 right margin

	var b strings.Builder
	b.WriteString("\033[2J\033[H") // clear entire screen, go home

	ts := time.Now().Format("15:04:05")
	fmt.Fprintf(&b, "%s  cc-watch%s  %s%s%s\n\n", bold, reset, dim, ts, reset)
	// "    " (4) = 2 spaces + pointer slot (2) — same as row prefix
	fmt.Fprintf(&b, "    %s%-*s  %-*s  %s%s\n", bold,
		nameWidth, "SESSION", stateWidth, "STATE", "LAST PROMPT", reset)
	fmt.Fprintf(&b, "    %s\n", strings.Repeat("─", sepWidth))
	used := 4 // title, blank, header, rule

	if len(names) == 0 {
		fmt.Fprintf(&b, "\n  %sno agent sessions found%s\n", dim, reset)
		used += 2
	} else {
		for i, key := range names {
			s := sessions[key]
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
			// The left margin carries the bell: a dim mark while it is on,
			// a bold one, with the name in bold too, for a notification not
			// seen yet.
			margin := "  "
			switch {
			case s.unread:
				margin = "\033[1;33m" + icons().unread + reset + " "
				nameStyle = bold
			case s.bell:
				margin = "\033[2;33m" + icons().bell + reset + " "
			}
			state := s.state.color() + fmt.Sprintf("%-*s", stateWidth, s.state.label()) + reset
			// margin (2) + pointer (2) = 4 chars before name, matches header indent
			fmt.Fprintf(&b, "%s%s%s%-*s%s  %s  %s%s%s\n",
				margin, pointer,
				nameStyle, nameWidth, truncate(labels[i], nameWidth), reset,
				state,
				dim, truncate(text, descWidth), reset,
			)
		}
		used += len(names)
	}

	// The preview takes whatever height the list and the footer leave, up to
	// preview_lines: two blanks + titled rule above it, blank + help line below,
	// and the line the cursor lands on after the help line.
	if preview && selected < len(names) {
		avail := termHeight - used - 6
		if n := min(cfg.previewLines(), avail); n >= 3 {
			s := sessions[names[selected]]
			// A heavy rule and a gutter in the selected row's state colour
			// set the preview apart from the list and tie it to its row.
			accent := s.state.color()
			title := truncate(labels[selected]+" · "+s.state.label(), max(0, sepWidth-5))
			fmt.Fprintf(&b, "\n\n  %s━━ %s%s%s%s %s%s\n",
				accent, bold, title, reset, accent,
				strings.Repeat("━", max(0, sepWidth-len([]rune(title))-2)), reset)
			gutter := "  " + accent + "│" + reset + " "
			tail := s.tail
			if len(tail) > n {
				tail = tail[len(tail)-n:]
			}
			if len(tail) == 0 {
				fmt.Fprintf(&b, "%s%s(no output captured yet)%s\n", gutter, dim, reset)
			}
			for _, l := range tail {
				fmt.Fprintf(&b, "%s%s%s\n", gutter, clipColumns(l, sepWidth-1), reset)
			}
		}
	}

	fmt.Fprintf(&b, "\n  %s↑↓ select   ↵ attach   b bell   p preview   q quit%s\n", dim, reset)
	b.WriteString("\033[J")

	// raw mode clears OPOST/ONLCR so \n no longer implies \r; use \r\n explicitly.
	fmt.Print(strings.ReplaceAll(b.String(), "\n", "\r\n"))
}

// sgrEscape matches a Select Graphic Rendition sequence — colours and text
// attributes — which is all capture-pane -e emits.
var sgrEscape = regexp.MustCompile(`^\x1b\[[0-9;:]*m`)

// paneTail is the last n lines of a capture, cleaned for the preview. Colours
// (SGR sequences) are kept; every other escape is dropped, tabs are expanded,
// other control characters blanked and trailing blank lines trimmed. Box
// drawing is kept — the agent's input box at the bottom is the clearest sign of
// whether it is done.
//
// Each line is printed on its own, followed by a reset, so it is given the
// attributes still in effect from the lines above it: a colour that runs
// across a line break looks the same in the preview as in the pane.
func paneTail(pane string, n int) []string {
	lines := strings.Split(strings.ReplaceAll(pane, "\r", ""), "\n")
	for len(lines) > 0 && strings.TrimSpace(stripANSI(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	first := max(0, len(lines)-n)
	var carry string // SGR sequences in effect since the last reset
	out := make([]string, 0, len(lines)-first)
	for i, l := range lines {
		var b strings.Builder
		b.WriteString(carry)
		col := 0
		for j := 0; j < len(l); {
			if l[j] == 0x1b {
				if m := sgrEscape.FindString(l[j:]); m != "" {
					b.WriteString(m)
					if m == "\x1b[m" || m == "\x1b[0m" {
						carry = ""
					} else {
						carry += m
					}
					j += len(m)
					continue
				}
				if m := ansiEscape.FindStringIndex(l[j:]); m != nil && m[0] == 0 {
					j += m[1]
					continue
				}
				j++
				continue
			}
			r, size := utf8.DecodeRuneInString(l[j:])
			j += size
			switch {
			case r == '\t':
				pad := 8 - col%8
				b.WriteString(strings.Repeat(" ", pad))
				col += pad
				continue
			case unicode.IsControl(r):
				r = ' '
			}
			b.WriteRune(r)
			col += runeColumns(r)
		}
		if i >= first {
			out = append(out, strings.TrimRight(b.String(), " "))
		}
	}
	return out
}

// clipColumns cuts s to at most n terminal columns, counting wide glyphs as
// two and escape sequences as none, so a preview line can never wrap and push
// the screen around.
func clipColumns(s string, n int) string {
	col := 0
	for i := 0; i < len(s); {
		if m := sgrEscape.FindString(s[i:]); m != "" {
			i += len(m)
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		cw := runeColumns(r)
		if col+cw > n {
			return s[:i]
		}
		col += cw
		i += size
	}
	return s
}

// runeColumns is how many terminal columns r takes: 0 for combining marks, 2
// for East Asian wide glyphs and emoji, 1 otherwise. It is an approximation of
// wcwidth, enough to keep preview lines inside the screen.
func runeColumns(r rune) int {
	switch {
	case unicode.Is(unicode.Mn, r), r == 0x200B, r == 0x200D, r >= 0xFE00 && r <= 0xFE0F:
		return 0
	case r >= 0x1100 && r <= 0x115F,
		r >= 0x2E80 && r <= 0xA4CF && r != 0x303F,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE4F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F,
		r >= 0x1F900 && r <= 0x1FAFF,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

// updateStatusBar publishes a numbered, state-coloured strip of the detected
// agents to a tmux user option, e.g. "1 2 3" with 1 yellow and 2 green. The
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

// captureColor is capture with the pane's colours and attributes kept as SGR
// escape sequences (-e).
func captureColor(ctx context.Context, target string, n int) (string, error) {
	return tmux(ctx, "capture-pane", "-p", "-e", "-t", target, "-S", fmt.Sprintf("-%d", n))
}

// stripANSI removes every escape sequence from a capture.
func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

func tmux(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.Output()
	return string(out), err
}
