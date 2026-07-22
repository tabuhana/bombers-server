//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the child in its OWN session, so it survives the terminal that
// started it closing (no SIGHUP) and isn't in this shell's process group.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// processAlive reports whether a pid is a live process. Signal 0 performs the
// permission + existence checks without delivering anything.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// signalStop asks the server to shut down gracefully — the same SIGTERM the
// serve loop already handles by draining HTTP and closing the database.
func signalStop(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
