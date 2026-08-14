package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/tabuhana/bombers-server/internal/backup"
	"github.com/tabuhana/bombers-server/internal/config"
	"github.com/tabuhana/bombers-server/internal/embeddedpg"
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/media"
	"github.com/tabuhana/bombers-server/internal/setup"
)

// `bombers backup` and `bombers restore` — the answer to "this machine is
// going away".
//
// One file, and it is deliberately portable rather than a snapshot of this
// installation: a plain SQL dump plus the media as ordinary files, so it can go
// back into whatever the NEW machine is running. Moving from the laptop's
// embedded Postgres to a VPS's system one, or from filesystem media to MinIO, is
// the normal case, not an edge case.

// humanSize renders bytes for a line somebody reads once.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// backupOptions assembles what both directions need from the saved config +
// environment, exactly as the server itself would.
func backupOptions() (backup.Options, error) {
	_ = godotenv.Load()
	dataDir, _ := setup.DataDir()
	if dataDir != "" {
		if fc, err := setup.Load(dataDir); err == nil {
			setup.EnsureSecret(fc)
			fc.Apply()
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return backup.Options{}, fmt.Errorf("reading configuration: %w", err)
	}

	opts := backup.Options{DatabaseURL: cfg.DatabaseURL, DataDir: dataDir, Version: version}
	// The embedded backend's URL isn't in the environment — it's this package's
	// own fixed loopback string. The database must be RUNNING for either
	// direction, which for a self-hosted install means the server is up.
	if opts.DatabaseURL == "" {
		opts.DatabaseURL = embeddedpg.ConnString()
	}

	// A typed nil in the interface would be non-nil and then panic on use, the
	// same trap the console's status hit — so assign only on success.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if cfg.MediaBackend == "fs" {
		if fs, ferr := media.NewFSStore(cfg.MediaDir); ferr == nil {
			opts.Media = fs
		}
	} else if s3, serr := media.NewStorage(ctx, cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3UseSSL); serr == nil {
		opts.Media = s3
	}
	return opts, nil
}

func runBackup(args []string) {
	logx.Init()

	opts, err := backupOptions()
	if err != nil {
		logx.Fatal("backup: %v", err)
	}

	path := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			path = a
			break
		}
	}
	if path == "" {
		// Named for the day, in the directory you ran it from — so `bombers
		// backup` with no arguments is a complete, sensible action rather than
		// a usage message.
		path = fmt.Sprintf("bombers-backup-%s.tar.gz", time.Now().Format("2006-01-02"))
	}
	if _, statErr := os.Stat(path); statErr == nil {
		logx.Fatal("backup: %s already exists — move it or name a different file", path)
	}

	fmt.Printf("  backing up to %s\n", path)
	if opts.Media == nil {
		fmt.Println("  (no media store configured — the archive will hold the database only)")
	}

	man, err := backup.Create(context.Background(), path, opts)
	if err != nil {
		logx.Fatal("backup: %v", err)
	}

	info, _ := os.Stat(path)
	var size int64
	if info != nil {
		size = info.Size()
	}
	fmt.Printf("  done — %s, %d media file(s)\n", humanSize(size), man.MediaFiles)
	fmt.Println("  keep it somewhere that isn't this machine; that's the whole point of it")
}

func runRestore(args []string) {
	logx.Init()

	path := ""
	yes := false
	for _, a := range args {
		if a == "--yes" || a == "-y" {
			yes = true
			continue
		}
		if !strings.HasPrefix(a, "-") && path == "" {
			path = a
		}
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: bombers restore <file> [--yes]")
		os.Exit(2)
	}

	man, err := backup.Inspect(path)
	if err != nil {
		logx.Fatal("restore: %v", err)
	}

	opts, err := backupOptions()
	if err != nil {
		logx.Fatal("restore: %v", err)
	}

	fmt.Printf("  %s\n", filepath.Base(path))
	fmt.Printf("  taken %s by Bombers %s\n", man.CreatedAt.Local().Format("2 Jan 2006, 15:04"), man.Server)
	fmt.Printf("  holds the whole database and %d media file(s), %s\n", man.MediaFiles, humanSize(man.MediaBytes))
	fmt.Println()
	// Said plainly, because it is the thing people get wrong about restore: it
	// is not a merge. Whatever is in this database now is going away.
	fmt.Println("  This REPLACES everything currently on this server — accounts, notes,")
	fmt.Println("  friendships, published nodes, all of it. There is no undo.")

	if !yes {
		if !confirm("  Type RESTORE to continue: ", "RESTORE") {
			fmt.Println("  cancelled")
			return
		}
	}

	fmt.Println("  restoring…")
	if _, err := backup.Restore(context.Background(), path, opts); err != nil {
		logx.Fatal("restore: %v", err)
	}
	fmt.Println("  done — restart the server so it picks up what changed underneath it")
}

// confirm makes you type a word rather than press y. An irreversible command
// should cost a deliberate act, and it REFUSES outright when stdin isn't a
// terminal — the same rule `deluser` follows, so a script can't stumble into it.
func confirm(prompt, want string) bool {
	info, err := os.Stdin.Stat()
	if err != nil || (info.Mode()&os.ModeCharDevice) == 0 {
		fmt.Fprintln(os.Stderr, "  refusing: restore needs a terminal (or pass --yes if you really mean it)")
		return false
	}
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line) == want
}
