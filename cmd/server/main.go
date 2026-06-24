package main

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/config"
	"github.com/tabuhana/bombers-server/internal/friends"
	"github.com/tabuhana/bombers-server/internal/profiles"
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
	authService := auth.NewService(issuer, pool)
	authHandler := auth.NewHandler(authService)
	usersHandler := users.NewHandler(pool, authService)
	friendsHandler := friends.NewHandler(pool)
	profilesHandler := profiles.NewHandler(pool)

	r := chi.NewRouter()
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{cfg.CorsAllowedOrigin},
		AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowedHeaders: []string{"Authorization", "Content-Type"},
	}))
	r.Get("/health", healthHandler(pool))
	r.Post("/auth/register", usersHandler.Register)
	r.Post("/auth/login", usersHandler.Login)
	r.Post("/auth/refresh", authHandler.Refresh)

	r.Group(func(r chi.Router) {
		r.Use(issuer.RequireAuth)
		r.Get("/me", usersHandler.Me)
		r.Get("/friends", friendsHandler.List)
		r.Get("/friends/code", friendsHandler.MyCode)
		r.Get("/friends/requests", friendsHandler.ListRequests)
		r.Post("/friends/requests", friendsHandler.SendRequest)
		r.Post("/friends/requests/{requesterID}/accept", friendsHandler.AcceptRequest)
		r.Post("/friends/requests/{requesterID}/reject", friendsHandler.RejectRequest)
		r.Delete("/friends/{userID}", friendsHandler.RemoveFriend)
		r.Post("/friends/{userID}/block", friendsHandler.Block)
		r.Post("/friends/{userID}/unblock", friendsHandler.Unblock)
		r.Get("/me/profile", profilesHandler.GetMine)
		r.Put("/me/profile", profilesHandler.UpdateMine)
		r.Get("/profiles/{userID}", profilesHandler.GetForUser)
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
