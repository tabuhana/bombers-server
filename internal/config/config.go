package config

import (
	"fmt"
	"os"
	"strings"
)

const defaultPort = "8080"

type Config struct {
	Port        string
	DatabaseURL string
	TokenSecret string
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

	cfg := &Config{
		Port:        port,
		DatabaseURL: require("DATABASE_URL"),
		TokenSecret: require("TOKEN_SECRET"),
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}
