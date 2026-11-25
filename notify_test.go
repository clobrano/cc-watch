package main

import (
	"reflect"
	"testing"
	"time"
)

func TestTrackBell(t *testing.T) {
	const after = 10 * time.Second
	t0 := time.Now()
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

	s := &session{bell: true, state: StateWaiting}
	if trackBell(s, at(0), after) || trackBell(s, at(30), after) {
		t.Fatal("rang for a session never seen running")
	}

	steps := []struct {
		state State
		sec   int
		ring  bool
	}{
		{StateActive, 40, false},
		{StateWaiting, 42, false}, // settle window starts
		{StateActive, 46, false},  // back to work: not done after all
		{StateWaiting, 48, false}, // window restarts
		{StateIdle, 52, false},    // a break in the waiting stretch...
		{StateWaiting, 54, false}, // ...restarts it, but keeps the arming
		{StateWaiting, 63, false},
		{StateWaiting, 64, true},  // 10s of waiting: ring
		{StateWaiting, 70, false}, // once only
		{StateWaiting, 200, false},
	}
	for i, st := range steps {
		s.state = st.state
		if got := trackBell(s, at(st.sec), after); got != st.ring {
			t.Errorf("step %d (%v at %ds): rang = %v, want %v", i, st.state, st.sec, got, st.ring)
		}
	}
}

func TestTrackBellOff(t *testing.T) {
	const after = 10 * time.Second
	t0 := time.Now()
	s := &session{state: StateActive}
	trackBell(s, t0, after)
	s.state = StateWaiting
	trackBell(s, t0.Add(time.Second), after)
	if trackBell(s, t0.Add(20*time.Second), after) {
		t.Fatal("rang with the bell off")
	}
	// The turn settled while the bell was off; turning it on now must not
	// ring for a turn that ended long ago.
	s.bell = true
	if trackBell(s, t0.Add(30*time.Second), after) {
		t.Fatal("rang for a turn that had already settled")
	}
}

func TestGvariantString(t *testing.T) {
	for in, want := range map[string]string{
		"plain":      "'plain'",
		"it's":       `'it\'s'`,
		`back\slash`: `'back\\slash'`,
		"42":         "'42'",
		"true":       "'true'",
	} {
		if got := gvariantString(in); got != want {
			t.Errorf("gvariantString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNotifyCommandSubstitution(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.NotifyCommand = []string{"ntfy", "publish", "--title={title}", "topic", "{body}"}
	got := findNotifier().args("notes is waiting", "fix the parser")
	want := []string{"ntfy", "publish", "--title=notes is waiting", "topic", "fix the parser"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestUnreadLifecycle(t *testing.T) {
	oldS, oldC := sessions, cfg
	defer func() { sessions, cfg = oldS, oldC }()
	cfg.NotifyAfter = 10
	quiet := notifier{name: "test", args: func(_, _ string) []string { return []string{"true"} }}

	s := &session{name: "notes:0.1", bell: true, state: StateActive}
	sessions = map[string]*session{s.name: s}
	t0 := time.Now()
	ringBells(quiet, t0)
	s.state = StateWaiting
	ringBells(quiet, t0.Add(time.Second))
	ringBells(quiet, t0.Add(12*time.Second))
	if !s.unread {
		t.Fatal("not unread after the notification")
	}
	ringBells(quiet, t0.Add(20*time.Second))
	if !s.unread {
		t.Fatal("unread cleared while nobody looked")
	}

	// Looking at the pane reads it.
	s.visible = true
	ringBells(quiet, t0.Add(22*time.Second))
	if s.unread {
		t.Fatal("still unread with the pane on screen")
	}

	// So does the session going back to work, answered from elsewhere.
	s.visible, s.unread = false, true
	s.state = StateActive
	ringBells(quiet, t0.Add(30*time.Second))
	if s.unread {
		t.Fatal("still unread once running again")
	}

	// And going to it from the dashboard.
	s.unread = true
	applyCommand(command{Seen: s.name})
	if s.unread {
		t.Fatal("still unread after a seen command")
	}
}

func TestStatusStripMarksUnread(t *testing.T) {
	oldS, oldC := sessions, cfg
	defer func() { sessions, cfg = oldS, oldC }()
	cfg.Icons = ""
	sessions = map[string]*session{
		"%1": {name: "api:0.0", state: StateActive},
		"%2": {name: "notes:0.0", state: StateWaiting, bell: true, unread: true},
		"%3": {name: "zeta:0.0", state: StateWaiting, bell: true},
	}
	want := "#[fg=yellow]◇ #[fg=yellow,bold]◆#[nobold] #[fg=blue]◇#[default]"
	if got := statusStrip(); got != want {
		t.Errorf("statusStrip =\n%q\nwant\n%q", got, want)
	}
	sessions = map[string]*session{}
	if got := statusStrip(); got != "" {
		t.Errorf("statusStrip with no sessions = %q, want empty", got)
	}
}

func TestAgentCount(t *testing.T) {
	old := sessions
	defer func() { sessions = old }()
	sessions = map[string]*session{}
	if got := agentCount(); got != "" {
		t.Errorf("agentCount with none = %q", got)
	}
	sessions = map[string]*session{"%1": {state: StateWaiting}}
	if got, want := agentCount(), "   1 agent · 0 running"; got != want {
		t.Errorf("agentCount = %q, want %q", got, want)
	}
	sessions["%2"] = &session{state: StateActive}
	sessions["%3"] = &session{state: StateIdle}
	if got, want := agentCount(), "   3 agents · 1 running"; got != want {
		t.Errorf("agentCount = %q, want %q", got, want)
	}
}

func TestFirstClassificationDoesNotArm(t *testing.T) {
	const after = 10 * time.Second
	t0 := time.Now()
	// A fresh agent: its first classification saw it drawing its banner, so
	// it reads as running, but that change is where arming starts from.
	s := &session{bell: true, state: StateActive, lastChange: t0, armFrom: t0}
	trackBell(s, t0, after)
	s.state = StateWaiting
	trackBell(s, t0.Add(5*time.Second), after)
	if trackBell(s, t0.Add(20*time.Second), after) {
		t.Fatal("rang for an agent that only drew its screen")
	}

	// Its first real turn: the pane changes after that point.
	s.state, s.lastChange = StateActive, t0.Add(30*time.Second)
	trackBell(s, t0.Add(30*time.Second), after)
	s.state = StateWaiting
	trackBell(s, t0.Add(36*time.Second), after)
	if !trackBell(s, t0.Add(47*time.Second), after) {
		t.Fatal("did not ring for a real turn")
	}
}
