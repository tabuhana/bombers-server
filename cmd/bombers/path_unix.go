//go:build !windows

package main

import "errors"

// The non-Windows side of the pair. Nothing here runs: `install` reaches for
// either of these only inside an isWindows() branch, and the Unix answer to
// both questions already lives in install.go — /usr/local/bin or ~/.local/bin,
// and a printed `export PATH=…` line for your shell config.
//
// They exist so install.go, which is built for every platform, compiles.

func windowsInstallDir() (string, error) {
	return "", errors.New("not Windows")
}

func addToUserPath(string) (bool, error) {
	return false, errors.New("not Windows")
}
