package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/setup"
)

// `bombers install` — the first and only time you install.
//
// It puts the binary somewhere on your PATH so every later command is `bombers
// …` instead of `./bombers …`, and records where the source lives so `bombers
// update` can rebuild from it later without being run from that directory.
//
// It never needs sudo: /usr/local/bin is used when it happens to be writable
// (a container running as root), otherwise ~/.local/bin, which is yours.
//
// install does NOT configure anything and does NOT touch the database — that's
// `setup`, which stays separate precisely so you can reconfigure later without
// reinstalling.

// installRecord remembers where things are, so `update` works from anywhere.
type installRecord struct {
	// Source is the checkout to rebuild from.
	Source string `json:"source"`
	// Binary is where the installed executable lives.
	Binary string `json:"binary"`
}

func recordPath() (string, error) {
	dir, err := setup.DataDir()
	if err != nil {
		return "", fmt.Errorf("resolving the data directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating the data directory: %w", err)
	}
	return filepath.Join(dir, "install.json"), nil
}

func saveInstallRecord(rec installRecord) error {
	path, err := recordPath()
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func loadInstallRecord() (installRecord, error) {
	var rec installRecord
	path, err := recordPath()
	if err != nil {
		return rec, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return rec, fmt.Errorf("no install record — run `bombers install` first")
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		return rec, fmt.Errorf("install record is unreadable: %w", err)
	}
	return rec, nil
}

// installDir picks where the binary goes: a system location when we can write
// there without help, otherwise the user's own bin directory. Never sudo.
func installDir() (string, error) {
	// Windows shares neither destination — see path_windows.go.
	if isWindows() {
		return windowsInstallDir()
	}
	if writable("/usr/local/bin") {
		return "/usr/local/bin", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving your home directory: %w", err)
	}
	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	return dir, nil
}

// writable reports whether we can create a file in dir as the current user.
func writable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	probe := filepath.Join(dir, ".bombers-write-test")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true
}

// onPath reports whether dir is in the current PATH.
func onPath(dir string) bool {
	clean := filepath.Clean(dir)
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == clean {
			return true
		}
	}
	return false
}

func runInstall(_ []string) {
	logx.Init()

	self, err := os.Executable()
	if err != nil {
		logx.Fatal("install: locating this binary: %v", err)
	}
	source, err := moduleRoot(self)
	if err != nil {
		logx.Fatal("install: %v", err)
	}

	dir, err := installDir()
	if err != nil {
		logx.Fatal("install: %v", err)
	}
	target := filepath.Join(dir, binaryName())

	// Build fresh from the checkout rather than copying whatever binary is
	// running: install should produce the code that's actually in the tree.
	if err := buildInto(source, target); err != nil {
		logx.Fatal("install: %v", err)
	}

	if err := saveInstallRecord(installRecord{Source: source, Binary: target}); err != nil {
		logx.Fatal("install: %v", err)
	}

	fmt.Println()
	fmt.Println("Bombers installed.")
	fmt.Printf("  binary:  %s\n", target)
	fmt.Printf("  source:  %s\n", source)
	fmt.Println()
	// onPath reads THIS process's PATH, so on Windows it stays false right after
	// we've edited the stored one — which is the truth worth telling you anyway:
	// this terminal can't see it yet.
	newPath := false
	if !onPath(dir) {
		switch {
		case !isWindows():
			fmt.Printf("  %s isn't on your PATH yet. Add this to your shell config:\n\n", dir)
			fmt.Printf("      export PATH=\"%s:$PATH\"\n\n", dir)
		default:
			added, err := addToUserPath(dir)
			switch {
			case err != nil:
				fmt.Printf("  %s isn't on your PATH, and adding it didn't work: %v\n", dir, err)
				fmt.Println("  Add it yourself, in PowerShell:")
				fmt.Printf("\n      [Environment]::SetEnvironmentVariable('Path',[Environment]::GetEnvironmentVariable('Path','User')+';%s','User')\n\n", dir)
			case added:
				newPath = true
				fmt.Printf("  Added to your PATH:  %s\n\n", dir)
			default:
				// Already stored — this terminal just started before it was.
				newPath = true
				fmt.Printf("  Already on your PATH:  %s\n\n", dir)
			}
		}
	}
	if newPath {
		fmt.Println("  Open a NEW terminal first — this one still has the old PATH.")
		fmt.Println()
	}
	fmt.Println("  Next:  bombers setup     configure the database + media, and migrate")
	fmt.Println("         bombers start     run it in the background")
	fmt.Println("         bombers           open the admin console")
	fmt.Println()
}

// binaryName is the installed filename (Windows wants the extension).
func binaryName() string {
	if isWindows() {
		return "bombers.exe"
	}
	return "bombers"
}

// buildInto compiles the checkout at `source` to `target`, atomically: it builds
// beside the target and renames, so a failed build never leaves you without a
// working binary (and on Unix, renaming over a RUNNING binary is fine — the
// running process keeps its own inode).
func buildInto(source, target string) error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("the Go toolchain isn't on PATH, so the server can't be built")
	}
	tmp := target + ".new"
	cmd := exec.Command("go", "build", "-o", tmp, "./cmd/bombers")
	cmd.Dir = source
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	logx.Info("building from %s", source)
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("build failed (nothing was changed): %w", err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("making it executable: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		// A cross-device rename fails; fall back to a copy.
		if cerr := copyFile(tmp, target); cerr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("installing to %s: %w", target, err)
		}
		_ = os.Remove(tmp)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

// moduleRoot finds the server checkout: the nearest ancestor of the binary (or
// of the working directory) holding THIS module's go.mod. Checking the module
// line matters — a binary sitting inside an unrelated Go project must not
// silently build from it.
func moduleRoot(self string) (string, error) {
	starts := []string{filepath.Dir(self)}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	for _, start := range starts {
		dir := start
		for {
			if isModuleRoot(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("no bombers-server checkout found near %s — run install from the folder you cloned", self)
}

func isModuleRoot(dir string) bool {
	body, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	if !strings.Contains(string(body), "module github.com/tabuhana/bombers-server") {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, "cmd", "bombers"))
	return err == nil && info.IsDir()
}
