package transfers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/tabuhana/bombers-server/internal/auth"
)

func getICE(t *testing.T, h *Handler, signedIn bool) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if signedIn {
				req = req.WithContext(auth.WithUserID(req.Context(), "alice"))
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/ice", h.ICE)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ice", nil))
	return rec
}

func TestICEDefaultsToTheStunListTheClientAlreadyUsed(t *testing.T) {
	h, err := NewHandler("")
	if err != nil {
		t.Fatal(err)
	}
	rec := getICE(t, h, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ICEServers []struct {
			URLs []string `json:"urls"`
		} `json:"iceServers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"stun:stun.l.google.com:19302", "stun:stun1.l.google.com:19302"}
	if len(got.ICEServers) != 1 || !reflect.DeepEqual(got.ICEServers[0].URLs, want) {
		t.Errorf("iceServers = %+v, want one entry of %v", got.ICEServers, want)
	}
}

func TestICEServesTheConfiguredListAndOnlyThat(t *testing.T) {
	h, err := NewHandler(" stun:stun.example.org:3478 , STUNS:[2001:db8::1]:5349,stun:10.0.0.5, ")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ICEServers []struct {
			URLs []string `json:"urls"`
		} `json:"iceServers"`
	}
	if err := json.Unmarshal(getICE(t, h, true).Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"stun:stun.example.org:3478", "stuns:[2001:db8::1]:5349", "stun:10.0.0.5"}
	if len(got.ICEServers) != 1 || !reflect.DeepEqual(got.ICEServers[0].URLs, want) {
		t.Errorf("iceServers = %+v, want one entry of %v", got.ICEServers, want)
	}
}

// The wire shape is {"iceServers":[{"urls":[...]}]} and nothing else: no
// username, no credential, which exist only for TURN.
func TestICEResponseCarriesNoCredentialFields(t *testing.T) {
	h, _ := NewHandler("")
	var raw map[string]any
	if err := json.Unmarshal(getICE(t, h, true).Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Errorf("top-level keys = %v, want only iceServers", raw)
	}
	for _, s := range raw["iceServers"].([]any) {
		if keys := s.(map[string]any); len(keys) != 1 {
			t.Errorf("an entry carries %v, want only urls", keys)
		}
	}
}

func TestICEIsBehindAuthentication(t *testing.T) {
	h, _ := NewHandler("")
	if rec := getICE(t, h, false); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// A TURN server is never served, however it is spelled: the whole design is that
// no transfer byte is relayed, and a TURN entry is a relay.
func TestNoTurnServerCanBeConfigured(t *testing.T) {
	for _, v := range []string{
		"turn:turn.example.org:3478",
		"turns:turn.example.org:5349",
		"TURN:turn.example.org",
		"Turns:turn.example.org",
		"stun:stun.example.org,turn:turn.example.org",
		"turn:user@turn.example.org",
		"turn:turn.example.org?transport=udp",
	} {
		if _, err := NewHandler(v); err == nil {
			t.Errorf("ICE_STUN_URLS=%q was accepted", v)
		} else if !strings.Contains(strings.ToLower(err.Error()), "turn") && !strings.Contains(err.Error(), "stun") {
			t.Errorf("ICE_STUN_URLS=%q: unhelpful error %v", v, err)
		}
	}
}

func TestMalformedStunEntriesAreRefusedAtStartup(t *testing.T) {
	for _, v := range []string{
		"stun.example.org:3478",
		"http://stun.example.org",
		"stun:",
		"stun:host:0",
		"stun:host:70000",
		"stun:host:abc",
		"stun:-bad.example.org",
		"stun:bad host.example.org",
		"stun:a:b:c",
		"stun://stun.example.org",
		"stun:host/path",
		"stun:host@evil",
		"javascript:alert(1)",
	} {
		if _, err := ParseSTUN(v); err == nil {
			t.Errorf("ParseSTUN(%q) accepted it", v)
		}
	}
}

func TestParseSTUNBlankMeansDefault(t *testing.T) {
	for _, v := range []string{"", "   ", " , ,", ","} {
		got, err := ParseSTUN(v)
		if err != nil || !reflect.DeepEqual(got, DefaultSTUN) {
			t.Errorf("ParseSTUN(%q) = %v, %v; want the default", v, got, err)
		}
	}
	// The default is a copy: serving it must not let a caller edit it.
	got, _ := ParseSTUN("")
	got[0] = "stun:evil"
	if DefaultSTUN[0] == "stun:evil" {
		t.Error("ParseSTUN returned the default slice itself")
	}
}

func TestTheDefaultListHasNoTurnInIt(t *testing.T) {
	for _, u := range DefaultSTUN {
		if _, err := cleanSTUN(u); err != nil {
			t.Errorf("default %q does not pass its own check: %v", u, err)
		}
	}
}
