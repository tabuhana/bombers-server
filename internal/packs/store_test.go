package packs

import "testing"

// The traversal guard is the dangerous part, and packs get it from the same
// rule activities use — pin it here too, since a pack is a separate package.
func TestValidAssetPathAndKey(t *testing.T) {
	for _, ok := range []string{"sounds/dm.mp3", "sounds/button-click.ogg", "wallpaper.png"} {
		if !ValidAssetPath(ok) {
			t.Errorf("%q should be allowed", ok)
		}
	}
	for _, bad := range []string{"", "../secret", "sounds/../../etc", `sounds\dm.mp3`, "sounds/./dm.mp3"} {
		if ValidAssetPath(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
	if AssetKey("midnight", "sounds/dm.mp3") != "packs/midnight/sounds/dm.mp3" {
		t.Errorf("unexpected key: %q", AssetKey("midnight", "sounds/dm.mp3"))
	}
}
