package config

import (
	"fmt"
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
	// Blank the rest so nothing in the developer's own environment leaks into a case.
	for _, k := range []string{"PORT", "SIGNUP_MODE", "DATABASE_URL", "S3_ENDPOINT", "S3_ACCESS_KEY", "S3_SECRET_KEY"} {
		t.Setenv(k, "")
	}
}

// refuses runs Load with one variable set to a bad value and checks the error
// is about that variable: it names the variable, carries the value exactly as
// given, and Load hands back no config alongside it.
func refuses(t *testing.T, key, value string) error {
	t.Helper()
	t.Setenv(key, value)
	cfg, err := Load()
	if err == nil {
		t.Fatalf("%s=%q: Load succeeded with %+v", key, value, cfg)
	}
	if cfg != nil {
		t.Errorf("%s=%q: Load returned a config alongside its error", key, value)
	}
	if !strings.Contains(err.Error(), key) {
		t.Errorf("%s=%q: error does not name the variable: %v", key, value, err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", value)) {
		t.Errorf("%s=%q: error does not carry the offending value: %v", key, value, err)
	}
	return err
}

// An enumerated variable with an unrecognised value is refused by name, with the
// accepted values listed — never corrected to a near miss, and never reported as
// some other variable being missing.
func TestLoadRefusesAnUnrecognisedEnumeration(t *testing.T) {
	cases := []struct {
		key      string
		values   []string
		accepted []string
		// notBlamed is a variable the bad value would otherwise cause to be
		// reported as missing; it must NOT appear in the error.
		notBlamed []string
	}{
		{"DB_BACKEND", []string{"embeded", "Embedded", "EXTERNAL", "postgres", "embedded,external", "emb"}, []string{"external", "embedded"}, []string{"DATABASE_URL"}},
		{"MEDIA_BACKEND", []string{"FS", "S3", "disk", "filesystem", "s3,fs", "f"}, []string{"s3", "fs"}, []string{"S3_ACCESS_KEY", "S3_SECRET_KEY"}},
		{"SIGNUP_MODE", []string{"lst", "List", "OPEN", "closed", "true", "everyone"}, []string{"list", "open"}, nil},
	}
	for _, c := range cases {
		for _, v := range c.values {
			t.Run(c.key+"="+v, func(t *testing.T) {
				validEnv(t)
				// DB_BACKEND / MEDIA_BACKEND are what pick whether these are
				// demanded, so make sure they are genuinely absent.
				err := refuses(t, c.key, v)
				for _, a := range c.accepted {
					if !strings.Contains(err.Error(), a) {
						t.Errorf("error does not list accepted value %q: %v", a, err)
					}
				}
				for _, other := range c.notBlamed {
					if strings.Contains(err.Error(), other) {
						t.Errorf("error blames %s, which the bad %s did not cause: %v", other, c.key, err)
					}
				}
			})
		}
	}
}

// The mirror of the case above, with the dependants really unset: the bad
// backend is the ONLY thing reported.
func TestLoadBadBackendDoesNotDemandWhatItDecides(t *testing.T) {
	validEnv(t)
	t.Setenv("MEDIA_BACKEND", "fs")
	err := refuses(t, "DB_BACKEND", "embeded")
	if strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("a typo in DB_BACKEND was reported as a missing variable: %v", err)
	}

	validEnv(t)
	t.Setenv("DB_BACKEND", "embedded")
	err = refuses(t, "MEDIA_BACKEND", "FS")
	if strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "S3_") {
		t.Errorf("a typo in MEDIA_BACKEND was reported as a missing variable: %v", err)
	}
}

func TestLoadAcceptsTheRecognisedBackendsAndModes(t *testing.T) {
	validEnv(t)
	t.Setenv("DB_BACKEND", " external ")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MEDIA_BACKEND", "s3")
	t.Setenv("S3_ACCESS_KEY", "a")
	t.Setenv("S3_SECRET_KEY", "b")
	t.Setenv("SIGNUP_MODE", "open")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBBackend != "external" || cfg.MediaBackend != "s3" || cfg.SignupMode != "open" {
		t.Errorf("got db=%q media=%q signup=%q", cfg.DBBackend, cfg.MediaBackend, cfg.SignupMode)
	}
}

