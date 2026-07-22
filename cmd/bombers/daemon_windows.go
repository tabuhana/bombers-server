//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// detach hides the child's console window and detaches it from this one, so it
// keeps running after the launching shell closes.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008, // DETACHED_PROCESS
	}
}

// processAlive reports whether a pid is a live process. Windows has no signal 0,
// so this opens the process and checks its exit status.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}

// signalStop ends the server. Windows has no SIGTERM to deliver to another
// process, so this is a hard stop; the graceful path is a console `stop` or the
// OS service manager.
func signalStop(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// isWindows reports the build target, for the installed binary's filename.
func isWindows() bool { return true }
