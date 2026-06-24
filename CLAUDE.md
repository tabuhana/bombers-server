# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Read these first

Two documents define the product and must be read before non-trivial work:

1. **`PRODUCT.md`** — the product vision (the "what" and "why"). Decided. Don't re-litigate.
2. **`SERVER.md`** — the server spec (the "how"). The architectural frame and the full sync/sharing/realtime contract; endpoint-level design for unbuilt domains is still planned interactively.

If a code decision contradicts either document, the document wins. `PRODUCT.md` wins over `SERVER.md` on conflict. But the specs are a moving snapshot of the owner's direction, not law — when his live direction differs, follow him and update the docs rather than citing old text back.

## Current state (what's built vs. spec)

The repo is **no longer spec-only** — a working HTTP API is in place. The doc above (`SERVER.md`) still describes the full target including unbuilt domains; this section is the source of truth for what actually exists today.

**Built (`internal/`):**

- `config/` — twelve-factor env loading (`config.Load`). Vars: `PORT` (default 8080), `DATABASE_URL` (required), `TOKEN_SECRET` (required), `CORS_ALLOWED_ORIGIN` (default `http://localhost:1420`, the Vite dev origin — not in the README's env table). `main.go` loads `.env` first.
- `store/` — `pgxpool` connection (`store.NewPool`), pings on startup.
- `httpx/` — shared JSON response/error helpers (`WriteJSON`, `WriteError`). All handlers use these.
- `types/` — shared domain types (e.g. `User`) used across packages.
- `auth/` — the JWT issuer (`token.go`: stateless HS256 access tokens, DB-tracked rotating refresh tokens with `jti`), `Service` (rotation/reuse detection), `RequireAuth` middleware, and request-context helpers. Owns all signing — other domains depend on it, never duplicate claim shapes. Access TTL 15m, refresh TTL 30d.
- `users/` — registration, login, `/me`, friend-code generation, username normalization/validation.
- `friends/` — friend graph: codes, requests (send/accept/reject), list, remove, block/unblock.
- `profiles/` — the **self-card**: a user's own published profile (display name, birthday, country, timezone, freeform bio + visibility `friends`/`private`). **Age is derived from birthday at read time, never stored.** `GET|PUT /me/profile` (own card), `GET /profiles/{userID}` (a friend's card, gated by accepted friendship + visibility; every not-allowed case collapses to an opaque `404 profile_not_found`). Owns a narrow `areFriends` query against the `friendships` table rather than importing `friends` (same loose-coupling tradeoff the domains already make). NB: this is only the *self-card*; the *about-card* (notes a user keeps about another) lives client-local for now and has no server domain yet.

**Routes (wired in `cmd/server/main.go`):** `GET /health`, `POST /auth/{register,login,refresh}` (register/login live on the `users` handler), and an auth-gated group (`issuer.RequireAuth`): `GET /me`, `GET /friends`, `GET /friends/code`, `GET|POST /friends/requests`, `POST /friends/requests/{requesterID}/{accept,reject}`, `DELETE /friends/{userID}`, `POST /friends/{userID}/{block,unblock}`, `GET|PUT /me/profile`, `GET /profiles/{userID}`.

**Migrations (`migrations/`):** `users`, `refresh_tokens`, `friendships`, `profiles`.

**Not yet built (still spec in `SERVER.md`):** `sync/`, `sharing/`, `events/`, `messaging/`, `rooms/`, `realtime/` (WebSocket). No published-content storage, sharing, or any real-time layer exists yet. The next domains follow the same package-per-domain shape. (Profiles covers only the self-card; about-card sharing and the rest of the profile spec remain unbuilt.)

## What this repo is

The **Bombers Server** — the Go backend half of Bombers. The Tauri/React client lives in a **separate codebase**; this repo is server-only. The API is the product, and the desktop client is just one consumer — never bake client-specific assumptions into endpoints.

Scope is friends-and-family (tens of users, one official VPS-hosted instance). Don't over-engineer for scale or federation; private self-hosted servers are explicitly isolated islands and unsupported (per `PRODUCT.md`).

