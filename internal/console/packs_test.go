package console

import (
	"os"
	"path/filepath"
	"testing"
)

// A pack has no source — pack.json is the whole bundle — so readPackAssets must
// pick up ONLY sounds/** and skip everything else. A top-level wallpaper used to
// count; packs carry no background now, so it is skipped like any other stray.
func TestReadPackAssetsPicksSoundsOnly(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pack.json", `{"id":"p","name":"P"}`)
	write("sounds/ff14-chirp.mp3", "MP3")
	write("sounds/soft-tick.ogg", "OGG")
	write("wallpaper.png", "PNG")         // skipped: packs carry no background
	write("readme.md", "notes")           // skipped
	write("theme.psd", "source junk")     // skipped
	write("extra/whatever.png", "nested") // skipped: not under sounds/

	assets, err := readPackAssets(dir)
	if err != nil {
		t.Fatalf("readPackAssets: %v", err)
	}
	got := map[string]string{}
	for _, a := range assets {
		got[a.path] = a.contentType
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 assets, got %d: %v", len(got), got)
	}
	if got["sounds/ff14-chirp.mp3"] != "audio/mpeg" {
		t.Errorf("mp3 content type: %q", got["sounds/ff14-chirp.mp3"])
	}
	if got["sounds/soft-tick.ogg"] != "audio/ogg" {
		t.Errorf("ogg content type: %q", got["sounds/soft-tick.ogg"])
	}
	if _, ok := got["wallpaper.png"]; ok {
		t.Error("a top-level wallpaper should be skipped")
	}
	if _, ok := got["readme.md"]; ok {
		t.Error("readme should be skipped")
	}
	if _, ok := got["extra/whatever.png"]; ok {
		t.Error("a nested non-sound file should be skipped")
	}
}
