// Package setup is local self-host mode's configuration layer: data-directory
// resolution, the JSON config file the first-run wizard writes, and the env
// pre-population that lets config.Load stay a single env-only loader. It runs
// only for `server local` / `server setup`; the managed (no-subcommand) path
// never touches it. Like logx it is a leaf — stdlib only, no domain imports —
// so any caller can pull it in without dragging along the rest of the tree.
//
// The precedence rule the whole design rests on: a real environment variable
// always wins. Apply only fills a var the environment left empty, so the file
// and its generated defaults sit strictly below explicit env (LOCAL_MODE.md §3).
package setup

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const configFile = "config.json"

// Wizard defaults, in one place because two prompt paths offer them and a pair
// that disagreed would be a bug nobody notices until a support thread.
const (
	defaultPort        = "1337"
	defaultDatabaseURL = "postgresql://admin:adminpassword@localhost:5432/bombers"
	defaultS3Endpoint  = "localhost:9000"
	defaultS3AccessKey = "bombers"
	defaultS3SecretKey = "hanascript"
	defaultS3Bucket    = "bombers-media"
	exampleDomain      = "bombers.example.com"
)

// The two places a Bombers server lives, as the wizard sees it.
//
// There used to be a third, "this computer only", binding 127.0.0.1. It went
// because it answered a question nobody asks: if you are running a personal
// notebook server, you want your own laptop to reach it, so the network answer
// was always the right one. The narrow bind didn't disappear — the domain
// answer uses it, since a reverse proxy is what faces the internet there.
const (
	reachNetwork = "network"
	reachDomain  = "domain"
)

// ErrCancelled is returned when the operator abandons the wizard (Ctrl+C).
// Callers should exit without saving: half-answered configuration is worse than
// none, because it looks finished.
var ErrCancelled = errors.New("setup cancelled")

// DataDir resolves (but does NOT create) the directory the local server owns.
// Today that's just the config file; later phases add the embedded Postgres
// data dir, filesystem media, and cached binaries under the same root.
// BOMBERS_DATA_DIR overrides; otherwise it is "Bombers" under the OS
// user-config dir (%AppData% on Windows, ~/.config on Linux, ~/Library/
// Application Support on macOS).
//
// Resolution is side-effect-free on purpose: the managed (pure-env) path calls
// DataDir on every start, and must NOT leave a stray config directory behind
// when nothing is ever saved. The directory is created lazily by Save, the only
// place that actually writes a file.
func DataDir() (string, error) {
	dir := os.Getenv("BOMBERS_DATA_DIR")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolving user config dir: %w", err)
		}
		dir = filepath.Join(base, "Bombers")
	}
	return dir, nil
}

// FileConfig is the on-disk local config (<dataDir>/config.json). It is
// machine-written by the wizard; env is the human override. Each field maps to
// the environment variable Apply pushes it into before config.Load reads it.
type FileConfig struct {
	Host              string `json:"host"`
	Port              string `json:"port"`
	TokenSecret       string `json:"token_secret"`
	CorsAllowedOrigin string `json:"cors_allowed_origin"`
	// Domain is the public name an internet-facing install answers on. It is the
	// one field here that is NOT an environment variable, because nothing in the
	// server reads it: we bind localhost in that mode and Caddy owns the public
	// ports, so the domain only shapes what setup tells you to put in a
	// Caddyfile. Empty for the this-computer and LAN setups.
	Domain string `json:"domain"`
	// DBBackend is "embedded" (the server runs its own local Postgres) or
	// "external" (connect to DatabaseURL). Empty is treated as "external", the
	// managed default. DatabaseURL carries the external connection.
	DBBackend   string `json:"db_backend"`
	DatabaseURL string `json:"database_url"`
	// MediaBackend is "fs" (local files) or "s3" (external object storage);
	// MediaDir is the filesystem root when MediaBackend is "fs". The S3 fields
	// below carry over the external-storage path when MediaBackend is "s3".
	MediaBackend string `json:"media_backend"`
	MediaDir     string `json:"media_dir"`
	S3Endpoint   string `json:"s3_endpoint"`
	S3AccessKey  string `json:"s3_access_key"`
	S3SecretKey  string `json:"s3_secret_key"`
	S3Bucket     string `json:"s3_bucket"`
	S3UseSSL     bool   `json:"s3_use_ssl"`
}

