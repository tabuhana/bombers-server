package console

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tabuhana/bombers-server/internal/packs"
)

// Publishing look-and-feel PACKS from the console — a theme, a sound set, a
// wallpaper, or any mix. Same operator-curated model as games.
//
// A pack is a FOLDER:
//
//     midnight/
//       pack.json          id, name, author, description, and the theme's vars
//       sounds/
//         dm.mp3           named after the sound SLOTS (button-click, dm, …)
//         button-click.mp3
//       wallpaper.png      optional
//
// pack.json is the bundle the client reads (theme variables live inside it);
// sounds/ and wallpaper.* are uploaded byte-for-byte to object storage.
//
// This is one of TWO ways into the same operation — the other is the admin-
// gated POST /packs + PUT /packs/{id}/assets/*, for publishing from the client.
// The size caps live in the packs package so the two can't disagree about what
// fits.

func runPacks(ctx context.Context, c *Console, _ []string) error {
	records, err := packs.List(ctx, c.pool)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		fmt.Fprintln(c.out, "  no packs published")
		return nil
	}
	for _, r := range records {
		assets, _ := packs.ListAssets(ctx, c.pool, r.ID)
		var bytes int64
		for _, a := range assets {
			bytes += a.Size
		}
		line := fmt.Sprintf("  %-24s %-28s %s", r.ID, r.Name, r.Version)
		if len(assets) > 0 {
			line += fmt.Sprintf("   %d file(s), %s", len(assets), humanBytes(bytes))
		}
		fmt.Fprintln(c.out, line)
	}
	fmt.Fprintf(c.out, "%d pack(s)\n", len(records))
	return nil
}

func runUnpublishPack(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: unpublish-pack <id>")
		return nil
	}
	id := args[0]
	removed, err := packs.Delete(ctx, c.pool, id)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("no pack %q is published", id)
	}
	if c.media != nil {
		if err := c.media.RemovePrefix(ctx, packs.AssetKey(id, "")); err != nil {
			fmt.Fprintf(c.out, "  (warning: could not remove stored assets: %v)\n", err)
		}
	}
	fmt.Fprintf(c.out, "  unpublished %s\n", id)
	return nil
}

func runPublishPack(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: publish-pack <folder>")
		fmt.Fprintln(c.out, "  the folder holds pack.json, an optional sounds/ folder, and an optional wallpaper")
		return nil
	}
	dir := strings.Trim(strings.Join(args, " "), `"`)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("read %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is a file — publish-pack takes the pack's FOLDER", dir)
	}

	manifestRaw, err := os.ReadFile(filepath.Join(dir, "pack.json"))
	if err != nil {
		return fmt.Errorf("read pack.json: %w", err)
	}
	var manifest struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return fmt.Errorf("pack.json is not valid JSON: %w", err)
	}
	if manifest.ID == "" || manifest.Name == "" {
		return fmt.Errorf("pack.json needs at least an id and a name")
	}

	assets, err := readPackAssets(dir)
	if err != nil {
		return err
	}

	// The bundle IS pack.json — the theme vars live inside it. Store it verbatim.
	if err := packs.Upsert(ctx, c.pool, packs.Record{
		ID: manifest.ID, Name: manifest.Name, Version: manifest.Version, Bundle: manifestRaw,
	}); err != nil {
		return err
	}

	if c.media == nil && len(assets) > 0 {
		return fmt.Errorf("this pack ships assets but no media store is configured")
	}
	if c.media != nil {
		// Republishing replaces the whole asset set, so a removed sound stops
		// being served rather than lingering.
		if err := c.media.RemovePrefix(ctx, packs.AssetKey(manifest.ID, "")); err != nil {
			fmt.Fprintf(c.out, "  (warning: could not clear old assets: %v)\n", err)
		}
	}

	recorded := make([]packs.Asset, 0, len(assets))
	var total int64
	for _, a := range assets {
		if err := c.media.PutObject(ctx, packs.AssetKey(manifest.ID, a.path), a.data, a.contentType); err != nil {
			return fmt.Errorf("upload %s: %w", a.path, err)
		}
		recorded = append(recorded, packs.Asset{Path: a.path, ContentType: a.contentType, Size: int64(len(a.data))})
		total += int64(len(a.data))
	}
	if err := packs.ReplaceAssets(ctx, c.pool, manifest.ID, recorded); err != nil {
		return err
	}

	fmt.Fprintf(c.out, "  published %s (%s) %s\n", manifest.Name, manifest.ID, manifest.Version)
	if len(recorded) > 0 {
		fmt.Fprintf(c.out, "  %d file(s) — %s\n", len(recorded), humanBytes(total))
	}
	return nil
}

// readPackAssets collects sounds/** and a wallpaper.* from a pack folder. Unlike
// a game, a pack has NO source — pack.json is the whole bundle — so everything
// else is an asset (or ignored).
func readPackAssets(dir string) ([]pendingAsset, error) {
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
		if rel == "pack.json" {
			return nil // the bundle, carried separately
		}

		// Only sounds/** and a top-level wallpaper are meaningful. Anything else
		// (a readme, a source .psd) is skipped rather than uploaded.
		isSound := strings.HasPrefix(rel, "sounds/")
		isWallpaper := !strings.Contains(rel, "/") && strings.HasPrefix(strings.ToLower(rel), "wallpaper.")
		if !isSound && !isWallpaper {
			return nil
		}
		if !packs.ValidAssetPath(rel) {
			return fmt.Errorf("asset path %q is not allowed", rel)
		}
		if info.Size() > packs.AssetLimit {
			return fmt.Errorf("asset %s is %s; the limit is %s", rel, humanBytes(info.Size()), humanBytes(packs.AssetLimit))
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		assets = append(assets, pendingAsset{path: rel, contentType: sniffContentType(rel, data), data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return assets, nil
}
