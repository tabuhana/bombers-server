package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/setup"
)

// Running detached, the way you'd expect a server to run: `bombers start`
// returns to your prompt and the server keeps going.
//
// This is the binary daemonising ITSELF — no systemd required, nothing to
// install, no root. `start` re-execs itself in the background with its output
// redirected to a log file, records the pid, and prints how to reach it. The
// `service` subcommands remain a separate, heavier option for when you want the
// OS to own the process (start on boot, restart on failure).
//
// The pieces are deliberately boring: a pidfile and a log file in the data
// directory, `stop` to end it, `status` to ask about it, and `console` (which
// already existed) for the admin prompt.

const (
	pidFileName = "server.pid"
	logFileName = "server.log"
)

// runtimePaths returns the pidfile and logfile for this installation.
func runtimePaths() (pidPath, logPath string, err error) {
	dir, err := setup.DataDir()
	if err != nil {
		return "", "", fmt.Errorf("resolving the data directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("creating the data directory: %w", err)
	}
	return filepath.Join(dir, pidFileName), filepath.Join(dir, logFileName), nil
}

// readPid returns the recorded pid, or 0 when there isn't one.
func readPid(pidPath string) int {
	body, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// runningPid returns the pid of a live background server, or 0. A pidfile left
// behind by a crash is cleaned up here rather than confusing every later command.
func runningPid(pidPath string) int {
	pid := readPid(pidPath)
	if pid == 0 {
		return 0
	}
	if !processAlive(pid) {
		_ = os.Remove(pidPath)
		return 0
	}
	return pid
}

// daemonize re-execs this binary detached, pointing its output at the log file,
// and records the pid. `args` is what the child should run (the foreground
// serve). It returns the child's pid.
func daemonize(args []string) (int, error) {
	pidPath, logPath, err := runtimePaths()
	if err != nil {
		return 0, err
	}
	if pid := runningPid(pidPath); pid != 0 {
		return 0, fmt.Errorf("already running (pid %d) — `bombers stop` first", pid)
	}

	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locating this binary: %w", err)
	}

	// Append, so restarting doesn't throw away the last run's logs.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("opening the log file: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(self, args...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Keep the working directory: .env and a source checkout are found relative
	// to it, exactly as they are for a foreground run.
	if wd, werr := os.Getwd(); werr == nil {
		cmd.Dir = wd
	}
	detach(cmd) // platform-specific: new session/process group, so it outlives this shell

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting the background server: %w", err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return pid, fmt.Errorf("recording the pid: %w", err)
	}
	// Don't wait for it — but don't leave a zombie either.
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

// runStop ends the background server.
func runStop(_ []string) {
	logx.Init()

	pidPath, _, err := runtimePaths()
	if err != nil {
		logx.Fatal("stop: %v", err)
	}
	pid := runningPid(pidPath)
	if pid == 0 {
		logx.Info("no background server is running")
		return
	}

	if err := signalStop(pid); err != nil {
		logx.Fatal("stop: could not signal pid %d: %v", pid, err)
	}

	// Give it a moment to shut down gracefully (it drains HTTP and closes the
	// database on SIGTERM), then confirm.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			_ = os.Remove(pidPath)
			logx.Info("server stopped")
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	logx.Warn("server (pid %d) hasn't exited yet; it may still be shutting down", pid)
}

// runStatusCmd answers "is it running, and where are its logs".
func runStatusCmd(_ []string) {
	logx.Init()

	pidPath, logPath, err := runtimePaths()
	if err != nil {
		logx.Fatal("status: %v", err)
	}
	pid := runningPid(pidPath)
	if pid == 0 {
		fmt.Println("bombers: not running")
		fmt.Printf("  log:  %s\n", logPath)
		return
	}
	fmt.Printf("bombers: running (pid %d)\n", pid)
	fmt.Printf("  log:  %s\n", logPath)
	fmt.Println("  admin: bombers console")
}

// runLogs prints the tail of the background server's log.
func runLogs(args []string) {
	logx.Init()

	_, logPath, err := runtimePaths()
	if err != nil {
		logx.Fatal("logs: %v", err)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("no log yet — the server hasn't run in the background")
			return
		}
		logx.Fatal("logs: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	n := 40
	if len(args) > 0 {
		if parsed, perr := strconv.Atoi(args[0]); perr == nil && parsed > 0 {
			n = parsed
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Println(strings.Join(lines, "\n"))
}
