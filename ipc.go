package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// first frame, and a fresh one after every poll. The stream is one-way —
// clients never write — so any number of dashboards can watch the same daemon
// without the panes being captured once per dashboard.

func socketPath() string { return filepath.Join(runtimeDir(), "daemon.sock") }

// writeTimeout bounds a single snapshot write, so a client that stops reading
// (a suspended dashboard, say) is dropped instead of piling up behind.
const writeTimeout = 5 * time.Second

// maxSnapshot caps the line a client will accept. Rows carry prompt text, which
// is the only unbounded field; a megabyte is far more than any screen shows.
const maxSnapshot = 1 << 20

type snapshotRow struct {
	Name   string `json:"name"`
	Agent  string `json:"agent"`
	State  State  `json:"state"`
	Prompt string `json:"prompt,omitempty"`
	Desc   string `json:"desc,omitempty"`
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
		})
	}
	return snap
}

// applySnapshot replaces the client's sessions with the daemon's, so render and
// sortedNames work on a client exactly as they do next to the poller.
func applySnapshot(snap snapshot) {
	sessions = make(map[string]*session, len(snap.Sessions))
	for _, r := range snap.Sessions {
		sessions[r.Name] = &session{name: r.Name, agent: r.Agent, state: r.State, prompt: r.Prompt, desc: r.Desc}
	}
}

// hub fans each snapshot out to every connected client.
type hub struct {
	mu      sync.Mutex
	latest  []byte
	clients map[chan []byte]struct{}
}

func newHub() *hub { return &hub{clients: map[chan []byte]struct{}{}} }

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

	// Clients send nothing, so a read returning means the client has gone.
	// Without this a client that quits between polls would only be noticed on
	// the next failed write.
	gone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, conn)
		close(gone)
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
