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
