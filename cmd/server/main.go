package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/config"
	"github.com/tabuhana/bombers-server/internal/console"
	"github.com/tabuhana/bombers-server/internal/friends"
	"github.com/tabuhana/bombers-server/internal/messaging"
	"github.com/tabuhana/bombers-server/internal/nodes"
	"github.com/tabuhana/bombers-server/internal/nodeshare"
	"github.com/tabuhana/bombers-server/internal/profiles"
	"github.com/tabuhana/bombers-server/internal/store"
	"github.com/tabuhana/bombers-server/internal/sync"
	"github.com/tabuhana/bombers-server/internal/users"
)

func main() {
	headless := flag.Bool("headless", false,
		"serve without the interactive admin console (stop with SIGINT/SIGTERM)")
	flag.Parse()

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
	messagingHandler := messaging.NewHandler(pool)
	syncHandler := sync.NewHandler(pool)
	nodesHandler := nodes.NewHandler(pool)
	nodeshareHandler := nodeshare.NewHandler(pool)

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
		r.Get("/me/about", profilesHandler.ListMyAbout)
		r.Get("/me/about/{subjectID}", profilesHandler.GetMyAbout)
		r.Put("/me/about/{subjectID}", profilesHandler.UpsertMyAbout)
		r.Delete("/me/about/{subjectID}", profilesHandler.DeleteMyAbout)
		r.Get("/about/{authorID}", profilesHandler.GetSharedAbout)
		r.Post("/messages", messagingHandler.Send)
		r.Get("/messages/{userID}", messagingHandler.History)
		r.Post("/sync/push", syncHandler.Push)
		r.Get("/sync/pull", syncHandler.Pull)
		r.Get("/sync/status", syncHandler.Status)
		// Node store (the installable-node catalog + bundles). Browse/download for
		// any authed user; publish is owner-curated (owner-gating is a follow-up).
		r.Get("/nodes", nodesHandler.List)
		r.Get("/nodes/{id}/bundle", nodesHandler.Download)
		r.Post("/nodes", nodesHandler.Publish)
		// Friend node-sharing (the clone model): a one-way transfer inbox,
		// friend-gated — separate from the public node-store catalog above.
		r.Post("/nodes/send", nodeshareHandler.Send)
		r.Get("/nodes/received", nodeshareHandler.ListReceived)
		r.Delete("/nodes/received/{id}", nodeshareHandler.Dismiss)
	})

	startedAt := time.Now()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: r}
	go func() {
		log.Printf("listening on %s", srv.Addr)
		// A serve error at startup (e.g. port in use) is fatal in both modes;
		// ErrServerClosed is the normal graceful-shutdown exit.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	// DEFAULT: the interactive operator console (Minecraft-style) on stdin.
	// --headless skips it, and so does a non-TTY stdin (service managers,
	// `< /dev/null`) — those wait for SIGINT/SIGTERM like a normal daemon. A
	// console that hits EOF also falls back to signal-waiting rather than
	// exiting or spinning.
	stopped := false
	if !*headless && console.Interactive(os.Stdin) {
		stopped = console.New(pool, startedAt).Run()
		if !stopped {
			log.Printf("console input ended; running headless (SIGINT/SIGTERM to stop)")
		}
	}
	if !stopped {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		s := <-sig
		log.Printf("received %s, shutting down", s)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown: %v", err)
	}
	log.Printf("server stopped")
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
