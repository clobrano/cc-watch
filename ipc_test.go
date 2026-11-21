package main

import (
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func recvSnapshot(t *testing.T, ch <-chan snapshot) snapshot {
	t.Helper()
	select {
	case snap, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return snap
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot received")
	}
	return snapshot{}
}

func TestHubServesLatestOnConnectAndFansOut(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	h := newHub()
	h.publish(encodeSnapshot(snapshot{Sessions: []snapshotRow{
		{Name: "notes:0.1", Agent: "claude", State: StateWaiting, Prompt: "fix the parser"},
	}}))
	go h.serve(ln)

	var streams []chan snapshot
	for i := 0; i < 2; i++ {
		conn, err := net.Dial("unix", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ch := make(chan snapshot, 4)
		go readSnapshots(conn, ch)
		streams = append(streams, ch)

		// Every client gets the state already known, without waiting a poll.
		snap := recvSnapshot(t, ch)
		if len(snap.Sessions) != 1 || snap.Sessions[0].State != StateWaiting ||
			snap.Sessions[0].Prompt != "fix the parser" {
			t.Fatalf("client %d: first snapshot = %+v", i, snap)
		}
	}

	h.publish(encodeSnapshot(snapshot{Sessions: []snapshotRow{
		{Name: "notes:0.1", Agent: "claude", State: StateActive},
	}}))
	for i, ch := range streams {
		if snap := recvSnapshot(t, ch); len(snap.Sessions) != 1 || snap.Sessions[0].State != StateActive {
			t.Fatalf("client %d: update = %+v", i, snap)
		}
	}
}

func TestApplySnapshotReplacesSessions(t *testing.T) {
	defer func(old map[string]*session) { sessions = old }(sessions)
	sessions = map[string]*session{"gone:0.0": {name: "gone:0.0"}}

	applySnapshot(snapshot{Sessions: []snapshotRow{
		{Name: "b:0.0", Agent: "codex", State: StateIdle},
		{Name: "a:0.0", Agent: "claude", State: StateError, Desc: "$"},
	}})

	if got := sortedNames(); len(got) != 2 || got[0] != "a:0.0" || got[1] != "b:0.0" {
		t.Fatalf("sortedNames() = %v", got)
	}
	if s := sessions["a:0.0"]; s.agent != "claude" || s.state != StateError || s.desc != "$" {
		t.Fatalf("a:0.0 = %+v", s)
	}
}

func TestHubReceivesCommands(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	h := newHub()
	go h.serve(ln)

	conn, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sendCommand(conn, command{Bell: "notes:0.1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-h.cmds:
		if c.Bell != "notes:0.1" {
			t.Errorf("command = %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no command received")
	}
}

func TestApplyCommandTogglesBell(t *testing.T) {
	old := sessions
	defer func() { sessions = old }()
	sessions = map[string]*session{"notes:0.1": {name: "notes:0.1"}}
	applyCommand(command{Bell: "notes:0.1"})
	if !sessions["notes:0.1"].bell {
		t.Fatal("bell not turned on")
	}
	if snap := takeSnapshot(); !snap.Sessions[0].Bell {
		t.Fatal("snapshot does not carry the bell")
	}
	applyCommand(command{Bell: "notes:0.1"})
	applyCommand(command{Bell: "gone:0.0"}) // unknown sessions are ignored
	if sessions["notes:0.1"].bell {
		t.Fatal("bell not turned off")
	}
}

func TestSessionsKeyedByPaneID(t *testing.T) {
	old := sessions
	defer func() { sessions = old }()

	// Rows are keyed by pane id but listed by name, and a session name is
	// shown in full only where it has more than one agent pane.
	applySnapshot(snapshot{Sessions: []snapshotRow{
		{ID: "%7", Name: "zeta:0.0"},
		{ID: "%2", Name: "api:1.0"},
		{ID: "%9", Name: "api:0.1"},
	}})
	keys := sortedNames()
	if want := []string{"%9", "%2", "%7"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("sortedNames = %q, want %q", keys, want)
	}
	if got, want := sessionNames(keys), []string{"api:0.1", "api:1.0", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sessionNames = %q, want %q", got, want)
	}
	if tmuxTarget("%9") != "%9" {
		t.Error("a pane id is not used as its own target")
	}

	// An older daemon sends no id: its rows are keyed, and targeted, by name.
	applySnapshot(snapshot{Sessions: []snapshotRow{{Name: "notes:0.1"}}})
	if _, ok := sessions["notes:0.1"]; !ok {
		t.Fatal("a row without an id is not keyed by its name")
	}
	if got := tmuxTarget("notes:0.1"); got != "=notes:0.1" {
		t.Errorf("tmuxTarget(name) = %q", got)
	}
}

func TestSessionNameWithColon(t *testing.T) {
	old := sessions
	defer func() { sessions = old }()
	// tmux allows ':' in a session name; only the last one separates the
	// window and pane.
	long := "OSAC-6011:-Design-Expose-ComputeInstance-Provisioning-Failures"
	applySnapshot(snapshot{Sessions: []snapshotRow{
		{ID: "%27", Name: long + ":0.0"},
		{ID: "%30", Name: "OSAC-6011:1.0"},
	}})
	got := sessionNames(sortedNames())
	if want := []string{"OSAC-6011", long}; !reflect.DeepEqual(got, want) {
		t.Errorf("sessionNames = %q, want %q", got, want)
	}
	// Two panes of the colon session: both are told apart by window.pane.
	applySnapshot(snapshot{Sessions: []snapshotRow{
		{ID: "%27", Name: long + ":0.0"},
		{ID: "%39", Name: long + ":1.0"},
	}})
	got = sessionNames(sortedNames())
	if want := []string{long + ":0.0", long + ":1.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sessionNames = %q, want %q", got, want)
	}
}

func TestSortedNamesNumericIndexes(t *testing.T) {
	old := sessions
	defer func() { sessions = old }()
	applySnapshot(snapshot{Sessions: []snapshotRow{
		{ID: "%1", Name: "api:10.0"},
		{ID: "%2", Name: "api:2.1"},
		{ID: "%3", Name: "api:2.0"},
		{ID: "%4", Name: "a:b:0.0"}, // session "a:b" sorts before "api"
	}})
	if got, want := sortedNames(), []string{"%4", "%3", "%2", "%1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sortedNames = %q, want %q", got, want)
	}
}
