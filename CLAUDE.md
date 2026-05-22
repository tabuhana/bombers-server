# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Read these first

This repo is currently **spec-only** — no Go source has been written yet. Two documents define the work and must be read before touching anything:

1. **`PRODUCT.md`** — the product vision (the "what" and "why"). Decided. Don't re-litigate.
2. **`SERVER.md`** — the server spec (the "how"). The architectural frame; endpoint-level design is still planned interactively.

If a code decision contradicts either document, the document wins. `PRODUCT.md` wins over `SERVER.md` on conflict.

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

## Proposed layout (from `SERVER.md` §Project structure)

```
cmd/server/main.go            wiring, config, start
internal/auth/                tokens, sessions, middleware
internal/users/               accounts, friend codes
internal/friends/             friend graph, requests
internal/sync/                publish/pull, versioning, conflicts
internal/sharing/             shares, permission checks
internal/profiles/            about-cards + self-cards + visibility
internal/events/              events + invites
internal/messaging/           DM persistence + delivery
internal/rooms/               ephemeral room lifecycle
internal/realtime/            WebSocket hub, presence, indicators
internal/store/               pgx access, migrations
```

Each `internal/<domain>` owns its own routes, logic, and queries. Domains should stay loosely coupled — don't reach into another domain's internals.

## Mental model that drives everything

- **Local client = working copy. Server = published copy.** The server never edits; it stores what clients have published, and is the substrate for sharing. Don't invert this.
- The server is a **versioned key-value store of published items keyed by ULID, scoped to an account, plus a sharing/permission layer.** Keep this framing when designing sync endpoints.
- **Sync is triggered, not real-time.** Push/pull are explicit operations. The "feels live" layer is a WebSocket nudge → client auto-pulls (except the note the user is actively editing). See `SERVER.md` §Sync.
- **DMs persist forever in Postgres. Rooms persist nothing past expiry.** This asymmetry is intentional.
- **No media in v1.** No photo pass-through, no image storage, no blob handling of any kind. All heavy media is deferred to a single later S3 phase. Don't build it piecemeal.

## Commands

No `Makefile` or build scripts exist yet. Once `cmd/server/main.go` exists, the standard Go workflow applies:

```powershell
go build ./...                  # compile everything
go run ./cmd/server             # run the server (once it exists)
go test ./...                   # run all tests
go test ./internal/sync -run TestPush   # single test
go vet ./...
gofmt -w .
```

When adding migrations / DB tooling, document the chosen commands here.

## Hard constraints (refuse these — see `PRODUCT.md` and `SERVER.md`)

- No federation, cross-server, or multi-server logic.
- No public discovery / username search endpoints. Friend codes only.
- No media/blob handling in v1.
- No OS push infrastructure — in-app indicators only.
- No persisting rooms past expiry.
- No baking Tauri-client assumptions into the API.
- No making the server authoritative over the working copy.
