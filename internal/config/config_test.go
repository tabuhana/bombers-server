package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tabuhana/bombers-server/internal/transfers"
)

// validEnv sets the least a Load needs to succeed, so a test can change one thing.
func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TOKEN_SECRET", "test-secret")
	t.Setenv("DB_BACKEND", "embedded")
	t.Setenv("MEDIA_BACKEND", "fs")
	t.Setenv("ICE_STUN_URLS", "")
}

// A bad ICE_STUN_URLS is refused by Load itself, not later by whatever builds
// the handler: Load runs before Postgres starts or a connection opens, which is
// the point. The error names the variable and the entry that was wrong.
func TestLoadRefusesABadICEStunURLs(t *testing.T) {
	for _, v := range []string{
		"turn:turn.example.org:3478",
		"turns:turn.example.org",
		"stun:stun.example.org,turn:turn.example.org",
		"stun.example.org:3478",
		"stun:host:99999",
		"http://stun.example.org",
	} {
		validEnv(t)
		t.Setenv("ICE_STUN_URLS", v)
		cfg, err := Load()
		if err == nil {
			t.Errorf("ICE_STUN_URLS=%q: Load succeeded with %+v", v, cfg.ICEStunURLs)
			continue
		}
		if cfg != nil {
			t.Errorf("ICE_STUN_URLS=%q: Load returned a config alongside its error", v)
		}
		if !strings.Contains(err.Error(), "ICE_STUN_URLS") {
			t.Errorf("ICE_STUN_URLS=%q: error does not name the variable: %v", v, err)
		}
		bad := strings.TrimSpace(strings.Split(v, ",")[len(strings.Split(v, ","))-1])
		if !strings.Contains(err.Error(), bad) {
			t.Errorf("ICE_STUN_URLS=%q: error does not name the offending value %q: %v", v, bad, err)
		}
	}
}

func TestLoadParsesICEStunURLs(t *testing.T) {
	validEnv(t)
	t.Setenv("ICE_STUN_URLS", " stun:a.example.org:3478 , STUNS:b.example.org ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"stun:a.example.org:3478", "stuns:b.example.org"}
	if !reflect.DeepEqual(cfg.ICEStunURLs, want) {
		t.Errorf("ICEStunURLs = %v, want %v", cfg.ICEStunURLs, want)
	}
}

// Unset means the default list, so a server that configures nothing changes
// nothing — and the field is never empty for GET /ice to serve.
func TestLoadDefaultsICEStunURLs(t *testing.T) {
	validEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.ICEStunURLs, transfers.DefaultSTUN) {
		t.Errorf("ICEStunURLs = %v, want %v", cfg.ICEStunURLs, transfers.DefaultSTUN)
	}
}