// Unset keeps every default, including the one that still demands DATABASE_URL
// for the (default) external backend.
func TestLoadDefaults(t *testing.T) {
	validEnv(t)
	t.Setenv("DB_BACKEND", "")
	t.Setenv("MEDIA_BACKEND", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected missing DATABASE_URL and S3 keys")
	}
	for _, want := range []string{"missing required environment variables", "DATABASE_URL", "S3_ACCESS_KEY", "S3_SECRET_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}

	validEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "8080" || cfg.SignupMode != "list" {
		t.Errorf("defaults: port=%q signup=%q", cfg.Port, cfg.SignupMode)
	}
}

func TestLoadRefusesABadPort(t *testing.T) {
	for _, v := range []string{"abc", "0", "65536", "99999", "-1", "+80", "80a", "8 0", "1e3", "0x50", "8080.0", "000080", ":8080", "localhost:8080"} {
		t.Run(v, func(t *testing.T) {
			validEnv(t)
			refuses(t, "PORT", v)
		})
	}
}

func TestLoadAcceptsAPort(t *testing.T) {
	for in, want := range map[string]string{"1": "1", "8080": "8080", " 1337 ": "1337", "65535": "65535", "08080": "8080", "": "8080", "  ": "8080"} {
		validEnv(t)
		t.Setenv("PORT", in)
		cfg, err := Load()
		if err != nil {
			t.Errorf("PORT=%q: %v", in, err)
			continue
		}
		if cfg.Port != want {
			t.Errorf("PORT=%q: Port = %q, want %q", in, cfg.Port, want)
		}
	}
}

// Only the SHAPE of S3_ENDPOINT is checked, and only when s3 is the backend.
func TestLoadRefusesABadS3Endpoint(t *testing.T) {
	for _, v := range []string{"https://s3.example.org", "http://localhost:9000", "s3.example.org/bucket", "user:pw@host:9000", "host:abc", "host:0", "host:99999", "host:", ":9000", "has space:9000", "host?x=1", "host#frag"} {
		t.Run(v, func(t *testing.T) {
			validEnv(t)
			t.Setenv("MEDIA_BACKEND", "s3")
			t.Setenv("S3_ACCESS_KEY", "a")
			t.Setenv("S3_SECRET_KEY", "b")
			refuses(t, "S3_ENDPOINT", v)
		})
	}
}

func TestLoadAcceptsS3Endpoints(t *testing.T) {
	for _, v := range []string{"localhost:9000", "s3.example.org", "s3.eu-west-1.amazonaws.com", "10.0.0.5:9000", "[::1]:9000", "minio:9000/"} {
		validEnv(t)
		t.Setenv("MEDIA_BACKEND", "s3")
		t.Setenv("S3_ACCESS_KEY", "a")
		t.Setenv("S3_SECRET_KEY", "b")
		t.Setenv("S3_ENDPOINT", v)
		if _, err := Load(); err != nil {
			t.Errorf("S3_ENDPOINT=%q refused: %v", v, err)
		}
	}
}

// A filesystem install has no use for the S3 variables, so a stale one must not
// stop it from booting.
func TestLoadIgnoresS3EndpointForFilesystemMedia(t *testing.T) {
	validEnv(t)
	t.Setenv("MEDIA_BACKEND", "fs")
	t.Setenv("S3_ENDPOINT", "https://not-used.example.org")
	if _, err := Load(); err != nil {
		t.Errorf("fs media was stopped by S3_ENDPOINT: %v", err)
	}
}

// Every problem is reported in one go, each against its own variable, so fixing
// an install is a single pass.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	validEnv(t)
	t.Setenv("DB_BACKEND", "embeded")
	t.Setenv("PORT", "eighty")
	t.Setenv("SIGNUP_MODE", "closed")
	t.Setenv("TOKEN_SECRET", "")
	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded")
	}
	for _, want := range []string{`DB_BACKEND="embeded"`, `PORT="eighty"`, `SIGNUP_MODE="closed"`, "TOKEN_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error spans several lines, which breaks doctor's one-line checklist: %q", err)
	}
}

// The signup gate fails closed: only an exact "open" lifts the allowlist. The
// handler asks this on every sign-in, so a bad value coming from anywhere (the
// environment, the settings table) must not mean "anyone may join".
func TestSignupRequiresAllowlist(t *testing.T) {
	for mode, want := range map[string]bool{
		"list": true, "open": false,
		"": true, "lst": true, "List": true, "OPEN": true, "Open": true,
		"closed": true, "true": true, "open ": true, "everyone": true,
	} {
		if got := SignupRequiresAllowlist(mode); got != want {
			t.Errorf("SignupRequiresAllowlist(%q) = %v, want %v", mode, got, want)
		}
	}
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
