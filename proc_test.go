package main

import "testing"

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
		// The script is not always the first argument after the interpreter.
		{"node --import /etc/setup.js /usr/local/bin/gemini", "gemini"},
		{"node /home/u/.local/bin/claude", "claude"},
		{"node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js", "claude"},
		// Codex: the npm shim, the native binary it spawns out of the platform
		// package, and a standalone install that renames nothing.
		{"node /usr/lib/node_modules/@openai/codex/bin/codex.js", "codex"},
		{"/usr/lib/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex", "codex"},
		{"/home/u/.local/bin/codex --model gpt-5", "codex"},
		// opencode: a native binary either way — the npm package lays it down at
		// node_modules/opencode-ai/bin/opencode.exe, brew and the install script
		// drop a plain "opencode". No node interpreter runs it.
		{"/usr/lib/node_modules/opencode-ai/bin/opencode.exe", "opencode"},
		{"/home/u/.opencode/bin/opencode", "opencode"},
		// Not agents: the name only appears in a later argument, in a directory
		// the program happens to live under, or not at all.
		{"node build.js gemini.json", ""},
		{"node /home/u/gemini-experiments/server.js", ""},
		{"node /home/u/codex-experiments/server.js", ""},
		{"node /home/u/opencode-experiments/server.js", ""},
		{"node build.js codex.json", ""},
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

	// pane shell (100) -> wrapper (200) -> node running gemini (300), a second
	// pane shell (500) -> the Codex npm shim (600) -> the native binary it spawns
	// (700), and a third pane shell (800) -> the opencode native binary (900),
	// which npm lays down under its own package directory.
	tbl := &procTable{
		args: map[int]string{
			100: "-bash",
			200: "/bin/sh /usr/local/bin/start-agent",
			300: "node /usr/lib/node_modules/@google/gemini-cli/dist/index.js",
			400: "node /srv/app/server.js",
			500: "-zsh",
			600: "node /usr/lib/node_modules/@openai/codex/bin/codex.js",
			700: "/usr/lib/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex",
			800: "-bash",
			900: "/usr/lib/node_modules/opencode-ai/bin/opencode.exe",
		},
		children: map[int][]int{100: {200}, 200: {300}, 500: {600}, 600: {700}, 800: {900}},
	}
	if got := tbl.agentInTree(100); got != "gemini" {
		t.Errorf("agentInTree(100) = %q, want %q", got, "gemini")
	}
	if got := tbl.agentInTree(500); got != "codex" {
		t.Errorf("agentInTree(500) = %q, want %q", got, "codex")
	}
	if got := tbl.agentInTree(800); got != "opencode" {
		t.Errorf("agentInTree(800) = %q, want %q", got, "opencode")
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

func TestParsePane(t *testing.T) {
	// session_name comes last in the format precisely so a '|' in it survives.
	p, ok := parsePane("2|1|4711|node|my|session")
	if !ok {
		t.Fatal("parsePane failed on a well-formed line")
	}
	if want := "my|session:2.1"; p.key != want {
		t.Errorf("key = %q, want %q", p.key, want)
	}
	if p.command != "node" || p.pid != 4711 {
		t.Errorf("command/pid = %q/%d, want %q/%d", p.command, p.pid, "node", 4711)
	}
	if _, ok := parsePane("2|1|node|session"); ok {
		t.Error("parsePane accepted a short line")
	}
	if _, ok := parsePane("2|1|not-a-pid|node|session"); ok {
		t.Error("parsePane accepted a non-numeric pid")
	}
}
