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
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const configFile = "config.json"

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

// Wizard runs the first-run prompts on stdin, writing answers into fc. It's a
// straight walk-through — one decision at a time, each a choice plus its config
// where the choice needs one: reachability, port, database (embedded / your own
// URL), then media (local files / S3-MinIO). Each prompt shows the
// current-or-default value in [brackets]; an empty line keeps it. dataDir is
// where the filesystem media root is placed (<dataDir>/media). Callers gate this
// on an interactive terminal, then run EnsureSecret + Save.
func Wizard(fc *FileConfig, dataDir string) {
	r := bufio.NewReader(os.Stdin)

	fmt.Println()
	fmt.Println("Bombers local setup — press Enter to accept each [default].")

	// Walk each decision in order — reachability, port, database, then media —
	// choosing (and configuring) each piece as you go. No bundled preset.
	fc.Host = askReachability(r, fc)
	fc.Port = askPort(r, fc)
	askDatabase(r, fc)
	askMedia(r, fc, dataDir)

	fmt.Println()
}

// askReachability asks who can reach the server and returns the bind host:
// 127.0.0.1 (this machine only, the default) or 0.0.0.0 (the LAN, over plain
// HTTP — fine on a trusted network, per LOCAL_MODE.md §8/§10).
func askReachability(r *bufio.Reader, fc *FileConfig) string {
	fmt.Println()
	fmt.Println("Who should be able to reach this server?")
	fmt.Println("  1) This computer only")
	fmt.Println("  2) Other devices on my network (LAN) — plain HTTP, trusted networks only")
	def := "1"
	if fc.Host == "0.0.0.0" {
		def = "2"
	}
	fmt.Printf("Choice [%s]: ", def)
	switch readLine(r) {
	case "":
		return hostFor(def)
	case "2":
		return "0.0.0.0"
	default:
		return "127.0.0.1"
	}
}

// askPort asks for the HTTP port, keeping the current-or-1337 default and
// falling back to it on non-numeric input rather than erroring.
func askPort(r *bufio.Reader, fc *FileConfig) string {
	port := ask(r, "Port", fc.Port, "1337")
	if _, err := strconv.Atoi(port); err != nil {
		return firstNonEmpty(fc.Port, "1337")
	}
	return port
}

// askDatabase is the customize-path DB pick: run our own Postgres (embedded, the
// default) or use an external one (enter a URL).
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
		askDatabaseURL(r, fc)
	} else {
		askEmbeddedPG(fc)
	}
}

// askEmbeddedPG selects the embedded Postgres backend: the server downloads +
// supervises a private Postgres bound to localhost, so there is nothing to
// install. On a 32-bit build it can't run (no 386 Postgres is published), so warn
// now; `bombers start` rejects it with the same guidance (embeddedpg.Start). Not
// a hard block — the config is portable, so they may rebuild 64-bit or run it
// elsewhere.
func askEmbeddedPG(fc *FileConfig) {
	fc.DBBackend = "embedded"
	if runtime.GOARCH == "386" {
		fmt.Println("  ! Note: this is a 32-bit build, and embedded Postgres needs 64-bit —")
		fmt.Println("    it won't start until you rebuild with `GOARCH=amd64 go build`")
		fmt.Println("    (or re-run setup and choose your own Postgres).")
	}
}

// askDatabaseURL selects the external Postgres backend and prompts for the
// connection URL (default matches the repo's docker-compose).
func askDatabaseURL(r *bufio.Reader, fc *FileConfig) {
	fc.DBBackend = "external"
	fc.DatabaseURL = ask(r, "Postgres URL", fc.DatabaseURL,
		"postgresql://admin:adminpassword@localhost:5432/bombers")
}

// askMedia is the customize-path media pick: plain files on this computer (the
// default — no daemon, no download) or an external S3/MinIO server.
func askMedia(r *bufio.Reader, fc *FileConfig, dataDir string) {
	fmt.Println()
	fmt.Println("Where should uploaded media (avatars, banners) be stored?")
	fmt.Println("  1) Store media as files on this computer (simplest, no extra setup)")
	fmt.Println("  2) Use an S3 / MinIO server (advanced)")
	def := "1"
	if fc.MediaBackend == "s3" {
		def = "2"
	}
	if ask(r, "Choice", "", def) == "2" {
		askS3Details(r, fc)
	} else {
		askFilesystem(fc, dataDir)
	}
}

// askFilesystem selects the filesystem media backend: plain files under
// <dataDir>/media. No S3 prompts, no daemon, nothing to run.
func askFilesystem(fc *FileConfig, dataDir string) {
	fc.MediaBackend = "fs"
	fc.MediaDir = filepath.Join(dataDir, "media")
}

// askS3Details selects the S3/MinIO media backend and prompts for its endpoint +
// credentials (defaults match the repo's docker-compose MinIO).
func askS3Details(r *bufio.Reader, fc *FileConfig) {
	fc.MediaBackend = "s3"
	fc.S3Endpoint = ask(r, "S3 endpoint", fc.S3Endpoint, "localhost:9000")
	fc.S3AccessKey = ask(r, "S3 access key", fc.S3AccessKey, "bombers")
	fc.S3SecretKey = ask(r, "S3 secret key", fc.S3SecretKey, "hanascript")
	fc.S3Bucket = ask(r, "S3 bucket", fc.S3Bucket, "bombers-media")
	fc.S3UseSSL = askBool(r, "Use TLS for S3 (https)", fc.S3UseSSL)
}

// hostFor maps the reachability choice to a bind host.
func hostFor(choice string) string {
	if choice == "2" {
		return "0.0.0.0"
	}
	return "127.0.0.1"
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
