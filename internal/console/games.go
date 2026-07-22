package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/tabuhana/bombers-server/internal/activities"
)

// Publishing games from the console — the same operator-curated model as the
// node store, extended for the one thing games have that nodes don't: assets.
//
// A game is a FOLDER:
//
//     word-sprint/
//       manifest.json      id, name, version, description, category, players
//       index.tsx          source — any text file outside assets/
//       assets/
//         sprites/ball.png
//         sfx/pop.wav
//
// Source files become the opaque {manifest, files} bundle the client compiles.
// Everything under assets/ is uploaded byte-for-byte to object storage and
// recorded, so a client can install the game and its art in one pass.

// maxAssetBytes bounds a single asset file. Generous for sprites and sound;
// small enough that a mistake (publishing a video) is caught here.
const maxAssetBytes = 16 << 20 // 16 MiB

// textExtensions are treated as source. Anything else outside assets/ is
// skipped with a warning rather than silently swept into the bundle.
var textExtensions = map[string]bool{
	".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".json": true, ".css": true, ".md": true, ".txt": true,
}

func runGames(ctx context.Context, c *Console, _ []string) error {
	records, err := activities.List(ctx, c.pool)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		fmt.Fprintln(c.out, "  no games published")
		return nil
	}
	for _, r := range records {
		assets, _ := activities.ListAssets(ctx, c.pool, r.ID)
		var bytes int64
		for _, a := range assets {
			bytes += a.Size
		}
		line := fmt.Sprintf("  %-24s %-28s %s", r.ID, r.Name, r.Version)
		if len(assets) > 0 {
			line += fmt.Sprintf("   %d asset(s), %s", len(assets), humanBytes(bytes))
		}
		fmt.Fprintln(c.out, line)
	}
	fmt.Fprintf(c.out, "%d game(s)\n", len(records))
	return nil
}

func runUnpublishGame(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: unpublish-game <id>")
		return nil
	}
	id := args[0]
	removed, err := activities.Delete(ctx, c.pool, id)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("no game %q is published", id)
	}
	// The asset ROWS cascade with the activity; the bytes are ours to clean up.
	if c.media != nil {
		if err := c.media.RemovePrefix(ctx, activities.AssetKey(id, "")); err != nil {
			fmt.Fprintf(c.out, "  (warning: could not remove stored assets: %v)\n", err)
		}
	}
	fmt.Fprintf(c.out, "  unpublished %s\n", id)
	return nil
}

func runPublishGame(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: publish-game <folder>")
		fmt.Fprintln(c.out, "  the folder holds manifest.json, source files, and an optional assets/ folder")
		return nil
	}
	dir := strings.Trim(strings.Join(args, " "), `"`)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("read %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is a file — publish-game takes the game's FOLDER", dir)
	}

	manifestRaw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("read manifest.json: %w", err)
	}
	var manifest struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return fmt.Errorf("manifest.json is not valid JSON: %w", err)
	}
	if manifest.ID == "" || manifest.Name == "" {
		return fmt.Errorf("manifest.json needs at least an id and a name")
	}

	files, assets, err := readGameFolder(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no source files found in %q", dir)
	}

	var manifestAny any
	_ = json.Unmarshal(manifestRaw, &manifestAny)
	bundle, err := json.Marshal(map[string]any{"manifest": manifestAny, "files": files})
	if err != nil {
		return fmt.Errorf("build bundle: %w", err)
	}

	if err := activities.Upsert(ctx, c.pool, activities.Record{
		ID: manifest.ID, Name: manifest.Name, Version: manifest.Version, Bundle: bundle,
	}); err != nil {
		return err
	}

	// Republishing replaces the whole asset folder, so a file removed from the
	// game stops being served rather than lingering forever.
	if len(assets) > 0 || c.media != nil {
		if c.media == nil {
			return fmt.Errorf("this game ships assets but no media store is configured")
		}
		if err := c.media.RemovePrefix(ctx, activities.AssetKey(manifest.ID, "")); err != nil {
			fmt.Fprintf(c.out, "  (warning: could not clear old assets: %v)\n", err)
		}
	}

	recorded := make([]activities.Asset, 0, len(assets))
	var total int64
	for _, a := range assets {
		if err := c.media.PutObject(ctx, activities.AssetKey(manifest.ID, a.path), a.data, a.contentType); err != nil {
			return fmt.Errorf("upload %s: %w", a.path, err)
		}
		recorded = append(recorded, activities.Asset{
			Path: a.path, ContentType: a.contentType, Size: int64(len(a.data)),
		})
		total += int64(len(a.data))
	}
	if err := activities.ReplaceAssets(ctx, c.pool, manifest.ID, recorded); err != nil {
		return err
	}

	fmt.Fprintf(c.out, "  published %s (%s) %s\n", manifest.Name, manifest.ID, manifest.Version)
	fmt.Fprintf(c.out, "  %d source file(s)", len(files))
	if len(recorded) > 0 {
		fmt.Fprintf(c.out, ", %d asset(s) — %s", len(recorded), humanBytes(total))
	}
	fmt.Fprintln(c.out)
	return nil
}

type pendingAsset struct {
	path        string
	contentType string
	data        []byte
}

// readGameFolder splits a game folder into source text and asset bytes.
func readGameFolder(dir string) (map[string]string, []pendingAsset, error) {
	files := map[string]string{}
	var assets []pendingAsset

	root := filepath.Clean(dir)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "manifest.json" {
			return nil // carried separately, as the bundle's manifest
		}

		if strings.HasPrefix(rel, "assets/") {
			assetPath := strings.TrimPrefix(rel, "assets/")
			if !activities.ValidAssetPath(assetPath) {
				return fmt.Errorf("asset path %q is not allowed", assetPath)
			}
			if info.Size() > maxAssetBytes {
				return fmt.Errorf("asset %s is %s; the limit is %s",
					assetPath, humanBytes(info.Size()), humanBytes(maxAssetBytes))
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			assets = append(assets, pendingAsset{
				path:        assetPath,
				contentType: sniffContentType(assetPath, data),
				data:        data,
			})
			return nil
		}

		if !textExtensions[strings.ToLower(filepath.Ext(rel))] {
			return nil // not source, not an asset — leave it out of the bundle
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return files, assets, nil
}

// sniffContentType picks a type from the bytes, falling back to the extension
// for the formats http.DetectContentType is vague about (audio, fonts).
func sniffContentType(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg":
		return "audio/ogg"
	case ".wav":
		return "audio/wav"
	case ".woff2":
		return "font/woff2"
	case ".json":
		return "application/json"
	}
	if ct := http.DetectContentType(data); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
