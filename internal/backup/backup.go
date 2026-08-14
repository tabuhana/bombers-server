// Package backup makes one file you can carry to another machine.
//
// The thing it is NOT is a snapshot of this installation. A backup that only
// restores onto an identical setup isn't a backup, it's a hostage situation:
// the whole point is to survive the machine, and the new machine may well use a
// different database and a different media backend. So the archive holds
// PORTABLE things — a plain SQL dump and the media as ordinary files — and
// restore puts them into whatever this server happens to be configured with.
//
// The database half shells out to pg_dump/psql rather than serialising rows
// here, and that is a deliberate refusal to be clever. A hand-rolled dumper
// that gets one escaping rule wrong produces an archive that looks fine and
// restores wrong, which is the worst failure a backup tool has. pg_dump has
// been getting this right for thirty years. When it isn't installed, say so
// plainly — a loud "install postgresql-client" beats a quiet corruption.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tabuhana/bombers-server/internal/media"
)

// Format is the archive layout's version. Restore refuses anything newer than
// it understands rather than guessing — an archive from a future version is one
// this binary has no way to read correctly.
const Format = 1

// Names inside the archive.
const (
	manifestName = "manifest.json"
	databaseName = "database.sql"
	mediaPrefix  = "media/"
)

// Manifest describes what an archive holds, so a restore can say what it's
// about to do before it does it.
type Manifest struct {
	Format    int       `json:"format"`
	CreatedAt time.Time `json:"created_at"`
	// Server version that wrote it — for a human reading a year-old archive,
	// not for any decision made here.
	Server     string `json:"server"`
	MediaFiles int    `json:"media_files"`
	MediaBytes int64  `json:"media_bytes"`
}

// Options is everything both directions need.
type Options struct {
	// DatabaseURL is what pg_dump/psql connect with.
	DatabaseURL string
	// DataDir is where the embedded Postgres keeps its extracted binaries, so
	// a self-hosted install can back up without any system Postgres installed.
	DataDir string
	// Media is the object store. Nil means this server has none, which is a
	// real configuration — the archive simply carries no media.
	Media media.Store
	// Version of the server, recorded in the manifest.
	Version string
}

// Create writes a backup archive to path.
func Create(ctx context.Context, path string, opts Options) (Manifest, error) {
	man := Manifest{Format: Format, CreatedAt: time.Now().UTC(), Server: opts.Version}

	dump, err := pgTool("pg_dump", opts.DataDir)
	if err != nil {
		return man, err
	}

	out, err := os.Create(path)
	if err != nil {
		return man, fmt.Errorf("create %s: %w", path, err)
	}
	// A half-written archive is worse than none: it looks like a backup. Remove
	// it on any failure, so what's on disk is always either complete or absent.
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()

	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)

	// The database, streamed straight from pg_dump into the archive — a
	// temporary .sql on disk would double the space and leave a copy of
	// everyone's notebook lying around if we died before deleting it.
	//
	// --clean --if-exists means the dump drops what it's replacing, so restoring
	// over an existing database is the same operation as restoring onto an empty
	// one. --no-owner --no-privileges because the roles on the new machine are
	// its own business.
	sql, err := runCapture(ctx, dump,
		"--clean", "--if-exists", "--no-owner", "--no-privileges", opts.DatabaseURL)
	if err != nil {
		return man, fmt.Errorf("pg_dump: %w", err)
	}
	if err := writeFile(tw, databaseName, sql); err != nil {
		return man, err
	}

	if opts.Media != nil {
		keys, lerr := opts.Media.ListObjects(ctx, "")
		if lerr != nil {
			return man, fmt.Errorf("list media: %w", lerr)
		}
		for _, key := range keys {
			body, gerr := opts.Media.GetObject(ctx, key)
			if gerr != nil {
				return man, fmt.Errorf("read media %s: %w", key, gerr)
			}
			data, rerr := io.ReadAll(body)
			_ = body.Close()
			if rerr != nil {
				return man, fmt.Errorf("read media %s: %w", key, rerr)
			}
			if err := writeFile(tw, mediaPrefix+key, data); err != nil {
				return man, err
			}
			man.MediaFiles++
			man.MediaBytes += int64(len(data))
		}
	}

	// The manifest goes in LAST but is read first on the way out — tar is a
	// stream, so restore scans for it rather than assuming a position.
	manJSON, _ := json.MarshalIndent(man, "", "  ")
	if err := writeFile(tw, manifestName, manJSON); err != nil {
		return man, err
	}

	if err := tw.Close(); err != nil {
		return man, fmt.Errorf("finish archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return man, fmt.Errorf("finish archive: %w", err)
	}
	ok = true
	return man, nil
}