## Stack (per `SERVER.md`, ratify during planning)

- Go 1.25, module `github.com/tabuhana/bombers-server`
- HTTP: `net/http` + `chi` router
- DB: PostgreSQL via `pgx` (or `sqlc` on top). No heavy ORMs.
- Auth: JWT (`golang-jwt/jwt/v5`) — username+password, short access token + long rotating refresh token
- IDs: ULIDs (`oklog/ulid/v2`) — the client's ULID is the join key for `PublishedItem`
- Real-time: WebSocket (library TBD — `nhooyr.io/websocket` or `gorilla/websocket`)
- Migrations: `goose` or `golang-migrate` (TBD)
- Config: env vars, twelve-factor

## Layout

Built today: `cmd/server/` (wiring) + `internal/{config,store,httpx,types,auth,users,friends,profiles}`. Planned per `SERVER.md`: `internal/{sync,sharing,events,messaging,rooms,realtime}`.

Each `internal/<domain>` owns its own routes, logic, and queries (typical files: `handler.go` HTTP, `service.go` logic, `store.go` pgx queries). Domains stay loosely coupled — don't reach into another domain's internals; depend on `auth` for tokens/middleware and `httpx`/`types`/`store` for shared plumbing. New routes are registered in `cmd/server/main.go` (auth-gated ones inside the `RequireAuth` group).

## Mental model that drives everything

- **Local client = working copy. Server = published copy.** The server never edits; it stores what clients have published, and is the substrate for sharing. Don't invert this.
- The server is a **versioned key-value store of published items keyed by ULID, scoped to an account, plus a sharing/permission layer.** Keep this framing when designing sync endpoints.
- **Sync is triggered, not real-time.** Push/pull are explicit operations. The "feels live" layer is a WebSocket nudge → client auto-pulls (except the note the user is actively editing). See `SERVER.md` §Sync.
- **DMs persist forever in Postgres. Rooms persist nothing past expiry.** This asymmetry is intentional.
- **No media in v1.** No photo pass-through, no image storage, no blob handling of any kind. All heavy media is deferred to a single later S3 phase. Don't build it piecemeal.

## Commands

Standard Go workflow (no `Makefile`):

```powershell
go build ./...                  # compile everything
go run ./cmd/server             # run the server (needs DATABASE_URL + TOKEN_SECRET)
go test ./...                   # run all tests (none exist yet)
go test ./internal/auth -run TestRotate   # single package / test
go vet ./...
gofmt -w .
```

A local Postgres is available via `docker compose up -d db` (uses `DB_NAME`/`DB_USER`/`DB_PASSWORD` from the environment; binds 5432). `server.exe` in the repo root is a stale committed build artifact — `.gitignore` excludes `*.exe`, so don't rely on or re-commit it; rebuild with `go build`.

### Migrations (goose)

Migrations live in `migrations/` at the repo root, written as `-- +goose Up` / `-- +goose Down` SQL files.

Install the CLI once:

```powershell
go install github.com/pressly/goose/v3/cmd/goose@latest
```

Run from the repo root (assumes `DATABASE_URL` is set, e.g. via `.env`):

```powershell
goose -dir migrations postgres $env:DATABASE_URL up        # apply all pending
goose -dir migrations postgres $env:DATABASE_URL status    # show applied/pending
goose -dir migrations postgres $env:DATABASE_URL down      # roll back last
goose -dir migrations postgres $env:DATABASE_URL create <name> sql   # new migration
```

New migration filenames are timestamped (`YYYYMMDDHHMMSS_<name>.sql`).

## Hard constraints (refuse these — see `PRODUCT.md` and `SERVER.md`)

- No federation, cross-server, or multi-server logic.
- No public discovery / username search endpoints. Friend codes only.
- No media/blob handling in v1.
- No OS push infrastructure — in-app indicators only.
- No persisting rooms past expiry.
- No baking Tauri-client assumptions into the API.
- No making the server authoritative over the working copy.
