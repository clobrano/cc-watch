package main

import "testing"

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
			pane: "> first thing\n\n⏺ Done.\n\n> second thing\n\n⏺ On it.\n",
			want: "second thing",
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
			pane: "\x1b[1m> \x1b[0mmake the parser stricter\n",
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
}
