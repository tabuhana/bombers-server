package main

import (
	"errors"
	"io/fs"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/joho/godotenv"

	"github.com/tabuhana/bombers-server/internal/config"
)

func main() {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Fatalf("loading .env: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	addr := ":" + cfg.Port
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatal(err)
	}
}
