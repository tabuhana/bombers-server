package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// An archive names its own paths, and a restore turns those names into object
// keys. A name that escapes is the same class of problem as a traversal through
// a published pack, so it gets the same narrow, positive rule.
func TestSafeNameRefusesAnythingThatEscapes(t *testing.T) {
	for _, ok := range []string{"database.sql", "media/users/01J/avatar", "manifest.json", "media/a.b.c"} {
		if !safeName(ok) {
			t.Errorf("safeName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"",
		"/etc/passwd",
		"../outside",
		"media/../../etc/passwd",
		"media/./x",
		`media\windows\path`,
		"media//double",
	} {
		if safeName(bad) {
			t.Errorf("safeName(%q) = true, want false", bad)
		}
	}
}

// walk must refuse the whole archive rather than skipping the bad entry: an
// archive containing a traversal is not one to half-trust.
func TestWalkRefusesAnUnsafeEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evil.tar.gz")
	writeArchive(t, path, map[string]string{"../escape": "x"})

	err := walk(path, func(string, []byte) error { return nil })
	if err == nil {
		t.Fatal("walk accepted an archive with a traversal in it")
	}
}

// Inspect is what a restore shows you before it does anything irreversible, so
// it has to be right about what it's looking at.
func TestInspectRejectsWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()

	notAnArchive := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notAnArchive, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(notAnArchive); err == nil {
		t.Error("Inspect accepted a file that isn't an archive")
	}

	noManifest := filepath.Join(dir, "empty.tar.gz")
	writeArchive(t, noManifest, map[string]string{"database.sql": "-- nothing"})
	if _, err := Inspect(noManifest); err == nil {
		t.Error("Inspect accepted an archive with no manifest")
	}

	// An archive from a NEWER Bombers must be refused rather than read
	// optimistically — this binary has no way to know what changed.
	future := filepath.Join(dir, "future.tar.gz")
	writeArchive(t, future, map[string]string{"manifest.json": `{"format":99}`})
	if _, err := Inspect(future); err == nil {
		t.Error("Inspect accepted an archive from a newer format")
	}
}

func writeArchive(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
