package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/tabuhana/bombers-server/internal/logx"
)

// serverVersion is the human-facing build version shown in the banner tagline.
const serverVersion = "0.1"

// bombersBanner is the 12-row BOMBERS / NOTEBOOK block-figlet shown on launch.
// It is hand-aligned art — do NOT reflow it or "fix" any box-drawing character.
const bombersBanner = `┌────────┐ ┌────────┐┌────────────┐┌────────┐ ┌────────┐┌─────────┐┌─────────┐
│ ·┌───┐·└┐│ .┌──┐ ·││ ·┌─┐  ┌─┐· ││ ·┌───┐·└┐│· ┌─────┘│  ┌───┐· ││· ┌──────┘
│. │┌──┘ ┌┘│· │  │  ││  │ │· │ │ ·││. │┌──┘ ┌┘│  └──┐   │· └───┘  ││ ·└──────┐
│──│└──┐──││──│  │──││──│ │──│ │──││──│└──┐──││──┌──┘   │──┌─┐──┌─┘└──────┐──│
│══└───┘═┌┘│══└──┘══││══│ │══│ │══││══└───┘═┌┘│══└─────┐│══│ └┐═└─┐┌──────┘══│
└────────┘ └────────┘└──┘ └──┘ └──┘└────────┘ └────────┘└──┘  └───┘└─────────┘
┌───┐ ┌──┐┌────────┐┌────────┐┌────────┐┌────────┐ ┌────────┐┌────────┐┌──┐  ┌──┐
│ . └┐│ ·││ .┌──┐ ·│└──┐· ┌──┘│· ┌─────┘│ ·┌───┐·└┐│ .┌──┐ ·││ .┌──┐ ·││ .│  │ ·│
│· │┐└┘  ││· │  │  │   │  │   │  └──┐   │. │┌──┘ ┌┘│· │  │  ││· │  │  ││· └──┘ ┌┘
│──│└─┐──││──│  │──│   │──│   │──┌──┘   │──│└──┐──││──│  │──││──│  │──││──┌──┐──│
│══│  │══││══└──┘══│   │══│   │══└─────┐│══└───┘═┌┘│══└──┘══││══└──┘══││══│  │══│
└──┘  └──┘└────────┘   └──┘   └────────┘└────────┘ └────────┘└────────┘└──┘  └──┘`

// bannerRamp is the warm top→bottom color for each of the 12 banner rows. The
// floor is kept fairly bright so the bottom rows stay readable on a dark or
// translucent terminal.
var bannerRamp = [12][3]uint8{
	{244, 199, 126}, // #f4c77e
	{238, 183, 107}, // #eeb76b
	{232, 167, 91},  // #e8a75b
	{226, 151, 78},  // #e2974e
	{219, 133, 68},  // #db8544
	{212, 113, 62},  // #d4713e
	{204, 95, 59},   // #cc5f3b
	{194, 85, 63},   // #c2553f
	{182, 79, 60},   // #b64f3c
	{170, 73, 56},   // #aa4938
	{158, 68, 52},   // #9e4434
	{147, 64, 49},   // #934031
}

// printBanner writes the BOMBERS/NOTEBOOK banner and a dim tagline to w: colored
// in the warm ramp when color is enabled, plain otherwise. Callers gate this on
// logx.Interactive() so piped logs never carry the art.
func printBanner(w io.Writer) {
	for i, row := range strings.Split(bombersBanner, "\n") {
		if logx.ColorEnabled() && i < len(bannerRamp) {
			c := bannerRamp[i]
			row = logx.Paint(row, c[0], c[1], c[2])
		}
		fmt.Fprintln(w, row)
	}

	tagline := fmt.Sprintf("notebook server · v%s", serverVersion)
	if logx.ColorEnabled() {
		// Faint so the tagline recedes under the art; \x1b[22m resets intensity.
		tagline = "\x1b[2m" + tagline + "\x1b[22m"
	}
	fmt.Fprintln(w, tagline)
}