// Load reads <dir>/config.json. A missing file is NOT an error — it returns an
// empty config so a first run flows on to the wizard; any other read or parse
// failure is returned.
func Load(dir string) (*FileConfig, error) {
	data, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		if os.IsNotExist(err) {
			return &FileConfig{}, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var fc FileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &fc, nil
}

// Save writes the config back to <dir>/config.json with 0600 perms — it holds
// TOKEN_SECRET in plaintext, which is acceptable on the user's own machine but
// should stay owner-only (LOCAL_MODE.md §10). It creates <dir> (and parents)
// first: DataDir only resolves the path, so the directory comes into existence
// lazily here, the first time something is actually persisted — a managed
// pure-env run never reaches this and so never makes one.
func (fc *FileConfig) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating data dir %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFile), data, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// Apply pushes each set field into the process environment, but only where the
// environment does not already define that var — so an explicit env value is
// never overwritten. After Apply, config.Load reads a fully-populated
// environment exactly as it does in managed mode.
func (fc *FileConfig) Apply() {
	setIfUnset("HOST", fc.Host)
	setIfUnset("PORT", fc.Port)
	setIfUnset("TOKEN_SECRET", fc.TokenSecret)
	setIfUnset("CORS_ALLOWED_ORIGIN", fc.CorsAllowedOrigin)
	setIfUnset("DB_BACKEND", fc.DBBackend)
	setIfUnset("DATABASE_URL", fc.DatabaseURL)
	setIfUnset("MEDIA_BACKEND", fc.MediaBackend)
	setIfUnset("MEDIA_DIR", fc.MediaDir)
	setIfUnset("S3_ENDPOINT", fc.S3Endpoint)
	setIfUnset("S3_ACCESS_KEY", fc.S3AccessKey)
	setIfUnset("S3_SECRET_KEY", fc.S3SecretKey)
	setIfUnset("S3_BUCKET", fc.S3Bucket)
	// Only a true value carries information; false leaves the var unset so
	// config.Load falls back to its own default (also false).
	if fc.S3UseSSL {
		setIfUnset("S3_USE_SSL", "true")
	}
}

// setIfUnset sets key=value unless value is empty or the env already holds key.
func setIfUnset(key, value string) {
	if value == "" || os.Getenv(key) != "" {
		return
	}
	_ = os.Setenv(key, value)
}

// EnsureSecret mints a TOKEN_SECRET when neither the file nor the environment
// supplies one — 32 crypto/rand bytes, base64 (std). Persisting it (via Save)
// keeps issued tokens valid across restarts. Managed mode still requires an
// explicit secret and never calls this.
func EnsureSecret(fc *FileConfig) {
	if fc.TokenSecret != "" || os.Getenv("TOKEN_SECRET") != "" {
		return
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read does not fail on supported platforms; bail rather
		// than persist a weak or empty secret on the off chance it does.
		return
	}
	fc.TokenSecret = base64.StdEncoding.EncodeToString(buf)
}

