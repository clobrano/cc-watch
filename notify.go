package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Desktop notifications for sessions with the bell on.
//
// A session is armed whenever a poll sees it running. Once it has then been
// waiting for notify_after without a break, the bell rings once and the session
// disarms, so a turn ends in exactly one notification. The classifier already
// needs idleThreshold of quiet before it calls a pane waiting; notify_after is
// on top of that, so a pause mid-turn — the agent thinking between two tool
// calls — does not ring the bell for a turn that is about to carry on.
//
// Arming does not depend on the bell, so turning the bell on for a session
// that has been waiting a while does not ring it, while turning it on during
// the settle window of a turn that just ended does.

const defaultNotifyAfter = 10 * time.Second

// notifyTimeout bounds one notifier run, so a stuck notify-send cannot pile up
// goroutines behind it.
const notifyTimeout = 10 * time.Second

func (c Config) notifyAfter() time.Duration {
	if c.NotifyAfter <= 0 {
		return defaultNotifyAfter
	}
	return time.Duration(c.NotifyAfter * float64(time.Second))
}

// trackBell advances s's bell state after a poll has classified it, and
// reports whether the bell should ring now.
func trackBell(s *session, now time.Time, after time.Duration) bool {
	switch s.state {
	case StateActive:
		// Running counts once the pane has changed since its first
		// classification: the change that classification saw may be no more
		// than the agent drawing its screen when it was found.
		if s.armFrom.IsZero() || s.lastChange.After(s.armFrom) {
			s.armed = true
		}
		s.waitingSince = time.Time{}
	case StateWaiting:
		if s.waitingSince.IsZero() {
			s.waitingSince = now
		}
		if s.armed && now.Sub(s.waitingSince) >= after {
			s.armed = false
			return s.bell
		}
	default:
		// Idle, error and unknown break a waiting stretch but keep the arming:
		// a running agent glimpsed as idle for a poll is still mid-turn.
		s.waitingSince = time.Time{}
	}
	return false
}

// notifier delivers one notification.
type notifier struct {
	name string // shown by --doctor
	args func(title, body string) []string
}

// findNotifier picks the first available way to raise a desktop notification:
// notify_command when configured, then the platform's own tool, then a tmux
// message as a last resort that works anywhere cc-watch does.
func findNotifier() notifier {
	if len(cfg.NotifyCommand) > 0 {
		cmd := cfg.NotifyCommand
		return notifier{
			name: "notify_command (" + cmd[0] + ")",
			args: func(title, body string) []string {
				out := make([]string, len(cmd))
				for i, a := range cmd {
					a = strings.ReplaceAll(a, "{title}", title)
					out[i] = strings.ReplaceAll(a, "{body}", body)
				}
				return out
			},
		}
	}
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("osascript"); err == nil {
			return notifier{name: "osascript", args: osascriptArgs}
		}
	}
	// Linux and the BSDs: the freedesktop notification service, which KDE
	// Plasma, GNOME and the standalone daemons (dunst, mako, ...) all provide.
	if _, err := exec.LookPath("notify-send"); err == nil {
		return notifier{name: "notify-send", args: notifySendArgs}
	}
	if _, err := exec.LookPath("gdbus"); err == nil {
		return notifier{name: "gdbus (org.freedesktop.Notifications)", args: gdbusArgs}
	}
	return notifier{name: "tmux display-message (no desktop notifier found)", args: tmuxMessageArgs}
}

func notifySendArgs(title, body string) []string {
	return []string{"notify-send", "--app-name=cc-watch", "--icon=utilities-terminal", "--", title, body}
}

// gdbusArgs calls org.freedesktop.Notifications.Notify directly, for systems
// with no notify-send. gdbus parses every argument as GVariant text, so every
// one is written with its type: the strings quoted (unquoted, a prompt that
// happens to read as a number would be parsed as one) and the rest annotated,
// since gdbus can only infer types from a service that offers introspection.
func gdbusArgs(title, body string) []string {
	return []string{"gdbus", "call", "--session",
		"--dest", "org.freedesktop.Notifications",
		"--object-path", "/org/freedesktop/Notifications",
		"--method", "org.freedesktop.Notifications.Notify",
		// "--" stops the -1 (expire timeout) being read as an option.
		"--",
		gvariantString("cc-watch"), "uint32 0", gvariantString("utilities-terminal"),
		gvariantString(title), gvariantString(body), "@as []", "@a{sv} {}", "int32 -1"}
}

// gvariantString quotes s as a GVariant text string literal.
func gvariantString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(s) + "'"
}

// osascriptArgs passes title and body as script arguments rather than splicing
// them into the script, so no quoting is needed.
func osascriptArgs(title, body string) []string {
	return []string{"osascript",
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		title, body}
}

func tmuxMessageArgs(title, body string) []string {
	msg := title
	if body != "" {
		msg += ": " + body
	}
	// display-message expands formats; '#' must be doubled to come out as is.
	return []string{"tmux", "display-message", "-d", "5000", strings.ReplaceAll(msg, "#", "##")}
}

// run delivers one notification and waits for the notifier to finish.
func (n notifier) run(title, body string) error {
	args := n.args(title, body)
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", notifyTimeout)
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	return fmt.Errorf("notify via %s: %w", n.name, err)
}

// send runs the notifier in the background, logging a failure, so a slow
// notifier never holds up a poll.
func (n notifier) send(title, body string) {
	go func() {
		if err := n.run(title, body); err != nil {
			log.Print(err)
		}
	}()
}

// runNotifyTest sends one notification through the notifier the daemon would
// use, for checking the desktop side without waiting for an agent.
func runNotifyTest() error {
	n := findNotifier()
	fmt.Printf("sending a test notification via %s\n", n.name)
	return n.run("cc-watch test", "Notifications from cc-watch will look like this.")
}

// ringBells advances every session's bell state after a poll and sends the
// notifications that are due.
func ringBells(n notifier, now time.Time) {
	names := sortedNames()
	labels := sessionNames(names)
	for i, key := range names {
		s := sessions[key]
		// The notification counts as read once the user has been to the
		// session: it is on their screen, or it is running again because
		// they answered it.
		if s.visible || s.state == StateActive {
			s.unread = false
		}
		if !trackBell(s, now, cfg.notifyAfter()) {
			continue
		}
		s.unread = true
		body := s.prompt
		if body == "" {
			body = s.desc
		}
		n.send(labels[i]+" is waiting", truncate(body, 200))
	}
}
