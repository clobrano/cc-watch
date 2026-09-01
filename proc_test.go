package main

import "testing"

func TestIsInterpreter(t *testing.T) {
	for _, cmd := range []string{"node", "NODE", "python3.13", "bun", "npx"} {
		if !isInterpreter(cmd) {
			t.Errorf("isInterpreter(%q) = false, want true", cmd)
		}
	}
	for _, cmd := range []string{"claude", "bash", "zsh", "vim", "nodemon"} {
		if isInterpreter(cmd) {
			t.Errorf("isInterpreter(%q) = true, want false", cmd)
		}
	}
}

func TestAgentInArgs(t *testing.T) {
	cfg = defaultConfig

	tests := []struct {
		args string
		want string
	}{
		// The two shapes an npm install of Gemini CLI produces.
		{"node /home/u/.npm-global/bin/gemini", "gemini"},
		{"node /usr/lib/node_modules/@google/gemini-cli/dist/index.js", "gemini"},
		{"/opt/node/bin/node --max-old-space-size=4096 /usr/local/bin/gemini", "gemini"},
		{"node /home/u/.local/bin/claude", "claude"},
		{"node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js", "claude"},
		// Not agents: the name only appears in a later argument, in a directory
		// the program happens to live under, or not at all.
		{"node build.js gemini.json", ""},
		{"node /home/u/gemini-experiments/server.js", ""},
		{"node /tmp/claude-0/scratch/server.js", ""},
		{"node /srv/app/server.js", ""},
		{"python3 train.py --config gemini.yaml", ""},
		// A prefix is not a match.
		{"node /usr/bin/geminids", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := agentInArgs(tt.args); got != tt.want {
			t.Errorf("agentInArgs(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestAgentInTree(t *testing.T) {
	cfg = defaultConfig

	// pane shell (100) -> wrapper (200) -> node running gemini (300)
	tbl := &procTable{
		args: map[int]string{
			100: "-bash",
			200: "/bin/sh /usr/local/bin/start-agent",
			300: "node /usr/lib/node_modules/@google/gemini-cli/dist/index.js",
			400: "node /srv/app/server.js",
		},
		children: map[int][]int{100: {200}, 200: {300}},
	}
	if got := tbl.agentInTree(100); got != "gemini" {
		t.Errorf("agentInTree(100) = %q, want %q", got, "gemini")
	}
	if got := tbl.agentInTree(400); got != "" {
		t.Errorf("agentInTree(400) = %q, want %q", got, "")
	}
	var nilTable *procTable
	if got := nilTable.agentInTree(100); got != "" {
		t.Errorf("agentInTree on nil table = %q, want %q", got, "")
	}
}

func TestAgentInTreeCycleTerminates(t *testing.T) {
	cfg = defaultConfig
	// A malformed table where two pids parent each other must not loop forever.
	tbl := &procTable{
		args:     map[int]string{1: "init", 2: "sh"},
		children: map[int][]int{1: {2}, 2: {1}},
	}
	if got := tbl.agentInTree(1); got != "" {
		t.Errorf("agentInTree = %q, want %q", got, "")
	}
}

func TestCutField(t *testing.T) {
	pid, rest := cutField("  1234   99 node /bin/gemini --flag")
	if pid != "1234" {
		t.Errorf("pid = %q, want %q", pid, "1234")
	}
	ppid, args := cutField(rest)
	if ppid != "99" {
		t.Errorf("ppid = %q, want %q", ppid, "99")
	}
	// The argv keeps its spaces: it is the remainder, not a field.
	if want := "node /bin/gemini --flag"; args != want {
		t.Errorf("args = %q, want %q", args, want)
	}
}
