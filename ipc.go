package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The daemon owns the polling; dashboards are clients of it. It listens on a
// Unix socket in runtimeDir and streams one JSON snapshot per line: the latest
// one the moment a client connects, so a dashboard has its full state on its
// first frame, and a fresh one after every poll, so any number of dashboards
// can watch the same daemon without the panes being captured once per
// dashboard. In the other direction a dashboard sends the odd command, also
// one JSON object per line: toggling a session's bell, say.

func socketPath() string { return filepath.Join(runtimeDir(), "daemon.sock") }

// writeTimeout bounds a single snapshot write, so a client that stops reading
// (a suspended dashboard, say) is dropped instead of piling up behind.
const writeTimeout = 5 * time.Second

// maxSnapshot caps the line a client will accept. Rows carry prompt text, which
// is the only unbounded field; a megabyte is far more than any screen shows.
const maxSnapshot = 1 << 20

type snapshotRow struct {
	Name   string   `json:"name"`
	Agent  string   `json:"agent"`
	State  State    `json:"state"`
	Prompt string   `json:"prompt,omitempty"`
	Desc   string   `json:"desc,omitempty"`
	Tail   []string `json:"tail,omitempty"`
	Bell   bool     `json:"bell,omitempty"`
	Unread bool     `json:"unread,omitempty"`
}

// command is a request from a dashboard to the daemon, one JSON object per
// line on the same socket the snapshots arrive on.
type command struct {
	// Bell toggles the bell on the named session (a "session:W.P" key).
	Bell string `json:"bell,omitempty"`
	// Seen marks the named session's notification as read.
	Seen string `json:"seen,omitempty"`
}

// sendCommand writes one command to the daemon.
func sendCommand(conn net.Conn, c command) error {
	line, _ := json.Marshal(c) // only strings: cannot fail
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := conn.Write(append(line, '\n'))
	return err
}

// applyCommand carries out a dashboard's command. It runs on the poll
// goroutine, which owns the sessions map.
func applyCommand(c command) {
	if c.Bell != "" {
		if s, ok := sessions[c.Bell]; ok {
			s.bell = !s.bell
			if !s.bell {
				s.unread = false
			}
		}
	}
	if s, ok := sessions[c.Seen]; ok {
		s.unread = false
	}
}

type snapshot struct {
	Sessions []snapshotRow `json:"sessions"`
}

// takeSnapshot copies the poller's view of the panes, in display order.
func takeSnapshot() snapshot {
	snap := snapshot{Sessions: []snapshotRow{}}
	for _, key := range sortedNames() {
		s := sessions[key]
		snap.Sessions = append(snap.Sessions, snapshotRow{
			Name: s.name, Agent: s.agent, State: s.state, Prompt: s.prompt, Desc: s.desc,
			Tail: paneTail(s.colorPane, tailLines),
			Bell: s.bell, Unread: s.unread,
		})
	}
	return snap
}

// applySnapshot replaces the client's sessions with the daemon's, so render and
// sortedNames work on a client exactly as they do next to the poller.
func applySnapshot(snap snapshot) {
	sessions = make(map[string]*session, len(snap.Sessions))
	for _, r := range snap.Sessions {
		sessions[r.Name] = &session{name: r.Name, agent: r.Agent, state: r.State, prompt: r.Prompt, desc: r.Desc, tail: r.Tail, bell: r.Bell, unread: r.Unread}
	}
}

// hub fans each snapshot out to every connected client.
type hub struct {
	mu      sync.Mutex
	latest  []byte
	clients map[chan []byte]struct{}
	// cmds carries the commands dashboards send to the poll goroutine.
	cmds chan command
}

func newHub() *hub {
	return &hub{clients: map[chan []byte]struct{}{}, cmds: make(chan command, 16)}
}

// publish records line as the latest snapshot and hands it to every client.
// Each client has a one-slot mailbox: a client still busy writing the previous
// snapshot has it replaced rather than queued, since only the newest matters.
func (h *hub) publish(line []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.latest = line
	for ch := range h.clients {
		select {
		case <-ch:
		default:
		}
		ch <- line
	}
}

func (h *hub) subscribe() chan []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan []byte, 1)
	if h.latest != nil {
		ch <- h.latest
	}
	h.clients[ch] = struct{}{}
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, ch)
}

// serve accepts clients until ln is closed.
func (h *hub) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("accept: %v", err)
			}
			return
		}
		go h.serveConn(conn)
	}
}

func (h *hub) serveConn(conn net.Conn) {
	defer conn.Close()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	// Reading also notices a client that has gone: without it a client that
	// quits between polls would only be noticed on the next failed write.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var c command
			if json.Unmarshal(sc.Bytes(), &c) == nil {
				h.cmds <- c
			}
		}
	}()

	for {
		select {
		case <-gone:
			return
		case line := <-ch:
			conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := conn.Write(line); err != nil {
				return
			}
		}
	}
}

// listen opens the daemon's socket. Only the pid file lock holder may call it:
// holding the lock is what makes removing a leftover socket safe, since the
// daemon that created it is known to be dead.
func listen() (net.Listener, error) {
	if err := os.Remove(socketPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return net.Listen("unix", socketPath())
}

// encodeSnapshot renders snap as one newline-terminated line of the stream.
func encodeSnapshot(snap snapshot) []byte {
	line, _ := json.Marshal(snap) // only strings and ints: cannot fail
	return append(line, '\n')
}

func dialDaemon() (net.Conn, error) {
	return net.Dial("unix", socketPath())
}

// connectDaemon connects to the running daemon, starting one first if none is
// running.
func connectDaemon() (net.Conn, error) {
	if conn, err := dialDaemon(); err == nil {
		return conn, nil
	}
	if !daemonRunning() {
		if _, err := spawnDaemon(); err != nil && !errors.Is(err, errDaemonRunning) {
			return nil, err
		}
	}
	// Either we just started it, or another one holds the lock and is still
	// coming up — a second dashboard launched at the same moment, say.
	deadline := time.Now().Add(stopTimeout)
	for {
		conn, err := dialDaemon()
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			if pid, perr := daemonPID(); perr == nil && daemonRunning() {
				return nil, fmt.Errorf("daemon (pid %d) is running but not accepting connections on %s; "+
					"restart it with --stop-server and try again", pid, socketPath())
			}
			return nil, fmt.Errorf("cannot connect to daemon on %s: %w", socketPath(), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readSnapshots decodes the daemon's stream into out, and closes out when the
// connection ends.
func readSnapshots(conn net.Conn, out chan<- snapshot) {
	defer close(out)
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), maxSnapshot)
	for sc.Scan() {
		var snap snapshot
		if err := json.Unmarshal(sc.Bytes(), &snap); err != nil {
			continue
		}
		out <- snap
	}
}
