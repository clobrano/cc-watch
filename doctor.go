package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// runDoctor prints what cc-watch sees in every pane and why each pane was or
// was not taken for an agent.
//
// It exists because the interesting failure — an agent that never appears — is
// invisible in the dashboard: the pane is simply absent, and nothing says
// whether the config, the pane's command, or the shape of the process tree is
// responsible. For an unmatched pane it prints the processes underneath it, so
// the program that should have been recognised can be read straight off.
func runDoctor(ctx context.Context) error {
	fmt.Println("cc-watch doctor")
	fmt.Println()

	reportConfig()

	out, err := tmux(ctx, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		return fmt.Errorf("tmux list-panes failed (is a tmux server running?): %w", err)
	}
	procs := scanProcesses(ctx)
	if procs == nil {
		fmt.Println("  ! ps failed, so a pane can only be matched by its own command")
		fmt.Println()
	}

	fmt.Printf("%-24s  %-14s  %-8s  %s\n", "PANE", "COMMAND", "PID", "AGENT")
	fmt.Println(strings.Repeat("-", 72))

	var unmatched []paneInfo
	matched := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		p, ok := parsePane(line)
		if !ok {
			continue
		}
		agent, how := "", ""
		if agent = agentForCommand(p.command); agent != "" {
			how = "pane command"
		} else if agent = procs.agentInTree(p.pid); agent != "" {
			how = "process tree"
		}

		shown := "-"
		if agent != "" {
			matched++
			shown = fmt.Sprintf("%s (%s)", agent, how)
		} else {
			unmatched = append(unmatched, p)
		}
		fmt.Printf("%-24s  %-14s  %-8d  %s\n", p.key, p.command, p.pid, shown)
	}

	fmt.Println()
	fmt.Printf("%d pane(s) recognised as agents.\n", matched)

	if len(unmatched) > 0 {
		fmt.Println()
		fmt.Println("Processes under the panes that were not recognised. If an agent is")
		fmt.Println("running in one of these, its program is on this list — send these")
		fmt.Println("lines along when reporting that an agent is not detected:")
		for _, p := range unmatched {
			fmt.Printf("\n  %s (%s):\n", p.key, p.command)
			empty := true
			procs.walk(p.pid, func(pid int) bool {
				if args := procs.args[pid]; args != "" {
					fmt.Printf("    %-8d %s\n", pid, args)
					empty = false
				}
				return true
			})
			if empty {
				fmt.Println("    (no processes found)")
			}
		}
	}
	return nil
}

// reportConfig prints the configuration actually in force. A config file
// replaces the defaults rather than extending them, so a file written before
// Gemini support existed silently keeps Gemini panes out of the dashboard —
// which looks exactly like detection being broken.
func reportConfig() {
	path := configPath()
	switch {
	case path == "":
		fmt.Println("config:  none (no home directory) — built-in defaults")
	default:
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("config:  %s\n", path)
		} else {
			fmt.Printf("config:  %s (absent) — built-in defaults\n", path)
		}
	}
	fmt.Printf("agents:  %s\n", strings.Join(cfg.AgentCommands, ", "))

	if agentForCommand("gemini") == "" {
		fmt.Println()
		fmt.Println("  ! \"gemini\" is not among the agents being watched, so Gemini panes")
		fmt.Println("    are ignored. A config file replaces the defaults rather than")
		fmt.Println("    adding to them: add \"gemini\" to agent_commands in the file above.")
	}
	fmt.Println()
}

type paneInfo struct {
	key     string
	command string
	pid     int
}

// parsePane reads one line of paneFormat output.
func parsePane(line string) (paneInfo, bool) {
	fields := strings.SplitN(line, "|", 5)
	if len(fields) != 5 {
		return paneInfo{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(fields[2]))
	if err != nil {
		return paneInfo{}, false
	}
	return paneInfo{
		key:     fields[4] + ":" + fields[0] + "." + fields[1],
		command: strings.TrimSpace(fields[3]),
		pid:     pid,
	}, true
}
