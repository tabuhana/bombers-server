package console

import (
	"os"
	"path/filepath"
	"testing"
)

// readGameFolder decides what ends up in a published bundle and what gets
// uploaded as bytes. Getting that split wrong is quiet and bad — a stray file
// swept into the bundle, or art silently dropped — so it's worth pinning down.

func TestReadGameFolderSplitsSourceFromAssets(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("manifest.json", `{"id":"g","name":"G"}`)
	write("index.tsx", "export default 1")
	write("lib/board.ts", "export const size = 8")
	write("readme.md", "# notes")
	write("assets/sprites/ball.png", "PNGBYTES")
	write("assets/sfx/pop.wav", "WAVBYTES")
	write("notes.psd", "binary junk that is neither source nor an asset")

	files, assets, err := readGameFolder(dir)
	if err != nil {
		t.Fatalf("readGameFolder: %v", err)
	}

	if _, ok := files["manifest.json"]; ok {
		t.Error("manifest.json must not be duplicated into files — it's the bundle's manifest")
	}
	for _, want := range []string{"index.tsx", "lib/board.ts", "readme.md"} {
		if _, ok := files[want]; !ok {
			t.Errorf("%s should be source, got files: %v", want, keys(files))
		}
	}
	if _, ok := files["notes.psd"]; ok {
		t.Error("an unrecognised binary outside assets/ must be skipped, not bundled")
	}
	if _, ok := files["assets/sprites/ball.png"]; ok {
		t.Error("assets must NOT be bundled as source")
	}

	if len(assets) != 2 {
		t.Fatalf("expected 2 assets, got %d (%+v)", len(assets), assets)
	}
	byPath := map[string]pendingAsset{}
	for _, a := range assets {
		byPath[a.path] = a
	}
	ball, ok := byPath["sprites/ball.png"]
	if !ok {
		t.Fatalf("asset path should be relative to assets/, got %v", keysOf(byPath))
	}
	if string(ball.data) != "PNGBYTES" {
		t.Errorf("asset bytes should pass through verbatim, got %q", ball.data)
	}
	if ball.contentType != "image/png" {
		t.Errorf("content type should come from the extension, got %q", ball.contentType)
	}
	if byPath["sfx/pop.wav"].contentType != "audio/wav" {
		t.Errorf("wav content type, got %q", byPath["sfx/pop.wav"].contentType)
	}
}

// The shipped example must stay publishable — it's what someone copies.
func TestSampleGameFolderReads(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "sample-game")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("sample game folder not present")
	}
	files, assets, err := readGameFolder(dir)
	if err != nil {
		t.Fatalf("the shipped example doesn't read: %v", err)
	}
	if len(files) == 0 {
		t.Error("the example should carry at least one source file")
	}
	if len(assets) == 0 {
		t.Error("the example should carry at least one asset (it demonstrates the folder)")
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOf(m map[string]pendingAsset) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
