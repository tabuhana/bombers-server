package config

import (
	"fmt"
	"os"
	"strings"
)

const (
	defaultPort               = "8080"
	defaultCorsAllowedOrigin  = "http://localhost:1420"
)

type Config struct {
	Port              string
	DatabaseURL       string
	TokenSecret       string
	CorsAllowedOrigin string
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

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	corsAllowedOrigin := os.Getenv("CORS_ALLOWED_ORIGIN")
	if corsAllowedOrigin == "" {
		corsAllowedOrigin = defaultCorsAllowedOrigin
	}

	cfg := &Config{
		Port:              port,
		DatabaseURL:       require("DATABASE_URL"),
		TokenSecret:       require("TOKEN_SECRET"),
		CorsAllowedOrigin: corsAllowedOrigin,
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}