// Inspect reads an archive's manifest without restoring anything, so a caller
// can show what it's about to do and ask.
func Inspect(path string) (Manifest, error) {
	var man Manifest
	err := walk(path, func(name string, data []byte) error {
		if name == manifestName {
			return json.Unmarshal(data, &man)
		}
		return nil
	})
	if err != nil {
		return man, err
	}
	if man.Format == 0 {
		return man, fmt.Errorf("%s has no manifest — is it a Bombers backup?", filepath.Base(path))
	}
	if man.Format > Format {
		return man, fmt.Errorf("that archive was written by a newer Bombers (format %d, this one reads %d) — update first", man.Format, Format)
	}
	return man, nil
}

// Restore puts an archive back. The database goes in first: media without its
// rows is unreachable bytes, whereas rows without their media are a card with a
// missing avatar, and of the two half-states that's the survivable one.
func Restore(ctx context.Context, path string, opts Options) (Manifest, error) {
	man, err := Inspect(path)
	if err != nil {
		return man, err
	}

	psql, err := pgTool("psql", opts.DataDir)
	if err != nil {
		return man, err
	}

	var sql []byte
	var mediaFiles [][2]any // key, data — applied after the database lands
	err = walk(path, func(name string, data []byte) error {
		switch {
		case name == databaseName:
			sql = data
		case strings.HasPrefix(name, mediaPrefix):
			mediaFiles = append(mediaFiles, [2]any{strings.TrimPrefix(name, mediaPrefix), data})
		}
		return nil
	})
	if err != nil {
		return man, err
	}
	if len(sql) == 0 {
		return man, fmt.Errorf("that archive has no database in it")
	}

	// ON_ERROR_STOP so a broken restore fails at the first problem instead of
	// running to the end and reporting success over a half-applied schema.
	if err := runInput(ctx, psql, sql, "--quiet", "-v", "ON_ERROR_STOP=1", "-d", opts.DatabaseURL); err != nil {
		return man, fmt.Errorf("psql: %w", err)
	}

	if len(mediaFiles) > 0 {
		if opts.Media == nil {
			return man, fmt.Errorf("the archive carries %d media file(s) but this server has no media store configured", len(mediaFiles))
		}
		for _, f := range mediaFiles {
			key, data := f[0].(string), f[1].([]byte)
			// Content type is re-sniffed rather than carried: the archive is a
			// pile of bytes and the type is metadata the store derives anyway.
			if err := opts.Media.PutObject(ctx, key, data, ""); err != nil {
				return man, fmt.Errorf("write media %s: %w", key, err)
			}
		}
	}
	return man, nil
}

// pgTool finds pg_dump or psql: the embedded Postgres's own copy first (so a
// self-hosted install needs nothing installed system-wide), then the PATH.
func pgTool(name, dataDir string) (string, error) {
	exe := name
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if dataDir != "" {
		// Where internal/embeddedpg extracts its distribution.
		candidate := filepath.Join(dataDir, "pg", "runtime", "bin", exe)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if found, err := exec.LookPath(name); err == nil {
		return found, nil
	}
	return "", fmt.Errorf("%s isn't installed and this server has no embedded Postgres to borrow it from — install the Postgres client tools (on Ubuntu: apt install postgresql-client)", name)
}

func runCapture(ctx context.Context, tool string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %v", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func runInput(ctx context.Context, tool string, input []byte, args ...string) error {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Stdin = strings.NewReader(string(input))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %v", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func writeFile(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("archive %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("archive %s: %w", name, err)
	}
	return nil
}

// walk reads every regular file out of an archive.
func walk(path string, fn func(name string, data []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s is not a Bombers backup: %w", filepath.Base(path), err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, nerr := tr.Next()
		if nerr == io.EOF {
			return nil
		}
		if nerr != nil {
			return fmt.Errorf("read archive: %w", nerr)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// A tar entry names its own path, so an archive could name one that
		// escapes wherever it's unpacked. Nothing here writes to the
		// filesystem, but media keys become object keys — the same class of
		// problem — so the check happens once, here, at the door.
		if !safeName(hdr.Name) {
			return fmt.Errorf("archive contains an unsafe path: %q", hdr.Name)
		}
		data, rerr := io.ReadAll(tr)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", hdr.Name, rerr)
		}
		if err := fn(hdr.Name, data); err != nil {
			return err
		}
	}
}

// safeName is the narrow, positive rule: forward slashes, no leading slash, no
// dot segments, no backslashes.
func safeName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
