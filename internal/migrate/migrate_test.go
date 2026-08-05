package migrate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/tabuhana/bombers-server/migrations"
)

// The directory path `update` relies on has to find the SAME migrations as the
// embed, or an update would silently apply a different set than a fresh install.
//
// This is the check that would have caught the breakage that motivated it: the
// old design exec'd a second binary with a hidden argument, which nothing tested
// and which broke the moment the argument was renamed — because the process
// CHOOSING the name was the old binary, already installed, unchangeable.
// Reading from a directory has no such handshake, and this proves the reading
// part works.

// repoRoot walks up from this file to the checkout root — the directory `update`
// would hand to UpFrom.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	// internal/migrate/migrate_test.go → up three
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

func TestMigrationsDirectoryMatchesTheEmbed(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "migrations")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("update reads migrations from %s, which is not there: %v", dir, err)
	}

	// What the embed carries.
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	embedded, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		t.Fatalf("collect from embed: %v", err)
	}

	// What `update` would read off disk. nil is goose's documented signal to go
	// back to the real filesystem — the setting is global, so this must CLEAR it
	// rather than merely not set it.
	goose.SetBaseFS(nil)
	onDisk, err := goose.CollectMigrations(dir, 0, goose.MaxVersion)
	if err != nil {
		t.Fatalf("collect from %s: %v", dir, err)
	}

	if len(onDisk) != len(embedded) {
		t.Fatalf("directory has %d migrations, embed has %d", len(onDisk), len(embedded))
	}
	if len(onDisk) == 0 {
		t.Fatal("no migrations found either way — the test is proving nothing")
	}
	for i := range onDisk {
		if onDisk[i].Version != embedded[i].Version {
			t.Errorf("migration %d: directory has version %d, embed has %d",
				i, onDisk[i].Version, embedded[i].Version)
		}
	}
}