// NeedsWizard reports whether the config that config.Load requires can't yet be
// satisfied from env-or-file. DATABASE_URL is needed ONLY when the DB resolves
// to the external backend — the embedded Postgres supplies its own connection,
// so its absence must not drag the user into the wizard. Likewise the S3
// access/secret keys are needed ONLY when media resolves to the S3 backend — a
// filesystem setup needs no S3 keys. When true (and stdin is a terminal) main
// runs the wizard; a set env var alone is enough to skip it.
func NeedsWizard(fc *FileConfig) bool {
	// Effective DB backend: explicit env wins, then the saved file, then the
	// managed default ("external").
	dbBackend := os.Getenv("DB_BACKEND")
	if dbBackend == "" {
		dbBackend = fc.DBBackend
	}
	if dbBackend == "" {
		dbBackend = "external"
	}
	if dbBackend == "external" && !present("DATABASE_URL", fc.DatabaseURL) {
		return true
	}

	// Effective media backend: explicit env wins, then the saved file, then the
	// managed default ("s3").
	backend := os.Getenv("MEDIA_BACKEND")
	if backend == "" {
		backend = fc.MediaBackend
	}
	if backend == "" {
		backend = "s3"
	}
	if backend == "s3" {
		return !present("S3_ACCESS_KEY", fc.S3AccessKey) ||
			!present("S3_SECRET_KEY", fc.S3SecretKey)
	}
	return false
}

// present reports whether a value is available from either the environment or
// the file config.
func present(key, fileValue string) bool {
	return os.Getenv(key) != "" || fileValue != ""
}

// Wizard runs the first-run questions and writes the answers into fc. It's a
// straight walk-through — one decision at a time, each a choice plus its config
// where the choice needs one: where the server lives, port, database (its own /
// yours), then media (local files / S3-MinIO). dataDir is where the filesystem
// media root is placed (<dataDir>/media). Callers run EnsureSecret + Save after.
//
// There are two prompt styles for the same four questions. At a real terminal
// you get selects you arrow through; anywhere else — a pipe, a script, a test —
// you get typed prompts reading lines from stdin. That's not only a fallback for
// politeness: a wizard that can ONLY be driven by a human is a wizard nobody can
// test or automate. Both paths route through the same apply* functions below, so
// the two can't drift apart on what an answer MEANS.
func Wizard(fc *FileConfig, dataDir string) error {
	if interactive(os.Stdin) {
		return wizardTUI(fc, dataDir)
	}
	wizardText(fc, dataDir)
	return nil
}

