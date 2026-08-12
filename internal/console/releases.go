package console

import (
	"context"
	"fmt"

	"github.com/tabuhana/bombers-server/internal/releases"
)

// The app's own releases, from the console.
//
// There is deliberately no `publish-release` here. Every other store has one,
// because a node or a pack is a folder somebody can put on the server box. An
// installer isn't: it comes out of a build on the operator's Windows desktop and
// this server runs on Linux, so a console publish would mean copying the file
// across first — the very step the admin-gated HTTP path exists to remove. So
// publishing is `bun run release` on the desktop, and what's left here is seeing
// what's out there and taking a bad one down.

func runReleases(ctx context.Context, c *Console, _ []string) error {
	records, err := releases.List(ctx, c.pool)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		fmt.Fprintln(c.out, "  nothing published — clients stay on the version they have")
		return nil
	}
	for i, r := range records {
		// The newest that actually has its file is what every updater is being
		// offered; mark it, because "which one are people getting" is the only
		// question this listing is ever asked.
		marker := "  "
		if r.Size > 0 && !anyLiveBefore(records[:i]) {
			marker = "→ "
		}
		line := fmt.Sprintf("%s%-12s %-40s %s", marker, r.Version, r.Artifact, r.PublishedAt.Format("2006-01-02 15:04"))
		if r.Size > 0 {
			line += "  " + humanBytes(r.Size)
		} else {
			// The metadata landed and the upload didn't. Say so plainly — this
			// version is invisible to clients until the file arrives.
			line += "  (no file yet — run the publish again)"
		}
		fmt.Fprintln(c.out, line)
	}
	fmt.Fprintf(c.out, "%d release(s)\n", len(records))
	return nil
}

// anyLiveBefore reports whether an earlier (newer) row already claimed the
// arrow. List is newest-first, so the first complete row is the live one.
func anyLiveBefore(earlier []releases.Record) bool {
	for _, r := range earlier {
		if r.Size > 0 {
			return true
		}
	}
	return false
}

func runUnpublishRelease(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: unpublish-release <version>")
		fmt.Fprintln(c.out, "  clients then fall back to the newest release still published")
		return nil
	}
	version := args[0]
	removed, err := releases.Delete(ctx, c.pool, version)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("no release %q is published", version)
	}
	if c.media != nil {
		if err := c.media.RemovePrefix(ctx, "releases/"+version+"/"); err != nil {
			fmt.Fprintf(c.out, "  (warning: could not remove the stored installer: %v)\n", err)
		}
	}
	fmt.Fprintf(c.out, "  unpublished %s\n", version)
	return nil
}
