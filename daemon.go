package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// daemonEnv marks the process re-executed by startDaemon. Without it, the
// backgrounded child would see --serve and try to background itself again.
const daemonEnv = "CC_WATCH_DAEMON"

// stopTimeout is how long --stop-server waits for the daemon to release its
// lock after SIGTERM, and how long --serve waits for the child to take it.
const stopTimeout = 5 * time.Second

// runtimeDir holds the pid file and the daemon log. XDG_RUNTIME_DIR is the
// right home for both (per-user, cleared at logout); the temp-dir fallback is
// suffixed with the uid so two users on one machine do not collide.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "cc-watch")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("cc-watch-%d", os.Getuid()))
}

func pidFilePath() string { return filepath.Join(runtimeDir(), "daemon.pid") }
func logFilePath() string { return filepath.Join(runtimeDir(), "daemon.log") }

// errDaemonRunning is returned by lockPidFile when another process already
// holds the lock.
var errDaemonRunning = errors.New("cc-watch daemon already running")

// lockPidFile opens the pid file and takes an exclusive, non-blocking advisory
// lock on it. The lock — not the file's existence — is what marks a live
// daemon: the kernel drops it when the holder exits, so a daemon killed with
// SIGKILL leaves nothing stale behind. The returned file must stay open for as
// long as the daemon runs, since closing it releases the lock.
func lockPidFile() (*os.File, error) {
	if err := os.MkdirAll(runtimeDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(pidFilePath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errDaemonRunning
		}
		return nil, err
	}
	return f, nil
}

// daemonRunning reports whether a daemon holds the pid file lock. Note that a
// process cannot use this to ask about itself: flock is held per open file
// description, so the daemon's own second open would fail to lock and it would
// see itself as "another" daemon.
func daemonRunning() bool {
	f, err := lockPidFile()
	if err != nil {
		return errors.Is(err, errDaemonRunning)
	}
	// We got the lock, so nobody else has it. Release it again immediately.
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
	return false
}

// daemonPID reads the pid recorded in the pid file. The value is only
// meaningful while daemonRunning() is true — the file is deliberately left
// behind on exit so the lock stays the single source of truth.
func daemonPID() (int, error) {
	data, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("malformed pid file %s", pidFilePath())
	}
	return pid, nil
}

// startDaemon re-executes cc-watch with --serve in a new session and returns
// as soon as the child has taken the pid file lock.
func startDaemon() error {
	if daemonRunning() {
		if pid, err := daemonPID(); err == nil {
			return fmt.Errorf("daemon already running (pid %d)", pid)
		}
		return errDaemonRunning
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(runtimeDir(), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logFilePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()

	cmd := exec.Command(exe, "--serve")
	cmd.Env = append(os.Environ(), daemonEnv+"=1")
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setsid detaches the child from the controlling terminal, so it survives
	// the shell that started it (and never steals terminal input).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	cmd.Process.Release() // no wait(): the child is orphaned to init on purpose

	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if daemonRunning() {
			pid, err := daemonPID()
			if err != nil {
				return err
			}
			fmt.Printf("cc-watch daemon started (pid %d)\n", pid)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not start within %s; see %s", stopTimeout, logFilePath())
}

// stopServer signals the running daemon and waits for it to exit.
func stopServer() error {
	if !daemonRunning() {
		return errors.New("no daemon running")
	}
	pid, err := daemonPID()
	if err != nil {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signalling daemon (pid %d): %w", pid, err)
	}

	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if !daemonRunning() {
			fmt.Printf("cc-watch daemon stopped (pid %d)\n", pid)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon (pid %d) did not stop within %s", pid, stopTimeout)
}

// runServe is the daemon body: the same polling loop as the TUI, minus every
// terminal concern, publishing only the tmux status strip.
func runServe() error {
	lock, err := lockPidFile()
	if err != nil {
		return err
	}
	defer lock.Close()
	// The pid file is rewritten rather than created fresh, since a previous
	// daemon leaves it in place with its own (now dead) pid inside.
	if err := lock.Truncate(0); err != nil {
		return err
	}
	if _, err := lock.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		return err
	}

	log.SetFlags(log.LstdFlags)
	log.Printf("cc-watch daemon started (pid %d)", os.Getpid())

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	update(ctx)
	updateStatusBar(ctx)

	for {
		select {
		case <-ctx.Done():
			clearStatusBar()
			log.Printf("cc-watch daemon stopped")
			return nil
		case <-ticker.C:
			update(ctx)
			updateStatusBar(ctx)
		}
	}
}
