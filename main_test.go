package main

import (
	"strings"
	"testing"
	"time"
)

// runningPane is a Claude Code pane mid-turn: a submitted prompt that wrapped
// onto a second line, some transcript, the spinner status line, the empty input
// box and the mode-hint footer.
const runningPane = "" +
	"> refactor the retry loop in client.go and add a test for the 429 case, then\n" +
	"  update the README\n" +
	"\n" +
	"⏺ I'll start by reading the client.\n" +
	"\n" +
	"⏺ Read(client.go)\n" +
	"  ⎿  Read 210 lines\n" +
	"\n" +
	"✢ Smooshing… (18m 44s · ↓ 35.6k tokens)\n" +
	"\n" +
	"╭────────────────────────────────────────────╮\n" +
	"│ >                                          │\n" +
	"╰────────────────────────────────────────────╯\n" +
	"  ⏵⏵ accept edits on (shift+tab to cycle)          ⧉ for agents\n"

// borderlessPane is the newer Claude Code UI: no box around the input, a chevron
// marking both the submitted prompt and the live input line, and the mode-hint
// footer under it.
const borderlessPane = "" +
	"✻ recap: Added YouTube playlist polling to wui.\n" +
	"\n" +
	"⟩ it looks great, commit and push\n" +
	"\n" +
	"⏺ Bash(git push)\n" +
	"  ⎿  To github.com:clobrano/wui.git\n" +
	"       b870c0f..3faa53e  main → main\n" +
	"\n" +
	"⏺ Pushed. Commit 3faa53e — feat: YouTube playlist poller.\n" +
	"\n" +
	"✻ Brewed for 49m 7s\n" +
	"\n" +
	"⟩\n" +
	"  ⏵⏵ accept edits on (shift+tab to cycle)\n"

