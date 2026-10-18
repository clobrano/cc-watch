package main

import (
	"net"
	"path/filepath"
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
