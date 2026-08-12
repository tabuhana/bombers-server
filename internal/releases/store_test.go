package releases

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The two halves of an object key are the whole of what could escape releases/,
// and both come out of a request body. A traversal through a published release
// would be the worst kind of bug here — the file at the other end is an
// executable somebody's machine is about to run.
func TestValidVersionRejectsAnythingThatCouldNameADirectory(t *testing.T) {
	ok := []string{"0.1.0", "1.0.0-beta.2", "2026.8.12", "0.1.0+win", "v1_2"}
	for _, v := range ok {
		if !ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = false, want true", v)
		}
	}

	bad := []string{
		"",
		"..",
		".",
		"../../etc",
		"0.1.0/../0.2.0",
		`0.1.0\..\0.2.0`,
		"0.1.0 ", // a trailing space is a different key that looks identical
		strings.Repeat("1", 65),
	}
	for _, v := range bad {
		if ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = true, want false", v)
		}
	}
}

func TestValidArtifactIsAFilenameNeverAPath(t *testing.T) {
	if !ValidArtifact("bombers_0.1.1_x64-setup.exe") {
		t.Error("a normal installer filename was refused")
	}
	// Unlike a pack asset there is no directory tree here: the installer sits
	// directly in its version's folder, so a separator is simply wrong rather
	// than something to sanitise.
	for _, name := range []string{"", ".", "..", "nested/setup.exe", `nested\setup.exe`, "/setup.exe", strings.Repeat("a", 129)} {
		if ValidArtifact(name) {
			t.Errorf("ValidArtifact(%q) = true, want false", name)
		}
	}
}

func TestArtifactKeyIsNamespaced(t *testing.T) {
	got := ArtifactKey("0.1.1", "bombers_0.1.1_x64-setup.exe")
	if want := "releases/0.1.1/bombers_0.1.1_x64-setup.exe"; got != want {
		t.Errorf("ArtifactKey = %q, want %q", got, want)
	}
	// One rule for the writer and the reader. If these ever diverge, a publish
	// succeeds and every download 404s.
	if !strings.HasPrefix(got, "releases/") {
		t.Error("an artifact key escaped the releases/ namespace")
	}
}

// The download URL in an update manifest is built from the request that asked
// for it, so a LAN server hands out LAN URLs with nothing configured and a
// proxied one hands out its public name.
func TestPublicOriginFollowsTheRequest(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		headers map[string]string
		tls     bool
		want    string
	}{
		{
			name: "plain LAN bind",
			host: "192.168.1.110:1337",
			want: "http://192.168.1.110:1337",
		},
		{
			name:    "behind a TLS-terminating proxy",
			host:    "bombers.hanascript.com",
			headers: map[string]string{"X-Forwarded-Proto": "https"},
			want:    "https://bombers.hanascript.com",
		},
		{
			name:    "a proxy chain sends a list; the client's is first",
			host:    "bombers.hanascript.com",
			headers: map[string]string{"X-Forwarded-Proto": "https, http"},
			want:    "https://bombers.hanascript.com",
		},
		{
			name:    "forwarded host wins over the internal one",
			host:    "10.0.0.4:1337",
			headers: map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "bombers.hanascript.com"},
			want:    "https://bombers.hanascript.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/releases/latest", nil)
			r.Host = tc.host
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := publicOrigin(r); got != tc.want {
				t.Errorf("publicOrigin = %q, want %q", got, tc.want)
			}
		})
	}
}

// Both clauses of the "which release is offered" query are decisions, not
// details, and neither is visible in any signature — so an edit that drops one
// would pass every other test here. This pins them.
func TestLatestPicksTheLastPublishedCompleteRelease(t *testing.T) {
	if !strings.Contains(latestSQL, "size_bytes > 0") {
		t.Error("Latest no longer skips releases whose installer never arrived")
	}
	if !strings.Contains(latestSQL, "ORDER BY published_at DESC") {
		t.Error("Latest no longer picks the most recently PUBLISHED release — rollback by republishing is broken")
	}
}
