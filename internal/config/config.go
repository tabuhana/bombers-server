package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/tabuhana/bombers-server/internal/transfers"
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

// SignupRequiresAllowlist is the one place that decides whether a newcomer
// must be on the allowlist, and it decides by asking whether the mode is
// EXACTLY "open". Anything else — "list", but also a typo, a different case, or
// an empty value — keeps the allowlist in force. The test is written this way
// round on purpose: comparing against "list" instead makes every value that is
// not "list" mean "no allowlist", so a typo opens the door. The mode is read on
// every sign-in, so a bad value has to fail closed here, not just be rejected at
// boot.
func SignupRequiresAllowlist(mode string) bool {
	return mode != SignupOpen
}

// The accepted values of the two backend selectors. They are compared exactly
// (after trimming whitespace): "Embedded" or "FS" is refused, not corrected,
// because guessing which backend someone meant is how a server ends up pointed
// at the wrong database.
const (
	DBExternal = "external"
	DBEmbedded = "embedded"

	MediaS3 = "s3"
	MediaFS = "fs"
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

	// STUNServers is the STUN list GET /ice hands to clients (from STUN_SERVERS),
	// so a peer-to-peer file transfer finds its route through a server this
	// deployment chose and not one it was never asked about. It is already parsed
	// and never empty: Load applies the public default when the variable is unset.
	// transfers owns what a valid entry is (and refuses a TURN one), so the rule
	// lives in one place — and Load applies it, so a bad value stops the server
	// before the database or anything else is started.
	STUNServers []string
}

// Load reads and CHECKS the whole environment, and it is the only place that
// does: the server runs it first thing in boot, before Postgres or any pool or
// store is touched, so a bad variable stops the server there with an error that
// names the variable and the value it was given. Nothing downstream should have
// to discover that a variable was wrong.
//
// It reports every problem it finds at once, on one line, so fixing a
// misconfigured install is one pass and not one restart per typo. A problem is
// never reported against the wrong variable: when DB_BACKEND or MEDIA_BACKEND is
// itself invalid, the variables that depend on which backend was chosen
// (DATABASE_URL, the S3 keys) are not demanded, because nobody knows yet whether
// they are needed.
//
// `doctor`, `backup`, `update` and the console all call this, so everything here
// is a hard failure for all of them.
func Load() (*Config, error) {
	var problems []string
	fail := func(err error) { problems = append(problems, err.Error()) }

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
	// backend, mirroring requireS3 below. If the value is not one of the two,
	// dbBackend is "" and neither case applies: the bad value is the only thing
	// reported, not a missing DATABASE_URL it caused.
	dbBackend, err := oneOf("DB_BACKEND", DBExternal, DBExternal, DBEmbedded)
	if err != nil {
		fail(err)
	}
	requireDB := func(key string) string {
		if dbBackend == DBExternal {
			return require(key)
		}
		return os.Getenv(key)
	}

	// MediaBackend defaults to "s3", preserving the managed path (S3 creds
	// required). "fs" stores media as local files and needs no S3 credentials.
	mediaBackend, err := oneOf("MEDIA_BACKEND", MediaS3, MediaS3, MediaFS)
	if err != nil {
		fail(err)
	}
	// requireS3 marks an S3 credential key required only when the S3 backend is
	// actually selected, so a filesystem self-host user is never forced to
	// invent S3 keys just to pass config validation.
	requireS3 := func(key string) string {
		if mediaBackend == MediaS3 {
			return require(key)
		}
		return os.Getenv(key)
	}

	// SIGNUP_MODE is read again on every sign-in (settings.Resolve), so this is
	// not the only guard — SignupRequiresAllowlist fails closed — but it is the
	// one that tells the operator at boot, with the value, instead of leaving a
	// typo to be silently treated as "list".
	signupMode, err := oneOf("SIGNUP_MODE", SignupList, SignupList, SignupOpen)
	if err != nil {
		fail(err)
	}

	listenPort, err := port("PORT", defaultPort)
	if err != nil {
		fail(err)
	}

	cfg := &Config{
		AutoMigrate:       truthy(optional("AUTO_MIGRATE", "")),
		Host:              optional("HOST", ""),
		Port:              listenPort,
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
		SignupMode:          signupMode,
		MediaBackend:        mediaBackend,
		MediaDir:            optional("MEDIA_DIR", ""),
		S3Endpoint:          optional("S3_ENDPOINT", defaultS3Endpoint),
		S3AccessKey:         requireS3("S3_ACCESS_KEY"),
		S3SecretKey:         requireS3("S3_SECRET_KEY"),
		S3Bucket:            optional("S3_BUCKET", defaultS3Bucket),
		S3UseSSL:            os.Getenv("S3_USE_SSL") == "true",
	}

	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("missing required environment variables: %s", strings.Join(missing, ", ")))
	}

	// The endpoint's SHAPE only. Whether anything answers there, and whether the
	// keys are good, can only be learned over the network, and a server that
	// refuses to boot because the object store was briefly unreachable is worse
	// than one that finds out when it first uses it — so that stays with the
	// media store. It is checked only for the s3 backend: a filesystem install
	// has no use for the variable and must not be stopped by a stale one.
	if mediaBackend == MediaS3 {
		if err := checkS3Endpoint(cfg.S3Endpoint); err != nil {
			fail(err)
		}
	}

	// Checked here, with the rest of the configuration, so that a typo (or a TURN
	// server) is refused before Postgres starts or a connection opens. The error
	// names the variable; ParseSTUN's own text names the offending value.
	stun, err := transfers.ParseSTUN(os.Getenv("STUN_SERVERS"))
	if err != nil {
		fail(fmt.Errorf("STUN_SERVERS: %w", err))
	}
	cfg.STUNServers = stun

	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return cfg, nil
}