// interactive reports whether f is a terminal (a character device) rather than a
// pipe or a file.
func interactive(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// wizardText is the typed-prompt path. Each prompt shows the current-or-default
// value in [brackets]; an empty line keeps it.
func wizardText(fc *FileConfig, dataDir string) {
	r := bufio.NewReader(os.Stdin)

	fmt.Println()
	fmt.Println("Bombers local setup — press Enter to accept each [default].")

	askReachability(r, fc)
	askPort(r, fc)
	askDatabase(r, fc)
	askMedia(r, fc, dataDir)

	fmt.Println()
}

// applyReachability records where the server lives: the bind host, and the
// public domain when there is one.
//
// The domain answer binds 127.0.0.1, which reads backwards until you see why. On
// a rented VPS the machine is already on the public internet; Caddy terminates
// TLS on 443 and forwards here on localhost. Binding wider wouldn't make the
// server more reachable — it would publish the UNENCRYPTED port beside the
// encrypted one. See the tui/text domain prompts for why we don't do TLS here.
func applyReachability(fc *FileConfig, choice, domain string) {
	if choice == reachDomain {
		fc.Host = "127.0.0.1"
		fc.Domain = cleanDomain(domain)
		return
	}
	// Clear the domain on the way past, or a config that used to be public keeps
	// printing Caddy instructions for a server that isn't.
	fc.Host = "0.0.0.0"
	fc.Domain = ""
}

// reachOf reports which answer a saved config represents, so a re-run opens on
// the choice already in force.
func reachOf(fc *FileConfig) string {
	if fc.Domain != "" {
		return reachDomain
	}
	return reachNetwork
}

// domainWarning describes what's wrong with a domain, or "" if it looks usable.
// A warning, never a refusal — the config is portable, and someone may well be
// configuring a machine that isn't the one the domain points at yet.
func domainWarning(domain string) string {
	if !strings.Contains(domain, ".") {
		return "that doesn't look like a public domain, and a certificate authority\n    can only issue for one that resolves on the internet"
	}
	return ""
}

// askReachability is the typed-prompt version of the where-does-it-live pick.
func askReachability(r *bufio.Reader, fc *FileConfig) {
	fmt.Println()
	fmt.Println("Where are you running this?")
	fmt.Println("  1) Local network only — a machine on your LAN")
	fmt.Println("  2) The internet — reached at a domain name")
	def := "1"
	if reachOf(fc) == reachDomain {
		def = "2"
	}
	fmt.Printf("Choice [%s]: ", def)
	choice := readLine(r)
	if choice == "" {
		choice = def
	}
	if choice != "2" {
		applyReachability(fc, reachNetwork, "")
		return
	}
	fmt.Println()
	fmt.Println("  It doesn't have to exist yet — you can set the DNS up after.")
	fmt.Println("  HTTPS starts working once an A record points it at this machine;")
	fmt.Println("  that DNS entry is the proof of ownership the certificate")
	fmt.Println("  authority checks.")
	applyReachability(fc, reachDomain, ask(r, "Domain", fc.Domain, exampleDomain))
	if w := domainWarning(fc.Domain); w != "" {
		fmt.Println("  ! Note: " + w)
	}
}

// cleanDomain keeps just the host out of whatever got pasted — people type a
// scheme and a trailing slash out of habit, and a Caddyfile wants neither.
func cleanDomain(in string) string {
	out := strings.TrimSpace(in)
	out = strings.TrimPrefix(out, "https://")
	out = strings.TrimPrefix(out, "http://")
	return strings.Trim(out, "/")
}

// portProblem says why a port can't be used, or "" when it can.
//
// The floor is 1024, not 1. Everything below it is privileged on Linux and
// macOS, so a server configured onto 443 would save happily and then fail to
// start with a permission error that names nothing useful. Nothing else in this
// install path needs sudo, and a public port is a reverse proxy's job anyway —
// which is exactly what the domain answer sets up.
func portProblem(in string) string {
	n, err := strconv.Atoi(strings.TrimSpace(in))
	if err != nil {
		return "a port has to be a number"
	}
	if n < 1024 || n > 65535 {
		return "use a port between 1024 and 65535 (lower ones need root)"
	}
	return ""
}

// normalizePort keeps a usable port, falling back to what was already there
// rather than erroring on something unusable.
func normalizePort(in, current string) string {
	if portProblem(in) != "" {
		return firstNonEmpty(current, defaultPort)
	}
	return strings.TrimSpace(in)
}

// askPort is the typed-prompt version of the port question. It can't re-ask the
// way the menu path does — there may be nobody there — so it says what it did
// instead of silently discarding the answer.
func askPort(r *bufio.Reader, fc *FileConfig) {
	in := ask(r, "Port", fc.Port, defaultPort)
	if p := portProblem(in); p != "" {
		fmt.Printf("  ! %s — keeping %s\n", p, firstNonEmpty(fc.Port, defaultPort))
	}
	fc.Port = normalizePort(in, fc.Port)
}

// askDatabase is the typed-prompt version of the DB pick: run our own Postgres
// (embedded, the default) or use an external one (enter a URL).
func askDatabase(r *bufio.Reader, fc *FileConfig) {
	fmt.Println()
	fmt.Println("Where should Bombers store its data (Postgres)?")
	fmt.Println("  1) Run Postgres for me (embedded — nothing to install)")
	fmt.Println("  2) Use my own Postgres (enter a connection URL)")
	def := "1"
	if fc.DBBackend == "external" {
		def = "2"
	}
	if ask(r, "Choice", "", def) == "2" {
		applyExternalDB(fc, ask(r, "Postgres URL", fc.DatabaseURL, defaultDatabaseURL))
		return
	}
	if w := applyEmbeddedPG(fc); w != "" {
		fmt.Println("  ! Note: " + w)
	}
}

// applyEmbeddedPG selects the embedded Postgres backend: the server downloads +
// supervises a private Postgres bound to localhost, so there is nothing to
// install. It returns a warning to show, or "" when there's nothing to say — on
// a 32-bit build it can't run (no 386 Postgres is published), and saying so at
// the pick beats finding out at `bombers start`, which rejects it with the same
// guidance (embeddedpg.Start). Not a hard block: the config is portable, so they
// may rebuild 64-bit or run it elsewhere.
func applyEmbeddedPG(fc *FileConfig) string {
	fc.DBBackend = "embedded"
	if runtime.GOARCH == "386" {
		return "this is a 32-bit build, and embedded Postgres needs 64-bit — it won't\n    start until you rebuild with `GOARCH=amd64 go build` (or re-run setup\n    and choose your own Postgres)"
	}
	return ""
}

// applyExternalDB selects the external Postgres backend with a connection URL.
func applyExternalDB(fc *FileConfig, url string) {
	fc.DBBackend = "external"
	fc.DatabaseURL = strings.TrimSpace(url)
}

// applyFilesystemMedia selects the filesystem media backend: plain files under
// <dataDir>/media. No credentials, no daemon, nothing to run.
func applyFilesystemMedia(fc *FileConfig, dataDir string) {
	fc.MediaBackend = "fs"
	fc.MediaDir = filepath.Join(dataDir, "media")
}

// applyS3Media selects the S3/MinIO media backend with its endpoint and keys.
func applyS3Media(fc *FileConfig, endpoint, accessKey, secretKey, bucket string, useSSL bool) {
	fc.MediaBackend = "s3"
	fc.S3Endpoint = strings.TrimSpace(endpoint)
	fc.S3AccessKey = strings.TrimSpace(accessKey)
	fc.S3SecretKey = strings.TrimSpace(secretKey)
	fc.S3Bucket = strings.TrimSpace(bucket)
	fc.S3UseSSL = useSSL
}

// askMedia is the typed-prompt version of the media pick: plain files on this
// computer (the default — no daemon, no download) or an external S3/MinIO server.
func askMedia(r *bufio.Reader, fc *FileConfig, dataDir string) {
	fmt.Println()
	fmt.Println("Where should uploaded media (avatars, banners) be stored?")
	fmt.Println("  1) Store media as files on this computer (simplest, no extra setup)")
	fmt.Println("  2) Use an S3 / MinIO server (advanced)")
	def := "1"
	if fc.MediaBackend == "s3" {
		def = "2"
	}
	if ask(r, "Choice", "", def) != "2" {
		applyFilesystemMedia(fc, dataDir)
		return
	}
	applyS3Media(fc,
		ask(r, "S3 endpoint", fc.S3Endpoint, defaultS3Endpoint),
		ask(r, "S3 access key", fc.S3AccessKey, defaultS3AccessKey),
		ask(r, "S3 secret key", fc.S3SecretKey, defaultS3SecretKey),
		ask(r, "S3 bucket", fc.S3Bucket, defaultS3Bucket),
		askBool(r, "Use TLS for S3 (https)", fc.S3UseSSL),
	)
}

// ask prints "label [default]: " and returns the trimmed input, or the default
// (the current value if set, else fallback) when the line is empty.
func ask(r *bufio.Reader, label, current, fallback string) string {
	def := firstNonEmpty(current, fallback)
	fmt.Printf("%s [%s]: ", label, def)
	if in := readLine(r); in != "" {
		return in
	}
	return def
}

// askBool prompts a yes/no question defaulting to def; empty or unrecognized
// input keeps the default rather than erroring.
func askBool(r *bufio.Reader, label string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Printf("%s [%s]: ", label, hint)
	switch strings.ToLower(readLine(r)) {
	case "y", "yes", "true", "1":
		return true
	case "n", "no", "false", "0":
		return false
	default:
		return def
	}
}

// readLine reads one line from r and trims surrounding whitespace (including a
// trailing CRLF on Windows). A read error (EOF) yields whatever was buffered,
// so the wizard degrades gracefully on a truncated stream instead of panicking.
func readLine(r *bufio.Reader) string {
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

// firstNonEmpty returns a if it is non-empty, otherwise b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
