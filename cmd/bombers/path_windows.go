//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The Windows half of `install`'s "put it somewhere you can type it from".
//
// The Unix side has an easy answer — /usr/local/bin if it's writable, your own
// ~/.local/bin otherwise, and either way telling you the one line to add to your
// shell config. Neither half of that translates: /usr/local/bin doesn't exist,
// ~/.local/bin isn't on anybody's PATH, and there is no shell config file to
// print a line for. So Windows gets its own destination and install edits the
// PATH itself.

// windowsInstallDir is %LOCALAPPDATA%\Programs\Bombers — where a per-user
// install belongs on Windows, and where you'd look for one.
func windowsInstallDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		base, err = os.UserCacheDir() // %LocalAppData% on Windows
		if err != nil {
			return "", fmt.Errorf("resolving %%LOCALAPPDATA%%: %w", err)
		}
	}
	dir := filepath.Join(base, "Programs", "Bombers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	return dir, nil
}

// addToUserPath appends dir to the CURRENT USER's PATH, in the registry, and
// tells the rest of the system it moved. No elevation: this is HKCU, your own
// settings, and the machine-wide PATH is never touched. It reports whether it
// actually changed anything, so a reinstall can say so rather than claiming
// credit for work it didn't do.
//
// Two things here are load-bearing, because getting either wrong corrupts a
// PATH in a way that's miserable to undo:
//
//   - It reads the USER value, not the process environment. The environment a
//     process sees is the machine PATH and the user PATH already merged, so
//     writing that back would copy every system entry into your personal PATH.
//   - It writes back the same value TYPE it read. A user PATH is normally
//     REG_EXPAND_SZ, and rewriting it as a plain string turns entries like
//     %JAVA_HOME%\bin into dead literal text.
func addToUserPath(dir string) (bool, error) {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, fmt.Errorf("opening your environment settings: %w", err)
	}
	defer func() { _ = key.Close() }()

	current, valueType, err := key.GetStringValue("Path")
	switch {
	case errors.Is(err, registry.ErrNotExist):
		current, valueType = "", registry.EXPAND_SZ
	case err != nil:
		return false, fmt.Errorf("reading your PATH: %w", err)
	}

	// Installing twice must not list it twice.
	if pathContains(current, dir) {
		return false, nil
	}

	next := dir
	if trimmed := strings.TrimRight(current, "; "); trimmed != "" {
		next = trimmed + ";" + dir
	}

	if valueType == registry.SZ {
		err = key.SetStringValue("Path", next)
	} else {
		err = key.SetExpandStringValue("Path", next)
	}
	if err != nil {
		return false, fmt.Errorf("writing your PATH: %w", err)
	}

	broadcastEnvironmentChange()
	return true, nil
}

// pathContains reports whether a PATH value already lists dir.
func pathContains(pathValue, dir string) bool {
	want := strings.ToLower(filepath.Clean(dir))
	for _, entry := range strings.Split(pathValue, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Entries are often written as %LOCALAPPDATA%\… — compare what one
		// MEANS, not how it's spelled, or a reinstall appends a duplicate.
		if expanded, err := registry.ExpandString(entry); err == nil {
			entry = expanded
		}
		if strings.ToLower(filepath.Clean(entry)) == want {
			return true
		}
	}
	return false
}

// broadcastEnvironmentChange tells running programs the environment changed.
// Without it, Explorer keeps handing every terminal it launches the stale block
// it started with, so a "new terminal" still wouldn't find bombers until the
// next sign-in. Best effort — a failure here costs a re-login, not the install.
func broadcastEnvironmentChange() {
	const (
		hwndBroadcast   = 0xFFFF
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
		timeoutMS       = 5000
	)
	target, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")
	_, _, _ = proc.Call(
		uintptr(hwndBroadcast),
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(target)),
		uintptr(smtoAbortIfHung),
		uintptr(timeoutMS),
		0,
	)
}
