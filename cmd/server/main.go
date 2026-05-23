package main

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/config"
	"github.com/tabuhana/bombers-server/internal/store"
	"github.com/tabuhana/bombers-server/internal/users"
)

func main() {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Fatalf("loading .env: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := store.NewPool(ctx, cfg.DatabaseURL)
	cancel()
	if err != nil {
		log.Fatalf("connecting to database: %v", err)
	}
	defer pool.Close()

	issuer := auth.NewIssuer(cfg.TokenSecret)
	usersHandler := users.NewHandler(pool, issuer)

	r := chi.NewRouter()
	r.Get("/health", healthHandler(pool))
	r.Post("/auth/register", usersHandler.Register)
	r.Post("/auth/login", usersHandler.Login)

	r.Group(func(r chi.Router) {
		r.Use(issuer.RequireAuth)
		r.Get("/me", usersHandler.Me)
	})

	addr := ":" + cfg.Port
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatal(err)
	}
}

func healthHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"degraded","db":"down"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","db":"up"}`))
	}
}
