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
	if got := truncate("✻ notes", minNameWidth); got != "✻ notes" {
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

// piPane is the tail of an idle pi pane: the transcript, the editor between two
// rules, then the cwd line and the token/cost/model statusline. pi draws no
// prompt glyph and no box, and its user messages are set apart by background
// colour only.
const piPane = `
 add a retry to the fetch helper

 Added a retry with backoff to fetchJSON; tests pass.

─────────────────────────────────────────────────────────────
 half-typed follow-up
─────────────────────────────────────────────────────────────
~/project (main)
↑12k ↓3.4k R40k $0.123 12.3%/200k              claude-sonnet-4
`

func TestExtractDescSkipsThePiStatusline(t *testing.T) {
	// Before the editor's rules were recognised this returned the statusline.
	want := "Added a retry with backoff to fetchJSON; tests pass."
	if got := extractDesc(piPane); got != want {
		t.Errorf("extractDesc() = %q, want %q", got, want)
	}
	if got := extractPrompt(piPane); got != "" {
		t.Errorf("extractPrompt() = %q, want none", got)
	}
}

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
	// Nor in pi: its editor is only a pair of rules above the statusline.
	if !hasAgentPrompt(piPane) {
		t.Error("hasAgentPrompt(piPane) = false, want true")
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
		{"stale spinner frame over a settled prompt", geminiPane, "⠋ Working", stale, StateWaiting},
		{"stale spinner frame over dead output", "Editing src/parser.go", "⠋ Working", stale, StateIdle},
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

	if got := agentIcon("claude"); got != "CL" {
		t.Errorf("agentIcon(claude) = %q, want %q", got, "CL")
	}
	if got := agentIcon("codex"); got != "CX" {
		t.Errorf("agentIcon(codex) = %q, want %q", got, "CX")
	}
	if got := agentIcon("GEMINI"); got != "GM" {
		t.Errorf("agentIcon is not case-insensitive: got %q", got)
	}
	if got := agentIcon("opencode"); got != "OC" {
		t.Errorf("agentIcon(opencode) = %q, want %q", got, "OC")
	}
	if got := agentIcon("pi"); got != "PI" {
		t.Errorf("agentIcon(pi) = %q, want %q", got, "PI")
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
	if got := agentIcon("gemini"); got != "GM" {
		t.Errorf("configuring one icon disturbed another: got %q", got)
	}
	cfg = defaultConfig
}

func TestDefaultIconsAreASCII(t *testing.T) {
	// The marks sit in a fixed-width column, so every character must be plain
	// printable ASCII — exactly one terminal column in any monospace font. A
	// multi-byte glyph (a dingbat star, an emoji) can render East-Asian Wide or
	// at an inconsistent weight and break the alignment, which is why the star
	// marks were dropped for two-letter codes.
	for agent, icon := range defaultAgentIcons {
		if icon == "" {
			t.Errorf("icon for %s is empty", agent)
			continue
		}
		for _, r := range icon {
			if r < 0x20 || r > 0x7E {
				t.Errorf("icon for %s (%q) has non-ASCII rune %U; marks must be ASCII to stay one column wide", agent, icon, r)
			}
		}
	}
}

func TestNameColumnWidth(t *testing.T) {
	long := strings.Repeat("x", 60)
	cases := []struct {
		names []string
		w     int
		want  int
	}{
		{[]string{"CL notes"}, 120, minNameWidth},                                      // short names keep the minimum
		{[]string{"CL " + strings.Repeat("a", 30)}, 120, 33},                           // grows to fit
		{[]string{"CL " + long}, 200, maxNameWidth},                                    // capped
		{[]string{"CL " + long}, 80, 80 - prefixWidth(0) - rightMargin - minDescWidth}, // leaves the prompt room
		{[]string{"CL " + long}, 40, minNameWidth},                                     // never below the minimum
	}
	for _, c := range cases {
		if got := nameColumnWidth(c.names, c.w); got != c.want {
			t.Errorf("nameColumnWidth(%q, %d) = %d, want %d", c.names, c.w, got, c.want)
		}
	}
}

func TestPaneTail(t *testing.T) {
	pane := "one\n\x1b[31mtwo\x1b[0m\tx\n╭────╮\n│ >  │\n╰────╯\n\n\n"
	got := paneTail(pane, 4)
	want := []string{"\x1b[31mtwo\x1b[0m     x", "╭────╮", "│ >  │", "╰────╯"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("paneTail = %q, want %q", got, want)
	}
	if got := paneTail("  \n\n", 4); got != nil {
		t.Errorf("paneTail of a blank pane = %q, want nil", got)
	}
}

func TestClipColumns(t *testing.T) {
	if got := clipColumns("hello", 3); got != "hel" {
		t.Errorf("clipColumns = %q", got)
	}
	// A wide glyph that would straddle the edge is dropped whole.
	if got := clipColumns("ab漢字", 3); got != "ab" {
		t.Errorf("clipColumns wide = %q", got)
	}
	if got := clipColumns("│ > │", 10); got != "│ > │" {
		t.Errorf("clipColumns box = %q", got)
	}
}

func TestPaneTailColors(t *testing.T) {
	// A colour left open at a line break is carried onto the next line; a
	// reset ends it; non-SGR escapes are dropped.
	pane := "\x1b[1;32mgreen\nstill\x1b[0m\nplain\x1b]0;title\x07\n"
	got := paneTail(pane, 3)
	want := []string{"\x1b[1;32mgreen", "\x1b[1;32mstill\x1b[0m", "plain"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("paneTail = %q, want %q", got, want)
	}
	// The carry is worked out from lines above the window too.
	if got := paneTail(pane, 1); len(got) != 1 || got[0] != "plain" {
		t.Errorf("paneTail(1) = %q", got)
	}
	if got := paneTail("\x1b[31mred\nmore", 1); got[0] != "\x1b[31mmore" {
		t.Errorf("carry into window = %q", got)
	}
}

func TestClipColumnsSkipsEscapes(t *testing.T) {
	if got := clipColumns("\x1b[31mhello\x1b[0m", 3); got != "\x1b[31mhel" {
		t.Errorf("clipColumns = %q", got)
	}
	if got := clipColumns("\x1b[31mhi\x1b[0m", 5); got != "\x1b[31mhi\x1b[0m" {
		t.Errorf("clipColumns kept = %q", got)
	}
}

func TestIcons(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	for name, want := range map[string]iconSet{
		"":        iconSets["unicode"],
		"nerd":    iconSets["nerd"],
		"ASCII":   iconSets["ascii"],
		"bogus":   iconSets["unicode"],
		"unicode": {bell: "◇", unread: "◆"},
	} {
		cfg.Icons = name
		if got := icons(); got != want {
			t.Errorf("icons() with %q = %+v, want %+v", name, got, want)
		}
	}
	// Every symbol must fit the one column the margin gives it.
	for name, set := range iconSets {
		for _, s := range []string{set.bell, set.unread} {
			if w := len([]rune(s)); w != 1 || runeColumns([]rune(s)[0]) != 1 {
				t.Errorf("%s icon %q is not a single one-column rune", name, s)
			}
		}
	}
}

// claudeBorderlessPane is Claude Code's current UI: no box, the input marked
// by a chevron between two rules.
const claudeBorderlessPane = `
❯ add a retry to the fetch helper

● Added a retry with backoff to fetchJSON; tests pass.

─────────────────────────────────────────────────────────────
❯ 
─────────────────────────────────────────────────────────────
  ? for shortcuts
`

func TestActivityKeyIgnoresTyping(t *testing.T) {
	cases := []struct {
		name, idle, typed string
	}{
		{"gemini", geminiPane, strings.Replace(geminiPane,
			"│ > Type your message or @path/to/file                        │",
			"│ > fix the flaky test in the integration suite please        │\n"+
				"│   and then run it ten times                                 │", 1)},
		{"claude bordered", claudePane, strings.Replace(strings.Replace(claudePane,
			"│ >                                                           │",
			"│ > now update the README                                     │", 1),
			"? for shortcuts", "esc to clear", 1)},
		{"claude borderless", claudeBorderlessPane, strings.Replace(claudeBorderlessPane,
			"❯ \n", "❯ now update the README and\n  the changelog too\n", 1)},
		{"codex", codexPane, strings.Replace(strings.Replace(codexPane,
			"› Ask Codex to do anything", "› now update the README", 1),
			"← for agents · ? for shortcuts", "⏎ send", 1)},
		{"pi", piPane, strings.Replace(piPane,
			" half-typed follow-up", " half-typed follow-up, now longer", 1)},
	}
	for _, c := range cases {
		if c.idle == c.typed {
			t.Fatalf("%s: the typed fixture is unchanged; fix the test", c.name)
		}
		if activityKey(c.idle) != activityKey(c.typed) {
			t.Errorf("%s: typing changed the activity key:\n%q\n%q", c.name,
				activityKey(c.idle), activityKey(c.typed))
		}
	}
}

func TestActivityKeySeesTheAgentWork(t *testing.T) {
	cases := []struct {
		name, before, after string
	}{
		{"gemini spinner", strings.Replace(geminiPane, "Loaded cached credentials.", "⠋ Thinking (esc to cancel, 1s)", 1),
			strings.Replace(geminiPane, "Loaded cached credentials.", "⠙ Thinking (esc to cancel, 2s)", 1)},
		{"claude output", claudePane, strings.Replace(claudePane, "Done. Tests pass.", "Done. Tests pass.\n● Running go vet", 1)},
		{"claude borderless spinner",
			strings.Replace(claudeBorderlessPane, "tests pass.\n", "tests pass.\n\n✻ Pondering… (esc to interrupt)\n", 1),
			strings.Replace(claudeBorderlessPane, "tests pass.\n", "tests pass.\n\n✶ Pondering… (esc to interrupt)\n", 1)},
		{"codex", codexPane, strings.Replace(codexPane, "covers 429.", "covers 429.\n\n• Working (3s • esc to interrupt)", 1)},
	}
	for _, c := range cases {
		if activityKey(c.before) == activityKey(c.after) {
			t.Errorf("%s: the agent's output did not change the activity key", c.name)
		}
	}
}
