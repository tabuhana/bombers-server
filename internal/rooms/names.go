package rooms

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// A room's default name.
//
// Rooms are temporary and disposable, so their names should be too: something
// you can say out loud ("I'm in amber-lantern") without anyone having to invent
// one. The host renames it the moment it matters and never notices this file
// again.
//
// Two words rather than a number because "Room 4" is unmemorable the instant
// there's a Room 3, and because a name you can read back over voice chat is the
// entire job here.

var roomAdjectives = []string{
	"amber", "brisk", "calm", "copper", "dusk", "eager", "fern", "glass",
	"hazel", "idle", "jade", "keen", "lucid", "mellow", "noble", "opal",
	"plum", "quiet", "rust", "slate", "tidy", "umber", "velvet", "warm",
}

var roomNouns = []string{
	"anchor", "beacon", "cabin", "delta", "ember", "forge", "grove", "harbor",
	"inlet", "junction", "kiln", "lantern", "meadow", "nook", "orbit", "porch",
	"quarry", "ridge", "summit", "thicket", "union", "vault", "willow", "yard",
}

// RandomName returns a fresh two-word room name like "amber-lantern".
//
// Collisions are fine and expected — 576 combinations across a friends-scale
// server means two rooms may share a name, and nothing anywhere identifies a
// room by it. The id does that; this is a label.
func RandomName() string {
	return fmt.Sprintf("%s-%s", pick(roomAdjectives), pick(roomNouns))
}

// pick chooses one word. crypto/rand rather than math/rand because the server
// has no seeded generator and doesn't want one for this — a name doesn't need
// unpredictability, but it does need to not repeat in a loop, and this is the
// source that needs no setup.
func pick(words []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		// A name is never worth failing over.
		return words[0]
	}
	return words[n.Int64()]
}
