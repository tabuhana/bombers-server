package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
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
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/media"
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

	// Load .env before configuring the logger so LOG_TIME_FORMAT / NO_COLOR from
	// the file take effect; hold any real load error until logx is up to report
	// it in the leveled format.
	envErr := godotenv.Load()
	logx.Init()
	if envErr != nil && !errors.Is(envErr, fs.ErrNotExist) {
		logx.Fatal("loading .env: %v", envErr)
	}

	// The banner is decorative — print it on a real terminal only so piped or
	// redirected logs stay clean.
	if logx.Interactive() {
		printBanner(os.Stdout)
	}

	cfg, err := config.Load()
	if err != nil {
		logx.Fatal("%v", err)
	}
	logx.Info("config loaded (.env)")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := store.NewPool(ctx, cfg.DatabaseURL)
	cancel()
	if err != nil {
		logx.Fatal("connecting to database: %v", err)
	}
	defer pool.Close()
	logx.Info("db connected")

	// Object storage (profile media) is part of the stack like Postgres is:
	// unreachable at startup → fatal, same as the DB. Ensures the bucket too.
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	storage, err := media.NewStorage(ctx, cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3UseSSL)
	cancel()
	if err != nil {
		logx.Fatal("connecting to object storage: %v", err)
	}
	logx.Info("media bucket %q ready", cfg.S3Bucket)

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
	mediaHandler := media.NewHandler(pool, storage)

	r := chi.NewRouter()
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{cfg.CorsAllowedOrigin},
		AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowedHeaders: []string{"Authorization", "Content-Type"},
	}))
	r.Get("/health", healthHandler(pool, storage))
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
		// Static /messages/unread is registered before the /messages/{userID}
		// param route so chi matches it first (it would anyway — static beats
		// wildcard — but keep them together).
		r.Get("/messages/unread", messagingHandler.Unread)
		r.Get("/messages/{userID}", messagingHandler.History)
		r.Post("/messages/{userID}/read", messagingHandler.MarkRead)
		r.Post("/sync/push", syncHandler.Push)
		r.Get("/sync/pull", syncHandler.Pull)
		r.Get("/sync/status", syncHandler.Status)
		// The official node store: operator-published SDK bundles. Any authed
		// user browses + installs; publishing is console-only (no HTTP publish).
		r.Get("/nodes", nodesHandler.List)
		r.Get("/nodes/{id}/bundle", nodesHandler.Download)
		// Friend node-sharing (the clone model): a one-way transfer inbox,
		// friend-gated — separate from the public node-store catalog above.
		r.Post("/nodes/send", nodeshareHandler.Send)
		r.Get("/nodes/received", nodeshareHandler.ListReceived)
		r.Delete("/nodes/received/{id}", nodeshareHandler.Dismiss)
		// Profile media (avatar/banner): raw image bytes in, proxied bytes
		// out. Serving is friendship + profile-visibility gated pass-through.
		r.Put("/me/media/{kind}", mediaHandler.Upload)
		r.Delete("/me/media/{kind}", mediaHandler.Delete)
		r.Get("/media/{userID}/{kind}", mediaHandler.Serve)
	})

	startedAt := time.Now()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: r}
	// Log the listen line from the MAIN goroutine, before the console prints its
	// prompt — otherwise this fires from the serve goroutine a beat later and
	// lands on the "bombers> " line, eating the prompt.
	logx.Info("http listening on %s", srv.Addr)
	go func() {
		// A serve error at startup (e.g. port in use) is fatal in both modes;
		// ErrServerClosed is the normal graceful-shutdown exit.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logx.Fatal("http server: %v", err)
		}
	}()

	// DEFAULT: the interactive operator console (Minecraft-style) on stdin.
	// --headless skips it, and so does a non-TTY stdin (service managers,
	// `< /dev/null`) — those wait for SIGINT/SIGTERM like a normal daemon. A
	// console that hits EOF also falls back to signal-waiting rather than
	// exiting or spinning.
	stopped := false
	if !*headless && console.Interactive(os.Stdin) {
		logx.Info(`console ready — type "help", "stop" to quit`)
		stopped = console.New(pool, storage, startedAt).Run()
		if !stopped {
			logx.Warn("console input ended; running headless (SIGINT/SIGTERM to stop)")
		}
	}
	if !stopped {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		s := <-sig
		logx.Info("received %s, shutting down", s)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logx.Error("graceful shutdown: %v", err)
	}
	logx.Info("server stopped")
}

func healthHandler(pool *pgxpool.Pool, storage *media.Storage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		// The DB governs status and the HTTP code (unchanged contract: clients
		// gate login on it). The media field is informational — object storage
		// down degrades only the media endpoints, not the core product.
		mediaState := "up"
		if err := storage.Ping(ctx); err != nil {
			mediaState = "down"
		}

		w.Header().Set("Content-Type", "application/json")
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"degraded","db":"down","media":"` + mediaState + `"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","db":"up","media":"` + mediaState + `"}`))
	}
}
