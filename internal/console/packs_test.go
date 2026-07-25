package console

import (
	"os"
	"path/filepath"
	"testing"
)

// A pack has no source — pack.json is the whole bundle — so readPackAssets must
// pick up ONLY sounds/** and a top-level wallpaper, and skip everything else.
func TestReadPackAssetsPicksSoundsAndWallpaper(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pack.json", `{"id":"p","name":"P"}`)
	write("sounds/dm.mp3", "MP3")
	write("sounds/button-click.ogg", "OGG")
	write("wallpaper.png", "PNG")
	write("readme.md", "notes")           // skipped
	write("theme.psd", "source junk")     // skipped
	write("extra/whatever.png", "nested") // skipped: not sounds/, not top-level wallpaper

	assets, err := readPackAssets(dir)
	if err != nil {
		t.Fatalf("readPackAssets: %v", err)
	}
	got := map[string]string{}
	for _, a := range assets {
		got[a.path] = a.contentType
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 assets, got %d: %v", len(got), got)
	}
	if got["sounds/dm.mp3"] != "audio/mpeg" {
		t.Errorf("dm.mp3 content type: %q", got["sounds/dm.mp3"])
	}
	if got["sounds/button-click.ogg"] != "audio/ogg" {
		t.Errorf("ogg content type: %q", got["sounds/button-click.ogg"])
	}
	if _, ok := got["wallpaper.png"]; !ok {
		t.Error("top-level wallpaper should be picked up")
	}
	if _, ok := got["readme.md"]; ok {
		t.Error("readme should be skipped")
	}
	if _, ok := got["extra/whatever.png"]; ok {
		t.Error("a nested non-sound file should be skipped")
	}
}
