package activities

import (
	"strings"
	"testing"
)

// The asset-path rule is the one piece of this domain that must not be wrong:
// a published bundle names its own files, and a path that escapes the activity's
// folder would read (or overwrite) something else entirely.

func TestValidAssetPath(t *testing.T) {
	ok := []string{
		"ball.png",
		"sprites/ball.png",
		"audio/sfx/pop.wav",
		"a/b/c/d/deep.json",
		strings.Repeat("a", 200) + ".png",
	}
	for _, p := range ok {
		if !ValidAssetPath(p) {
			t.Errorf("%q should be allowed", p)
		}
	}

	bad := []string{
		"",                       // nothing
		"/etc/passwd",            // absolute
		"../secrets.txt",         // climbs out
		"sprites/../../etc/x",    // climbs out mid-path
		"sprites/./ball.png",     // dot segment
		"sprites//ball.png",      // empty segment
		`sprites\ball.png`,       // backslash (Windows separator)
		"..",                     // just a climb
		strings.Repeat("a", 300), // absurd
	}
	for _, p := range bad {
		if ValidAssetPath(p) {
			t.Errorf("%q should be REFUSED", p)
		}
	}
}

// The storage key is used by the writer and the reader, so it has to be one
// rule — and it has to stay inside the activity's own folder.
func TestAssetKey(t *testing.T) {
	if got := AssetKey("word-sprint", "sprites/ball.png"); got != "activities/word-sprint/sprites/ball.png" {
		t.Errorf("got %q", got)
	}
	// The prefix form is what unpublish removes: it must cover the whole folder
	// and nothing outside it.
	prefix := AssetKey("word-sprint", "")
	if prefix != "activities/word-sprint/" {
		t.Errorf("prefix should end at the folder boundary, got %q", prefix)
	}
	if !strings.HasPrefix(AssetKey("word-sprint", "a.png"), prefix) {
		t.Error("an asset key should sit under its activity's prefix")
	}
	if strings.HasPrefix(AssetKey("word-sprint-2", "a.png"), prefix) {
		t.Error("a DIFFERENT activity must not fall under this prefix")
	}
}
