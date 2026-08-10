package main

import (
	"slices"
	"testing"
)

// The packaged client's origin has to survive every configuration, including a
// broken one — that's the entire reason this function exists rather than the
// config value being used directly.
func TestCorsOriginsAlwaysAllowsPackagedClient(t *testing.T) {
	for _, configured := range []string{"", "   ", ",,,", "http://localhost:1420"} {
		got := corsOrigins(configured)
		if !slices.Contains(got, PackagedClientOrigin) {
			t.Errorf("corsOrigins(%q) = %v, missing the packaged client origin", configured, got)
		}
	}
}

func TestCorsOrigins(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		want       []string
	}{
		{
			name:       "a single origin, the common case",
			configured: "http://localhost:1420",
			want:       []string{PackagedClientOrigin, "http://localhost:1420"},
		},
		{
			name:       "a list, which is what a website makes necessary",
			configured: "https://hanascript.com,http://localhost:1420",
			want:       []string{PackagedClientOrigin, "https://hanascript.com", "http://localhost:1420"},
		},
		{
			name:       "spaces around the commas",
			configured: "https://hanascript.com , http://localhost:1420",
			want:       []string{PackagedClientOrigin, "https://hanascript.com", "http://localhost:1420"},
		},
		{
			// A browser sends the bare origin, so a pasted trailing slash would
			// silently never match.
			name:       "a trailing slash is trimmed",
			configured: "https://hanascript.com/",
			want:       []string{PackagedClientOrigin, "https://hanascript.com"},
		},
		{
			name:       "duplicates collapse",
			configured: "https://hanascript.com,https://hanascript.com/",
			want:       []string{PackagedClientOrigin, "https://hanascript.com"},
		},
		{
			name:       "configuring the packaged origin doesn't double it",
			configured: PackagedClientOrigin,
			want:       []string{PackagedClientOrigin},
		},
		{
			name:       "empty entries are dropped",
			configured: ",https://hanascript.com,,",
			want:       []string{PackagedClientOrigin, "https://hanascript.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := corsOrigins(tt.configured)
			if !slices.Equal(got, tt.want) {
				t.Errorf("corsOrigins(%q)\n got: %v\nwant: %v", tt.configured, got, tt.want)
			}
		})
	}
}
