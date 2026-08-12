// Package svc wraps kardianos/service so the bombers binary can register,
// control, and run itself as an OS background service (Windows Service,
// systemd, launchd) — the "configure once, close the terminal, it's just
// there" model of LOCAL_MODE.md P5. It is a thin leaf over the library: it may
// import logx but no domain package and never main, so the service wiring in
// package main can pull it in without an import cycle.
//
// The binary self-detects how it was launched via Interactive (service.Inter-
// active): false only when the OS service manager started it. The dispatcher in
// main uses that to route an SCM-launched process through s.Run() while a human
// terminal keeps the ordinary interactive CLI.
package svc

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kardianos/service"
)

// Config is the OS-service registration for the Bombers server. No Arguments
// are set: the SCM launches the binary with the same command line and the
// dispatcher hands an SCM-launched process to s.Run() (it is non-Interactive),
// so the service needs no marker flag to know it is a service.
func Config() *service.Config {
	return &service.Config{
		Name:        "BombersServer",
		DisplayName: "Bombers Server",
		Description: "Bombers notebook server — runs the local self-host backend in the background.",
	}
}

// Interactive reports whether the process is running in an interactive session
// (a real terminal) rather than under the OS service manager. It is false only
// when the SCM launched us — exactly when the dispatcher should run through
// s.Run() instead of the interactive CLI path.
func Interactive() bool {
	return service.Interactive()
}

// Control performs a service-management action against the OS service manager:
// install, start, stop, restart, or uninstall (delegated to service.Control),
// plus status (via the service's own Status). It returns friendly, actionable
// errors — notably an "elevated terminal" hint when install/uninstall is
// refused for lack of privilege, which is the common first stumble on Windows.
func Control(s service.Service, action string) error {
	if action == "status" {
		return status(s)
	}
	// Ask whether a service exists BEFORE handing the request to the service
	// manager. systemctl asks for a root password first and mentions the missing
	// unit second, so an operator who runs the server with `bombers start` gets
	// prompted, authenticates, and then watches it fail — for a unit nobody
	// registered. Answering here costs one unprivileged query.
	if action != "install" {
		if _, err := s.Status(); errors.Is(err, service.ErrNotInstalled) {
			return notInstalled(action)
		}
	}
	if err := service.Control(s, action); err != nil {
		return friendly(action, err)
	}
	return nil
}

// notInstalled explains that there is no service, and names the command that
// does what was being asked for. The plain equivalents are spelled out because
// this is precisely the confusion that produces the mistake: `bombers restart`
// and `bombers service restart` read as the same thing and are not.
func notInstalled(action string) error {
	instead := map[string]string{
		"start":   "bombers start",
		"stop":    "bombers stop",
		"restart": "bombers restart",
	}[action]

	if instead == "" {
		return fmt.Errorf("no OS service is installed, so there is nothing to %s", action)
	}
	return fmt.Errorf("no OS service is installed, so there is nothing to %s.\n"+
		"       To %s the server you run with `bombers start`:  %s\n"+
		"       To register a service instead:                  sudo bombers service install",
		action, action, instead)
}

// status prints the installed/running state in plain words. A not-installed
// service is a normal state to report, not an error, so it is not surfaced as
// one.
func status(s service.Service) error {
	st, err := s.Status()
	if err != nil {
		if errors.Is(err, service.ErrNotInstalled) {
			fmt.Println("Bombers service: not installed")
			return nil
		}
		return friendly("status", err)
	}
	switch st {
	case service.StatusRunning:
		fmt.Println("Bombers service: running")
	case service.StatusStopped:
		fmt.Println("Bombers service: stopped")
	default:
		fmt.Println("Bombers service: unknown")
	}
	return nil
}

// friendly rewrites a low-level control failure into something a self-hoster
// can act on. Registering or removing a system service needs elevation, so a
// permission failure gets an explicit admin/root hint; everything else is
// passed through with the action named.
func friendly(action string, err error) error {
	if isPermission(err) {
		return fmt.Errorf("%s failed: this needs an elevated terminal — run it in an Administrator PowerShell (Windows) or with sudo (Linux/macOS): %w", action, err)
	}
	return fmt.Errorf("%s failed: %w", action, err)
}

// isPermission reports whether err looks like an access/elevation denial. It
// checks the portable os.ErrPermission first, then the platform strings the
// service manager tends to return (the Windows SCM answers "Access is denied.";
// systemd/launchd want root), because kardianos often surfaces those as plain
// wrapped strings that no longer satisfy errors.Is.
func isPermission(err error) bool {
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"access is denied",
		"permission denied",
		"must be run as",
		"administrator",
		"superuser",
		"root privileges",
		"not permitted",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