func TestExtractPrompt(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want string
	}{
		{
			name: "joins a wrapped prompt",
			pane: runningPane,
			want: "refactor the retry loop in client.go and add a test for the 429 case, then update the README",
		},
		{
			name: "ignores text typed into the input box but not yet sent",
			pane: "> the prompt I actually sent\n" +
				"\n" +
				"⏺ Working on it.\n" +
				"\n" +
				"╭─────────────────────────────────╮\n" +
				"│ > half-written follow up        │\n" +
				"╰─────────────────────────────────╯\n",
			want: "the prompt I actually sent",
		},
		{
			name: "takes the most recent of several prompts",
			pane: "⟩ first thing\n\n⏺ Done.\n\n⟩ second thing\n\n⏺ On it.\n\n⟩\n",
			want: "second thing",
		},
		{
			name: "reads a Codex prompt and its wrapped continuation",
			pane: codexPane,
			want: "rewrite the retry loop and add a test for the 429 case, then update the README",
		},
		{
			name: "reads a chevron prompt in the borderless UI",
			pane: borderlessPane,
			want: "it looks great, commit and push",
		},
		{
			name: "ignores borderless input holding an unsent follow up",
			pane: "⟩ the prompt I actually sent\n" +
				"\n" +
				"⏺ Working on it.\n" +
				"\n" +
				"⟩ half-written follow up\n" +
				"  ⏵⏵ accept edits on (shift+tab to cycle)\n",
			want: "the prompt I actually sent",
		},
		{
			name: "ignores a quoted line inside tool output",
			pane: "⏺ Bash(git log -1)\n" +
				"  ⎿  commit abc123\n" +
				"     > not a prompt, just quoted text\n",
			want: "",
		},
		{
			name: "strips ANSI escapes",
			pane: "\x1b[1m⟩ \x1b[0mmake the parser stricter\n\n⏺ On it.\n\n⟩\n",
			want: "make the parser stricter",
		},
		{
			name: "reports nothing when the prompt has scrolled away",
			pane: "⏺ Update(main.go)\n  ⎿  Updated main.go with 3 additions\n",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractPrompt(tt.pane); got != tt.want {
				t.Errorf("extractPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractDescSkipsTheModeHintFooter(t *testing.T) {
	// Without the footer cut this returns "accept edits on (shift+tab to cycle)
	// for agents" — the same line for every agent, on every poll.
	want := "Smooshing (18m 44s · 35.6k tokens)"
	if got := extractDesc(runningPane); got != want {
		t.Errorf("extractDesc() = %q, want %q", got, want)
	}
}

func TestExtractDescSkipsTheFooterInTheBorderlessUI(t *testing.T) {
	want := "Brewed for 49m 7s"
	if got := extractDesc(borderlessPane); got != want {
		t.Errorf("extractDesc() = %q, want %q", got, want)
	}
}

func TestExtractDescSkipsTheCodexFooter(t *testing.T) {
	// Without the cut this returns the context-left footer, which says nothing
	// about what this session is doing.
	want := "Done. The retry loop now backs off and the test covers 429."
	if got := extractDesc(codexPane); got != want {
		t.Errorf("extractDesc() = %q, want %q", got, want)
	}
}

func TestExtractDescFallsBackWhenNoInputBoxIsOnScreen(t *testing.T) {
	pane := "⏺ Bash(go test ./...)\n  ⎿  ok  github.com/clobrano/cc-watch  0.4s\n"
	want := "ok github.com/clobrano/cc-watch 0.4s"
	if got := extractDesc(pane); got != want {
		t.Errorf("extractDesc() = %q, want %q", got, want)
	}
}

func TestToDisplay(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain ascii", "plain ascii"},
		{"perché è così", "perché è così"},       // accents survive
		{"ship it 🚀 now", "ship it now"},         // wide glyphs dropped
		{"⏺ Update(main.go)", "Update(main.go)"}, // transcript glyph dropped
		{"a\t b    c", "a b c"},                  // whitespace collapsed
		{"  leading and trailing  ", "leading and trailing"},
	}
	for _, tt := range tests {
		if got := toDisplay(tt.in); got != tt.want {
			t.Errorf("toDisplay(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTruncateCountsRunesNotBytes(t *testing.T) {
	// Nine runes, eleven bytes: truncating by byte would cut this short and
	// could split a rune into replacement characters.
	s := "però però"
	if got := truncate(s, 9); got != s {
		t.Errorf("truncate(%q, 9) = %q, want it returned whole", s, got)
	}
	if got, want := truncate(s, 7), "però..."; got != want {
		t.Errorf("truncate(%q, 7) = %q, want %q", s, got, want)
	}

	// The same applies to a session name, which always carries a three-byte
	// agent icon in its first column.
	if got := truncate("✻ notes", nameWidth); got != "✻ notes" {
		t.Errorf("truncate shortened a name that fits: %q", got)
	}
	if got, want := truncate("✻ abcdefghij", 8), "✻ abc..."; got != want {
		t.Errorf("truncate = %q, want %q", got, want)
	}
}

// geminiPane is the tail of an idle Gemini CLI pane: the input box, then the
// footer line it draws underneath.
const geminiPane = `
Loaded cached credentials.

╭─────────────────────────────────────────────────────────────╮
│ > Type your message or @path/to/file                        │
╰─────────────────────────────────────────────────────────────╯

~/project (main*)   no sandbox (see /docs)   gemini-2.5-pro (98% context left)
`

// claudePane is the equivalent for Claude Code, whose footer is a hint line.
const claudePane = `
Done. Tests pass.

╭─────────────────────────────────────────────────────────────╮
│ >                                                           │
╰─────────────────────────────────────────────────────────────╯
  ? for shortcuts
`

// codexPane is the tail of an idle Codex pane. Codex draws no box at all: the
// composer is a "› " at column 0 carrying placeholder text, with the shortcut
// and context-left footer under it, and a submitted prompt is the same glyph
// again, wrapping onto two-space-indented continuations.
const codexPane = `
› rewrite the retry loop and add a test for the 429 case, then update
  the README

• Explored
  └ Read client.go

• Done. The retry loop now backs off and the test covers 429.

› Ask Codex to do anything
  ← for agents · ? for shortcuts                     100% context left
`

func TestHasAgentPrompt(t *testing.T) {
	cfg = defaultConfig

	// The box is above the footer, so the last line alone never finds it.
	if !hasAgentPrompt(geminiPane) {
		t.Error("hasAgentPrompt(geminiPane) = false, want true")
	}
	if !hasAgentPrompt(claudePane) {
		t.Error("hasAgentPrompt(claudePane) = false, want true")
	}
	// No box to find: the composer is a bare marker line, and finding it is what
	// tells a settled Codex or borderless Claude Code pane from an idle one.
	if !hasAgentPrompt(codexPane) {
		t.Error("hasAgentPrompt(codexPane) = false, want true")
	}
	if !hasAgentPrompt(borderlessPane) {
		t.Error("hasAgentPrompt(borderlessPane) = false, want true")
	}
	// A styled marker still sits at column 0 once the escapes are stripped.
	if !hasAgentPrompt("• Done.\n\n\x1b[1m› \x1b[0m\n  ? for shortcuts\n") {
		t.Error("hasAgentPrompt did not strip ANSI before looking for the marker")
	}
	// Transcript output that merely quotes an angle bracket is not an input.
	if hasAgentPrompt("⏺ Bash(git log -1)\n  ⎿  commit abc123\n     > quoted, not a prompt\n") {
		t.Error("hasAgentPrompt(quoted output) = true, want false")
	}
	if hasAgentPrompt("make: *** [build] Error 1\n$ ") {
		t.Error("hasAgentPrompt(shell output) = true, want false")
	}
}

func TestClassify(t *testing.T) {
	cfg = defaultConfig

	stale := time.Now().Add(-time.Minute)
	fresh := time.Now()

	tests := []struct {
		name       string
		pane       string
		title      string
		lastChange time.Time
		want       State
	}{
		{"gemini settled at its prompt", geminiPane, "", stale, StateWaiting},
		{"codex settled at its composer", codexPane, "", stale, StateWaiting},
		{"codex still typing out", codexPane, "", fresh, StateActive},
		{"codex exited to the shell", codexPane + "\nuser@host:~/project$ ", "", stale, StateError},
		{"claude settled at its prompt", claudePane, "", stale, StateWaiting},
		{"prompt on screen but still typing out", claudePane, "", fresh, StateActive},
		{"spinner in title", geminiPane, "⠋ Working", fresh, StateActive},
		{"output still moving", "Editing src/parser.go", "", fresh, StateActive},
		{"output stopped", "Editing src/parser.go", "", stale, StateIdle},
		{"agent exited to the shell", geminiPane + "\nuser@host:~/project$ ", "", stale, StateError},
		{"empty pane", "   \n\n", "", stale, StateUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.pane, tt.title, tt.lastChange); got != tt.want {
				t.Errorf("classify() = %v, want %v", got.label(), tt.want.label())
			}
		})
	}
}

func TestLooksLikeShellPrompt(t *testing.T) {
	cfg = defaultConfig

	if !looksLikeShellPrompt("user@host:~/project$") {
		t.Error("shell prompt not recognised")
	}
	// A box line ending in a prompt-like character is not a shell prompt.
	if looksLikeShellPrompt(strings.TrimSpace("│ > %                                    │")) {
		t.Error("agent box mistaken for a shell prompt")
	}
}

func TestAgentIcon(t *testing.T) {
	cfg = defaultConfig

	if got := agentIcon("claude"); got != "✻" {
		t.Errorf("agentIcon(claude) = %q, want %q", got, "✻")
	}
	if got := agentIcon("codex"); got != "✵" {
		t.Errorf("agentIcon(codex) = %q, want %q", got, "✵")
	}
	if got := agentIcon("GEMINI"); got != "✦" {
		t.Errorf("agentIcon is not case-insensitive: got %q", got)
	}
	if got := agentIcon("opencode"); got != "✶" {
		t.Errorf("agentIcon(opencode) = %q, want %q", got, "✶")
	}
	// An agent with no icon of its own falls back to its initial, which still
	// tells two custom agents apart.
	if got := agentIcon("aider"); got != "A" {
		t.Errorf("agentIcon(aider) = %q, want %q", got, "A")
	}
	if got := agentIcon(""); got != " " {
		t.Errorf("agentIcon(\"\") = %q, want a space", got)
	}

	// A configured icon wins — this is the escape hatch for a terminal that
	// draws the defaults badly — and leaves the other agents alone.
	cfg = Config{
		AgentCommands: defaultConfig.AgentCommands,
		AgentIcons:    map[string]string{"claude": "c"},
	}
	if got := agentIcon("claude"); got != "c" {
		t.Errorf("configured icon ignored: got %q, want %q", got, "c")
	}
	if got := agentIcon("gemini"); got != "✦" {
		t.Errorf("configuring one icon disturbed another: got %q", got)
	}
	cfg = defaultConfig
}

func TestDefaultIconsAreSingleColumn(t *testing.T) {
	// Every row's columns line up only if the icon is exactly one column wide.
	// Guard the built-ins against being swapped for a wide or emoji glyph —
	// U+2728 SPARKLES is East-Asian Wide, and the dingbats below carry an emoji
	// presentation that terminals may draw at double width.
	emojiDingbats := map[rune]bool{0x2728: true, 0x2733: true, 0x2734: true, 0x2747: true}
	for agent, icon := range defaultAgentIcons {
		r := []rune(icon)
		if len(r) != 1 {
			t.Errorf("icon for %s is %d runes, want 1", agent, len(r))
			continue
		}
		if emojiDingbats[r[0]] || r[0] >= 0x1F300 {
			t.Errorf("icon for %s (%U) has an emoji presentation and may render double width", agent, r[0])
		}
	}
}
