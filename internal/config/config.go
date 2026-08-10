package config

import (
	"fmt"
	"os"
	"strings"
)

const (
	defaultPort              = "8080"
	defaultCorsAllowedOrigin = "http://localhost:1420"

	// Local-dev defaults match the MinIO service in docker-compose.yml. Prod
	// points the same vars at MinIO-on-VPS or Cloudflare R2 — plain S3 API,
	// so only endpoint/credentials change, never code.
	defaultS3Endpoint = "localhost:9000"
	defaultS3Bucket   = "bombers-media"
)

// Who may create an account, via SIGNUP_MODE.
const (
	// SignupList — only Discord ids on the allowlist. The default.
	SignupList = "list"
	// SignupOpen — anyone who signs in with Discord.
	SignupOpen = "open"
)

type Config struct {
	// Host is the bind interface. Empty means all interfaces (":"+Port), which
	// preserves the managed default; local mode may set 127.0.0.1 or 0.0.0.0.
	Host        string
	Port        string
	DatabaseURL string

	// AutoMigrate applies pending migrations at startup. OFF by default: on a
	// managed/external database, applying schema changes should be deliberate.
	// It exists for platforms where there is no shell to run a command in — set
	// AUTO_MIGRATE=true on a container host (Railway, Fly, a plain Docker
	// deploy) and every deploy brings its own schema with it. The embedded
	// backend always migrates itself and ignores this.
	AutoMigrate bool

	// DBBackend selects where Postgres comes from: "external" (default — a
	// DATABASE_URL you provide, the managed path) or "embedded" (the server runs
	// its own local Postgres, self-host). DATABASE_URL is required only for the
	// external backend; the embedded backend supplies its own connection.
	DBBackend string

	TokenSecret       string
	CorsAllowedOrigin string

	// AdminUsername promotes that account to admin at boot. The escape hatch
	// for a hosted deploy (Railway, a container) where attaching to the
	// server's console to run `admin <user>` is awkward. Empty = no-op; an
	// unknown name warns rather than failing the boot, since the usual order on
	// a fresh deploy is "server up, then sign up".
	AdminUsername string

	// Discord OAuth — how people sign in. There is no password anywhere in the
	// product, so an unset client ID means nobody can log in at all.
	//
	// The SECRET lives here rather than in the desktop client on purpose: a
	// client ships to users and cannot keep one. So the server performs the code
	// exchange, and Discord only ever redirects here.
	//
	// DiscordRedirectURL must match a redirect registered on the Discord
	// application EXACTLY — Discord does not pattern-match — which is why it's
	// configuration and not derived from the request.
	DiscordClientID     string
	DiscordClientSecret string
	DiscordRedirectURL  string

	// WebsiteURL is where a login started on the website returns to. Required
	// only for that half — the desktop client's return address is built from a
	// port it supplies, never from configuration.
	WebsiteURL string

	// SignupMode is "list" (default — only Discord ids on the allowlist may
	// create an account) or "open" (anyone who signs in with Discord gets one).
	//
	// It defaults to closed because the cost of the two mistakes isn't
	// symmetrical: a friend who can't sign up sends you a message, and a
	// stranger who can is already in. Opening it later is a one-line change to
	// the environment, which is the right shape for something you do once.
	SignupMode string

	// MediaBackend selects where media bytes live: "s3" (default — external/
	// managed object storage) or "fs" (plain files on local disk, self-host).
	// MediaDir is the filesystem root when the backend is "fs".
	MediaBackend string
	MediaDir     string

	// S3-compatible object storage (profile media). Endpoint is host:port
	// without a scheme; S3UseSSL selects http/https. The access/secret keys are
	// required only for the "s3" backend.
	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	S3Bucket    string
	S3UseSSL    bool
}

func Load() (*Config, error) {
	var missing []string
	require := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}
	optional := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}

	// DBBackend defaults to "external", preserving the managed path (a
	// DATABASE_URL is required). "embedded" runs a local Postgres the server
	// starts itself, which supplies the connection — so no DATABASE_URL is
	// needed. requireDB marks DATABASE_URL required only for the external
	// backend, mirroring requireS3 below.
	dbBackend := optional("DB_BACKEND", "external")
	requireDB := func(key string) string {
		if dbBackend == "external" {
			return require(key)
		}
		return os.Getenv(key)
	}

	// MediaBackend defaults to "s3", preserving the managed path (S3 creds
	// required). "fs" stores media as local files and needs no S3 credentials.
	mediaBackend := optional("MEDIA_BACKEND", "s3")
	// requireS3 marks an S3 credential key required only when the S3 backend is
	// actually selected, so a filesystem self-host user is never forced to
	// invent S3 keys just to pass config validation.
	requireS3 := func(key string) string {
		if mediaBackend == "s3" {
			return require(key)
		}
		return os.Getenv(key)
	}

	cfg := &Config{
		AutoMigrate:       truthy(optional("AUTO_MIGRATE", "")),
		Host:              optional("HOST", ""),
		Port:              optional("PORT", defaultPort),
		DatabaseURL:       requireDB("DATABASE_URL"),
		DBBackend:         dbBackend,
		TokenSecret:       require("TOKEN_SECRET"),
		CorsAllowedOrigin: optional("CORS_ALLOWED_ORIGIN", defaultCorsAllowedOrigin),
		AdminUsername:     os.Getenv("ADMIN_USERNAME"),
		// Not required: a server with these unset still boots, serves, and lets
		// existing sessions work — it just can't sign anybody new in. Failing to
		// start would be worse than saying so at the one endpoint that needs them.
		DiscordClientID:     os.Getenv("DISCORD_CLIENT_ID"),
		DiscordClientSecret: os.Getenv("DISCORD_CLIENT_SECRET"),
		DiscordRedirectURL:  os.Getenv("DISCORD_REDIRECT_URL"),
		WebsiteURL:          os.Getenv("WEBSITE_URL"),
		SignupMode:          optional("SIGNUP_MODE", SignupList),
		MediaBackend:        mediaBackend,
		MediaDir:            optional("MEDIA_DIR", ""),
		S3Endpoint:          optional("S3_ENDPOINT", defaultS3Endpoint),
		S3AccessKey:         requireS3("S3_ACCESS_KEY"),
		S3SecretKey:         requireS3("S3_SECRET_KEY"),
		S3Bucket:            optional("S3_BUCKET", defaultS3Bucket),
		S3UseSSL:            os.Getenv("S3_USE_SSL") == "true",
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// truthy reads the usual spellings of "yes" for a boolean env var.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
