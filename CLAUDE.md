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
- `profiles/` — both profile cards. **Self-card** (`profiles` table, `handler.go`/`store.go`): a user's own published profile (display name, birthday, country, timezone, freeform bio + visibility `friends`/`private`); **age derived from birthday at read time, never stored**. `GET|PUT /me/profile`, `GET /profiles/{userID}` (gated by accepted friendship + visibility → opaque `404 profile_not_found`). **About-card** (`about_cards` table, `about_handler.go`/`about_store.go`): the private notes user A keeps *about* user B. The rich field set is stored as an **opaque JSONB `content` blob** (server never inspects it; client owns the shape) + a `visibility` of `private` (author only, default) or `subject` (the person it's about may read it too). `GET /me/about` (list mine, for restore), `GET|PUT|DELETE /me/about/{subjectID}` (PUT gated on accepted friendship → opaque `404 subject_not_friend`), `GET /about/{authorID}` (what a friend shared about me → opaque `404 about_not_found`). The domain owns a narrow `areFriends` query against the `friendships` table rather than importing `friends` (same loose-coupling tradeoff the domains already make).
- `sync/` — **publish/pull**, the heart of client↔server (`SERVER.md` §Sync). The server is a **versioned key-value store of published items** keyed by the client's ULID, scoped to an account; it never edits content. `published_items` table keyed by `(owner_id, id)`, with `content` (opaque JSONB), client `updated_at` (authoritative for **last-write-wins**), a `deleted` tombstone (so deletes propagate), and `server_updated_at` (drives incremental pulls). `POST /sync/push` upserts a batch in one transaction, LWW by `updated_at`, returning each item as `applied` (server took it) or `stale` (server kept a newer copy → client should pull). `GET /sync/pull[?since=<RFC3339>]` returns the caller's items (full, or only those server-written since the cursor), tombstones included. `GET /sync/status` returns `last_synced_at` + live item count (a `sync_state` row bumped on each push). **Triggered, not streamed** — real-time nudges that make pull "feel live" are the later `realtime/` WebSocket layer; pulling items others shared with me is the later `sharing/` domain (today pull is own-items only). NB: the package is named `sync` (shadows stdlib `sync` within its files — it needs neither).
- `nodes/` — the **official node store** (`handler.go`/`store.go`, table `nodes`): this server's OPERATOR-published nodes in the SDK **`{manifest, files}`** format (the same opaque-JSONB bundle shape `nodeshare` carries; 4 MiB cap). The table denormalizes `id`/`name`/`version` out of the manifest for cheap listings. `GET /nodes` (catalog — id/name/version + icon/description/tags pulled from each manifest, never the files), `GET /nodes/{id}/bundle` (the full `{manifest, files}` JSON for install; the `/bundle` suffix keeps it clear of nodeshare's static `/nodes/received`; unknown id → `404 node_not_found`). **There is NO HTTP publish** — publishing is operator-only via the console (`publish <path>` / `unpublish <id>` / `store`); a self-hosted server's operator curates their own store the same way (isolated per server). The old any-user `POST /nodes` + single-file `node.js` bytea format were removed by the `20260706130000` migration (drop + recreate; nothing was published in the old format).
- `console/` — the **interactive admin console** the binary runs on stdin by default (Minecraft-style `bombers>` prompt; skip with `--headless`): a small command registry + loop — `help`, `users` (username/id/created), `status` (uptime, DB ping, row counts), the **node-store publish surface** (`publish <path>` — read + validate a `{manifest, files}` JSON file and upsert it into the store; `unpublish <id>`; `store` — list published nodes; see `examples/sample-store-node.json`), and `stop` (aliases `quit`/`exit`) → graceful `http.Server.Shutdown`. Local-operator-privileged by definition (whoever holds the terminal — no auth); the store commands are the deliberate step past read-only (the console IS the store's only publish path). Other destructive/admin-role commands (delete user, an `is_admin` column, env admin bootstrap) remain a LATER follow-up. A non-TTY stdin or console EOF falls back to headless signal-waiting (never spins). `main.go` serves via `http.Server` in a goroutine and shuts down gracefully in every mode.
- `nodeshare/` — **friend node-sharing** (`handler.go`/`store.go`, table `node_transfers`): a lightweight inbox of node bundles sent friend-to-friend — the clone model, NOT the public node store above. A transfer is a **one-way copy**: `POST /nodes/send` (`{recipient_id, bundle}` — recipient must be an **accepted friend**, every not-allowed case collapses to an opaque `404 recipient_not_found`; 4 MiB cap), `GET /nodes/received` (my inbox, sender username joined in, newest first), `DELETE /nodes/received/{id}` (dismiss after handling — recipient only, opaque `404 transfer_not_found`). The `bundle` is the client's `{manifest, files}` node source stored as **opaque JSONB** (dumb-blob, like sync — the server never inspects it). No live link, no ownership; the recipient clones it into a project they own. Owns the same narrow `areFriends` query as `messaging`/`profiles`.
- `messaging/` — **direct messages**: text DMs between two users, persisted **indefinitely** in Postgres (DMs aren't ephemeral; rooms are). `POST /messages` (send to an accepted friend), `GET /messages/{userID}` (conversation history, oldest→newest, capped at the most recent `?limit=` — default 100, max 200). You may only message an accepted friend; every not-allowed case (non-friend, nonexistent, self) collapses to an opaque `404 recipient_not_found`. Owns the same narrow `areFriends` query as `profiles`. **This is the durable REST layer only** — real-time delivery (WebSocket push, offline queue, "feels live" indicators) is deferred to the unbuilt `realtime/` domain; today a client loads history on open and appends what it sends. **Text only in v1** — image/file attachments wait for the S3 phase.

**Routes (wired in `cmd/server/main.go`):** `GET /health`, `POST /auth/{register,login,refresh}` (register/login live on the `users` handler), and an auth-gated group (`issuer.RequireAuth`): `GET /me`, `GET /friends`, `GET /friends/code`, `GET|POST /friends/requests`, `POST /friends/requests/{requesterID}/{accept,reject}`, `DELETE /friends/{userID}`, `POST /friends/{userID}/{block,unblock}`, `GET|PUT /me/profile`, `GET /profiles/{userID}`, `GET /me/about`, `GET|PUT|DELETE /me/about/{subjectID}`, `GET /about/{authorID}`, `POST /messages`, `GET /messages/{userID}`, `POST /sync/push`, `GET /sync/pull`, `GET /sync/status`, `GET /nodes`, `GET /nodes/{id}/bundle`, `POST /nodes/send`, `GET /nodes/received`, `DELETE /nodes/received/{id}`. (No `POST /nodes` — store publishing is console-only.)

**Migrations (`migrations/`):** `users`, `refresh_tokens`, `friendships`, `profiles`, `messages`, `about_cards`, `published_items` (+ `sync_state`), `nodes` (created `20260625`, reshaped to SDK `{manifest, files}` JSONB bundles by `20260706130000_rework_nodes_store`), `node_transfers`.

**Not yet built (still spec in `SERVER.md`):** `sharing/`, `events/`, `rooms/`, `realtime/` (WebSocket). Published-content storage now exists (`sync/`); sharing and any real-time layer do not yet. The next domains follow the same package-per-domain shape. (Profiles now covers both self-card and about-card; what remains is *specific-friend* self-card visibility and sharing an about-card with friends other than its subject. Messaging is REST persistence only — its real-time delivery lands with `realtime/`.)

## What this repo is

The **Bombers Server** — the Go backend half of Bombers. The Tauri/React client lives in a **separate codebase**; this repo is server-only. The API is the product, and the desktop client is just one consumer — never bake client-specific assumptions into endpoints.

Scope is friends-and-family (tens of users per instance). Don't over-engineer for scale or federation; **self-hosting is supported and first-class** — anyone can run the binary as their own server, and the client picks a server on its login screen — but every server (official or private) is an **isolated island**: no cross-server anything (per `PRODUCT.md`; how to run one: `SERVER.md` §Self-hosting / Running).

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

Built today: `cmd/server/` (wiring) + `internal/{config,store,httpx,types,auth,users,friends,profiles,messaging,sync,nodes}`. Planned per `SERVER.md`: `internal/{sharing,events,rooms,realtime}`.

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
go run ./cmd/server             # run the server (needs DATABASE_URL + TOKEN_SECRET); opens the
                                # interactive `bombers>` console — pass --headless to just serve
go run ./cmd/server --headless  # daemon mode: no console, stops on SIGINT/SIGTERM
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
