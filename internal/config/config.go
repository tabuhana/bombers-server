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

type Config struct {
	Port              string
	DatabaseURL       string
	TokenSecret       string
	CorsAllowedOrigin string

	// S3-compatible object storage (profile media). Endpoint is host:port
	// without a scheme; S3UseSSL selects http/https.
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

	cfg := &Config{
		Port:              optional("PORT", defaultPort),
		DatabaseURL:       require("DATABASE_URL"),
		TokenSecret:       require("TOKEN_SECRET"),
		CorsAllowedOrigin: optional("CORS_ALLOWED_ORIGIN", defaultCorsAllowedOrigin),
		S3Endpoint:        optional("S3_ENDPOINT", defaultS3Endpoint),
		S3AccessKey:       require("S3_ACCESS_KEY"),
		S3SecretKey:       require("S3_SECRET_KEY"),
		S3Bucket:          optional("S3_BUCKET", defaultS3Bucket),
		S3UseSSL:          os.Getenv("S3_USE_SSL") == "true",
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}
