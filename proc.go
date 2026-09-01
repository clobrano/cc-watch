package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// interpreterCommands are pane commands that say nothing about which agent is
// running. An agent CLI shipped as a script is exec'd through its interpreter,
// so tmux reports the interpreter's name unless the agent renames its own
// process. Claude Code does rename itself and shows up as "claude"; Gemini CLI
// does not, so every Gemini pane appears as "node" and no amount of
// agent_commands tuning can match it. When a pane's command is one of these, we
// look at the process tree below the pane for an argv that names an agent.
var interpreterCommands = []string{
	"node", "nodejs", "bun", "deno",
	"npm", "npx", "pnpm", "yarn",
	"uv", "uvx", "ruby", "perl",
}

// isInterpreter reports whether cmd is a runtime we should look behind.
func isInterpreter(cmd string) bool {
	c := strings.ToLower(strings.TrimSpace(cmd))
	// python, python3, python3.13, … all count.
	if strings.HasPrefix(c, "python") {
		return true
	}
	for _, i := range interpreterCommands {
		if c == i {
			return true
		}
	}
	return false
}

// agentForCommand returns the configured agent name a pane command matches, or
// "" if it matches none.
func agentForCommand(cmd string) string {
	for _, ac := range cfg.AgentCommands {
		if strings.EqualFold(cmd, ac) {
			return ac
		}
	}
	return ""
}

// procTable is a snapshot of the process list, indexed for descendant walks.
type procTable struct {
	args     map[int]string
	children map[int][]int
}

// scanProcesses takes one snapshot of every process and its parent. It is taken
// at most once per poll, and only when some pane is running an interpreter, so
// the common all-"claude" case costs nothing.
func scanProcesses(ctx context.Context) *procTable {
	out, err := exec.CommandContext(ctx, "ps", "-eo", "pid=,ppid=,args=").Output()
	if err != nil {
		return nil
	}
	t := &procTable{args: map[int]string{}, children: map[int][]int{}}
	for _, line := range strings.Split(string(out), "\n") {
		pidStr, rest := cutField(line)
		ppidStr, args := cutField(rest)
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(ppidStr)
		if err != nil {
			continue
		}
		t.args[pid] = args
		if ppid != pid { // pid 0/1 parent themselves on some systems
			t.children[ppid] = append(t.children[ppid], pid)
		}
	}
	return t
}

// cutField splits off the first whitespace-separated field, returning it and
// the remainder with leading whitespace trimmed. The remainder is kept whole so
// that the final field (argv) survives the spaces inside it.
func cutField(s string) (string, string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i:], " \t")
}

// maxProcDepth bounds the descendant walk. An agent sits a step or two below
// the pane's shell (shell → node, or shell → wrapper → node), never deep in a
// build tree, and the bound keeps a malformed table from costing a long walk.
const maxProcDepth = 4

// agentInTree walks the process subtree rooted at pid, breadth first, and
// returns the first configured agent named by a process's argv — the agent the
// pane is really running behind its interpreter.
func (t *procTable) agentInTree(pid int) string {
	if t == nil {
		return ""
	}
	seen := map[int]bool{}
	frontier := []int{pid}
	for depth := 0; depth <= maxProcDepth && len(frontier) > 0; depth++ {
		var next []int
		for _, p := range frontier {
			if seen[p] {
				continue
			}
			seen[p] = true
			if name := agentInArgs(t.args[p]); name != "" {
				return name
			}
			next = append(next, t.children[p]...)
		}
		frontier = next
	}
	return ""
}

// agentInArgs reports which configured agent an argv names, if any.
//
// Only the program is inspected — the first two non-flag arguments, i.e. the
// interpreter and the script it was handed — so a session that merely mentions
// an agent in a later argument ("node build.js gemini.json") is not mistaken
// for one.
func agentInArgs(args string) string {
	checked := 0
	for _, field := range strings.Fields(args) {
		if strings.HasPrefix(field, "-") {
			continue // a flag, not the program
		}
		if checked++; checked > 2 {
			return ""
		}
		if name := agentInPath(field); name != "" {
			return name
		}
	}
	return ""
}

// scriptExtensions are stripped from a program's basename before it is matched,
// so that a bin shim and the script it points at are recognised alike.
var scriptExtensions = []string{".js", ".mjs", ".cjs", ".ts", ".py", ".rb"}

// agentInPath reports which agent a program path names. Exactly two shapes
// count, since a path is otherwise far too easy to match by accident — a
// server started from ~/gemini-experiments is not Gemini CLI:
//
//	.../bin/gemini, .../gemini.js                        the executable itself
//	.../node_modules/@google/gemini-cli/dist/index.js    an installed package
//
// The second shape is what a global npm install of an agent actually looks like
// once the bin shim has exec'd it, and it is why the package directory is only
// trusted below a node_modules of its own.
func agentInPath(path string) string {
	parts := strings.FieldsFunc(path, isPathSep)
	if len(parts) == 0 {
		return ""
	}

	base := parts[len(parts)-1]
	for _, ext := range scriptExtensions {
		if strings.HasSuffix(strings.ToLower(base), ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}
	for _, ac := range cfg.AgentCommands {
		if strings.EqualFold(base, ac) {
			return ac
		}
	}

	inPackage := false
	for _, part := range parts {
		if strings.EqualFold(part, "node_modules") {
			inPackage = true
			continue
		}
		if !inPackage {
			continue
		}
		for _, ac := range cfg.AgentCommands {
			if isPackageNameFor(part, ac) {
				return ac
			}
		}
	}
	return ""
}

func isPathSep(r rune) bool { return r == '/' || r == '\\' }

// isPackageNameFor reports whether an installed package directory belongs to an
// agent: named for it exactly ("gemini"), or as the head of a longer name
// ("gemini-cli", "claude_code"). A directory that merely starts with the same
// letters ("geminids") does not match.
func isPackageNameFor(part, agent string) bool {
	if len(part) < len(agent) || !strings.EqualFold(part[:len(agent)], agent) {
		return false
	}
	if len(part) == len(agent) {
		return true
	}
	switch part[len(agent)] {
	case '-', '_', '.':
		return true
	}
	return false
}
