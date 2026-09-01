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
