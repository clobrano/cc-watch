package main

import (
	"strings"
	"testing"
	"time"
)

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

func TestHasAgentPrompt(t *testing.T) {
	cfg = defaultConfig

	// The box is above the footer, so the last line alone never finds it.
	if !hasAgentPrompt(geminiPane) {
		t.Error("hasAgentPrompt(geminiPane) = false, want true")
	}
	if !hasAgentPrompt(claudePane) {
		t.Error("hasAgentPrompt(claudePane) = false, want true")
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
	if got := agentIcon("GEMINI"); got != "✦" {
		t.Errorf("agentIcon is not case-insensitive: got %q", got)
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

func TestTruncate(t *testing.T) {
	if got := truncate("✻ notes", nameWidth); got != "✻ notes" {
		t.Errorf("truncate shortened a name that fits: %q", got)
	}
	// Counting bytes rather than runes would cut this short and could split
	// the icon into mojibake.
	if got, want := truncate("✻ abcdefghij", 8), "✻ abc..."; got != want {
		t.Errorf("truncate = %q, want %q", got, want)
	}
	if got, want := truncate("✻ abcdefghij", 2), "✻ "; got != want {
		t.Errorf("truncate = %q, want %q", got, want)
	}
}