// oneOf reads an enumerated variable: unset or blank means the fallback, one of
// the accepted values (exactly, case included) is that value, and anything else
// is an error naming the variable, the value as given, and what would have been
// accepted. It never maps a near miss to a neighbour.
func oneOf(key, fallback string, accepted ...string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	for _, a := range accepted {
		if v == a {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s=%q is not a recognised value (accepted: %s)", key, v, strings.Join(accepted, ", "))
}

// port reads a TCP port variable: unset or blank means the fallback, otherwise
// plain decimal digits in 1-65535. Port 0 is refused — it asks the OS for a
// random port, and nothing that is told where the server is (the console, the
// Caddyfile the wizard prints, a platform's health check) could find it.
func port(key, fallback string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	n, ok := parsePort(v)
	if !ok {
		return "", fmt.Errorf("%s=%q is not a port number (a whole number from 1 to 65535)", key, v)
	}
	return strconv.Itoa(n), nil
}

// parsePort accepts only ASCII digits, so "+80", "-1", "8 0" and "0x50" are not
// ports even though strconv would take some of them.
func parsePort(s string) (int, bool) {
	// Five digits cover 65535; the length cap also stops "00000080" being
	// quietly accepted as 80.
	if s == "" || len(s) > 5 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// checkS3Endpoint validates the shape minio-go wants: host or host:port, no
// scheme (S3_USE_SSL chooses http or https), no path, credentials or query.
func checkS3Endpoint(endpoint string) error {
	bad := func(why string) error {
		return fmt.Errorf("S3_ENDPOINT=%q is not a usable endpoint: %s", endpoint, why)
	}
	if strings.TrimSpace(endpoint) != endpoint || strings.ContainsAny(endpoint, " \t\r\n") {
		return bad("it contains whitespace")
	}
	if strings.Contains(endpoint, "://") {
		return bad("give host or host:port without a scheme; S3_USE_SSL chooses http or https")
	}
	u, err := url.Parse("http://" + endpoint)
	if err != nil {
		return bad("it does not parse as host or host:port")
	}
	if u.Hostname() == "" {
		return bad("it has no host")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return bad("give only host or host:port, with no path, credentials or query")
	}
	if p := u.Port(); p != "" {
		if _, ok := parsePort(p); !ok {
			return bad("the port must be a whole number from 1 to 65535")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return bad("it ends in a colon with no port")
	}
	return nil
}

// truthy reads the usual spellings of "yes" for a boolean env var.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
