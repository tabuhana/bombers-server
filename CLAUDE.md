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

- `config/` — twelve-factor env loading (`config.Load`). Vars: `PORT` (default 8080), `DATABASE_URL` (required), `TOKEN_SECRET` (required), `CORS_ALLOWED_ORIGIN` (default `http://localhost:1420`, the Vite dev origin), `ADMIN_USERNAME` (optional — promotes that account to admin at boot; an unknown name warns rather than failing startup), and the S3 block for profile media: `S3_ACCESS_KEY`/`S3_SECRET_KEY` (required), `S3_ENDPOINT` (default `localhost:9000`), `S3_BUCKET` (default `bombers-media`), `S3_USE_SSL` (default false). `main.go` loads `.env` first.
- `store/` — `pgxpool` connection (`store.NewPool`), pings on startup.
- `httpx/` — shared JSON response/error helpers (`WriteJSON`, `WriteError`). All handlers use these.
- `logx/` — the shared **leveled, colored logger** (stdlib only; a leaf every package imports, so no cycles). One line per call: `[<timestamp>][<LEVEL>]: <message>` at `INFO`/`WARN`/`ERROR`/`FATAL` (`Fatal` exits 1); the level label is width-padded so the `:` aligns. 24-bit truecolor auto-enables **only** when stdout is a character device and `NO_COLOR` is unset (piped/redirected output is plain and greppable, and the launch banner is skipped). `LOG_TIME_FORMAT` (`time`|`datetime`|`iso`, default `datetime`) sets the timestamp layout at boot; the console's `logtime` command switches it live (guarded by an `RWMutex`). `Init()` runs once in `main` right after the `.env` load. Every domain's request-error logs and `main`'s startup/lifecycle lines route through this — no package imports `log` anymore.
- `types/` — shared domain types (e.g. `User`) used across packages.
- `auth/` — the JWT issuer (`token.go`: stateless HS256 access tokens, DB-tracked rotating refresh tokens with `jti`), `Service` (rotation/reuse detection), `RequireAuth` middleware, and request-context helpers. Owns all signing — other domains depend on it, never duplicate claim shapes. Access TTL 15m, refresh TTL 30d.
- `users/` — registration, login, `/me`, friend-code generation, username normalization/validation.
- `friends/` — friend graph: codes, requests (send/accept/reject), list, remove, block/unblock.
- `profiles/` — both profile cards. **Self-card** (`profiles` table, `handler.go`/`store.go`): a user's own published profile (display name, birthday, country, timezone, freeform bio + visibility `friends`/`private`); **age derived from birthday at read time, never stored**. `GET|PUT /me/profile`, `GET /profiles/{userID}` (gated by accepted friendship + visibility → opaque `404 profile_not_found`). Profile responses expose `avatar_url`/`banner_url` (null until uploaded) via a narrow read of `media/`'s `user_media` table + the shared `types.MediaURL` shape. **About-card** (`about_cards` table, `about_handler.go`/`about_store.go`): the private notes user A keeps *about* user B. The rich field set is stored as an **opaque JSONB `content` blob** (server never inspects it; client owns the shape) + a `visibility` of `private` (author only, default) or `subject` (the person it's about may read it too). `GET /me/about` (list mine, for restore), `GET|PUT|DELETE /me/about/{subjectID}` (PUT gated on accepted friendship → opaque `404 subject_not_friend`), `GET /about/{authorID}` (what a friend shared about me → opaque `404 about_not_found`). The domain owns a narrow `areFriends` query against the `friendships` table rather than importing `friends` (same loose-coupling tradeoff the domains already make). **The person card is two halves** (`cards_handler.go`/`cards_store.go`, table `published_cards`). FACTS — birthday/age, country, timezone, city, nickname — are the same for everyone you're linked to, so they are plain columns any accepted friend reads; `GET /profiles/{userID}` does NO per-viewer redaction. NOTES are the half that varies by person (shared per category, with any single note able to depart from its category's audience), and expressing that server-side would mean a row per note per viewer and a server that understands what a note is. So the CLIENT decides and publishes the result: `PUT /me/card` takes `{viewerId: <opaque blob>}` and REPLACES the whole set (a viewer left out has no row, and no row is no access — revocation with nothing to withdraw), silently dropping ids that aren't accepted friends; `GET /cards/{ownerID}` returns what that owner published for the caller, friend-gated, with every refusal collapsing to the same opaque `404 card_not_found` (a reader must not be able to tell "they wrote nothing" from "they wrote plenty, none for you"). `cardResponse` SPLICES the stored blob rather than re-encoding it, so what was published is byte-for-byte what arrives — a round trip through Go would reorder keys and drop anything the server's types don't model. Caps: 1 MiB per card, 500 viewers per publish. **The predecessor (`profile_shares`, per-field grants + server-side redaction) was deleted** along with `profiles.notes`: facts needed no grant and notes needed more than one, and two mechanisms for who-sees-what is the thing most likely to disagree with itself later.
- `presence/` — **who's around** (`presence.go`/`handler.go`, table `presence`): one row per user holding the status they CHOSE and when they were last HEARD FROM. Neither is the answer alone — a stored `online` from a laptop that shut three days ago is a lie, and a fresh heartbeat from somebody who set themselves offline is none of your business — so `Effective` computes what a friend actually sees at read time and never stores it (the same rule profiles follows for age). `offline` is honoured exactly (invisible AND disconnected, so a heartbeat can't override it); everything else expires after `StaleAfter` (3 min, against the client's 60s beat — generous on purpose, because a presence dot that flickers is one nobody reads). `PUT /me/presence` is the status AND the heartbeat in one write, because two calls that must agree are two calls that can disagree; `GET /presence` answers for every accepted friend at once, with never-seen friends coming back `offline` rather than missing. Friends only — there is no way to ask about a stranger, matching the no-username-search rule. **No history**: the table only ever answers "now", since a log of when somebody was at their computer is a surveillance feature wearing an analytics hat. Owns the same narrow `areFriends`-style join the other domains do.
- `sync/` — **publish/pull**, the heart of client↔server (`SERVER.md` §Sync). The server is a **versioned key-value store of published items** keyed by the client's ULID, scoped to an account; it never edits content. `published_items` table keyed by `(owner_id, id)`, with `content` (opaque JSONB), client `updated_at` (authoritative for **last-write-wins**), a `deleted` tombstone (so deletes propagate), and `server_updated_at` (drives incremental pulls). `POST /sync/push` upserts a batch in one transaction, LWW by `updated_at`, returning each item as `applied` (server took it) or `stale` (server kept a newer copy → client should pull). `GET /sync/pull[?since=<RFC3339>]` returns the caller's items (full, or only those server-written since the cursor), tombstones included. `GET /sync/status` returns `last_synced_at` + live item count (a `sync_state` row bumped on each push). **Triggered, not streamed** — real-time nudges that make pull "feel live" are the later `realtime/` WebSocket layer; pulling items others shared with me is the later `sharing/` domain (today pull is own-items only). NB: the package is named `sync` (shadows stdlib `sync` within its files — it needs neither).
- `nodes/` — the **official node store** (`handler.go`/`store.go`, table `nodes`): this server's OPERATOR-published nodes in the SDK **`{manifest, files}`** format (the same opaque-JSONB bundle shape `nodeshare` carries; 4 MiB cap). The table denormalizes `id`/`name`/`version` out of the manifest for cheap listings. `GET /nodes` (catalog — id/name/version + icon/description/tags pulled from each manifest, never the files), `GET /nodes/{id}/bundle` (the full `{manifest, files}` JSON for install; the `/bundle` suffix keeps it clear of nodeshare's static `/nodes/received`; unknown id → `404 node_not_found`). Publishing is **operator-only**, two ways into the same operation: the console (`publish <path>` / `unpublish <id>` / `store`), or the **admin-gated** `POST /nodes` + `DELETE /nodes/{id}` — the HTTP path exists so an operator can publish from the CLIENT (`publish <project>` in its terminal) instead of copying a bundle onto the server box. A self-hosted server's operator curates their own store the same way (isolated per server). The old any-user `POST /nodes` + single-file `node.js` bytea format were removed by the `20260706130000` migration (drop + recreate; nothing was published in the old format).
- `apitokens/` — **long-lived, scoped, revocable credentials** (`scope.go`/`store.go`/`middleware.go`/`handler.go`, table `api_tokens`): how something that ISN'T a person talks to a server — a script, a mini-client, an agent. The session model in `auth` is built for a human at a client (15m access token, single-use rotating refresh); nothing there can be handed out narrowly or taken back. A token is `bmb_` + 32 random bytes, shown ONCE and stored only as a SHA-256 hash, so the server can check one and can never show one again. `Resolve` checks revocation and expiry **in the query** (a condition applied in Go is one somebody can forget) and stamps `last_used_at` in the same round trip. Scopes are deliberately few and coarse — `store:read|write`, `notes:read|write`, `friends:read`, `people:read`, `messages:read|write`, plus an implicit `profile:read` — because a long fine-grained list reads as careful and behaves as careless. `RequireScope` gates a route: a SESSION passes unscoped (it IS its owner), a TOKEN must hold the scope, and a miss is `403 insufficient_scope`, never 401. **A scope is a ceiling, not a grant** — `store:write` doesn't make a token an admin. `auth.RequireAuth` accepts either credential on the same header, branching on the prefix, via a `TokenResolver` seam wired in `main` so the dependency points one way (apitokens → auth); `auth.APITokenPrefix` is a duplicated constant a test pins to `apitokens.Prefix`. `GET|POST /me/tokens` + `DELETE /me/tokens/{id}` are **SessionOnly** — a token that could mint or revoke a token is privilege that extends itself, so there is no scope for it. `GET /token-scopes` is unauthenticated (a consent screen needs it before a token exists).
- `admin/` — the operator **ROLE** (`users.is_admin`) and the HTTP gate admin-only routes hang off (`RequireAdmin`, which runs after `RequireAuth`; non-admin → `403 forbidden`, and a DB failure fails CLOSED). Before this, "operator" meant "whoever holds the console" — privilege by physical access, which made every operator action unreachable from the client. Granting is deliberately console-or-config only (`admin <user>` at the prompt, or `ADMIN_USERNAME` at boot): there is no endpoint that grants admin, because an HTTP path to self-promotion is precisely what you don't want. Today it gates node-store and pack-store publishing; the rest of the console surface can ride the same gate later.
- `console/` — the **interactive admin console** the binary runs on stdin by default (Minecraft-style `bombers>` prompt; skip with `--headless`): a small command registry + loop — `help`, `users` (username/id/created), `status` (uptime, address, DB + media health), `logtime` (show/switch the live log timestamp format), the **node-store publish surface** (`publish <path>` — read + validate a `{manifest, files}` JSON file and upsert it into the store; `unpublish <id>`; `store` — list published nodes; see `examples/sample-store-node.json`), and `stop` (aliases `quit`/`exit`) → graceful `http.Server.Shutdown`. Local-operator-privileged by definition (whoever holds the terminal — no auth); the store commands are the deliberate step past read-only (the console IS the store's only publish path). **User administration** is here too: `ban <user> [reason]` / `unban` / `banned` (list) and `deluser` — resolving a user by username OR id. A ban is a mark (`users.banned_at` + `ban_reason`), not a deletion, so it's reversible and the user's content stays consistent for everyone else; it's enforced at the DOORS (login refuses, refresh refuses + their refresh tokens are cleared) rather than on every request, so it takes hold within one 15m access-token lifetime without adding a DB read to every authed call. `deluser` is irreversible — every cross-user table cascades — so it makes you type the username back, and REFUSES outright when stdin isn't interactive. The console is also the ROOT OF TRUST for the admin role (`internal/admin`): `admin <user>` grants it, `unadmin` revokes, `admins` lists. There is deliberately NO endpoint that grants admin — the only ways in are at the machine or via `ADMIN_USERNAME` at boot. A non-TTY stdin or console EOF falls back to headless signal-waiting (never spins). `main.go` serves via `http.Server` in a goroutine and shuts down gracefully in every mode. On launch `cmd/bombers` prints a colored **BOMBERS/NOTEBOOK** ASCII banner (TTY only, via `banner.go`) before the prompt; the old plaintext console intro line is gone — the banner plus a `console ready` INFO log replace it.
- `nodeshare/` — **friend node-sharing** (`handler.go`/`store.go`, table `node_transfers`): a lightweight inbox of node bundles sent friend-to-friend — the clone model, NOT the public node store above. A transfer is a **one-way copy**: `POST /nodes/send` (`{recipient_id, bundle}` — recipient must be an **accepted friend**, every not-allowed case collapses to an opaque `404 recipient_not_found`; 4 MiB cap), `GET /nodes/received` (my inbox, sender username joined in, newest first), `DELETE /nodes/received/{id}` (dismiss after handling — recipient only, opaque `404 transfer_not_found`). The `bundle` is the client's `{manifest, files}` node source stored as **opaque JSONB** (dumb-blob, like sync — the server never inspects it). No live link, no ownership; the recipient clones it into a project they own. Owns the same narrow `areFriends` query as `messaging`/`profiles`.
- `media/` — **profile media** (avatar + banner), the first slice of the S3 phase (`storage.go` minio-go S3 wrapper / `store.go` metadata + narrow gating queries / `handler.go`, table `user_media`). Bytes live in **S3-compatible object storage** (MinIO via docker-compose locally; MinIO-on-VPS or Cloudflare R2 in prod — plain S3 API only) at the fixed key `users/<user_id>/<kind>`; the `user_media` row holds sniffed content type, size, `updated_at`. `PUT /me/media/{avatar|banner}` takes **raw image bytes** (type sniffed server-side, only png/jpeg/gif/webp; caps 5 MiB avatar / 10 MiB banner; 413 `media_too_large` / 415 `unsupported_media_type`), one key per (user, kind) so **reupload replaces**; `DELETE /me/media/{kind}` clears (idempotent 204). `GET /media/{userID}/{kind}` is the **authenticated pass-through**: streams bytes through the server (never a bucket/presigned URL — deliberate, per PRODUCT.md), allowed for the owner always and an accepted friend while the owner's profile visibility is `friends` (no saved card defaults to friends); every not-allowed case collapses to opaque `404 media_not_found`. ETag/If-None-Match revalidation from `updated_at`. Startup creates the bucket and is fatal if the store is unreachable (mirrors the DB); `/health` gained an informational `media: up|down` field (DB still governs status). Owns narrow `areFriends` + `profileVisibility` queries; `types.MediaURL` is the shared URL shape (`/media/{id}/{kind}?v=<unix>`) so profiles emits identical URLs without importing media.
- `messaging/` — **direct messages**: text DMs between two users, persisted **indefinitely** in Postgres (DMs aren't ephemeral; rooms are). `POST /messages` (send to an accepted friend), `GET /messages/{userID}` (conversation history, oldest→newest, capped at the most recent `?limit=` — default 100, max 200). You may only message an accepted friend; every not-allowed case (non-friend, nonexistent, self) collapses to an opaque `404 recipient_not_found`. Owns the same narrow `areFriends` query as `profiles`. **Unread is server-authoritative:** `GET /messages/unread` returns the per-peer unread summary in one request (`{ conversations: [{ user_id, count }], total }`, empty list + `total` 0 when nothing is unread), and `POST /messages/{userID}/read` marks that conversation read up to now (server time) → `204`. Read state lives in a per-`(user, peer)` `message_reads` last-read marker advanced **monotonically** (`GREATEST`, never rewinds), so reading on one device clears the badge on all of them; a received message counts as unread until its `created_at` ≤ your marker for that sender. Marking read is a personal bookmark (not friend-gated); a missing/self `{userID}` is a `400`. **This is the durable REST layer only** — real-time delivery (WebSocket push, offline queue, "feels live" indicators) is deferred to the unbuilt `realtime/` domain; today a client loads history on open and appends what it sends. **Text only in v1** — image/file attachments wait for a later S3-phase slice (profile media shipped first; see `media/`).

- `rooms/` -- the **realtime relay** behind games and lobbies, the first WebSocket domain (`github.com/coder/websocket`). **The server never learns a game's rules**: it forwards opaque `{t, d}` frames between the members of a room and tracks presence, nothing more. **A room is not a game** -- it's a temporary space with a name, some people, and an activity id the HOST sets from inside it over the socket (`POST /rooms` names no activity and generates a name like `amber-lantern` if you don't). The creator is the host and **the host seat never transfers**: they rename, kick, choose the game and end it, and their leaving ends the room for everyone. A dropped socket is not a decision, so the host gets the same 2-minute grace as anyone and the reaper closes the room only if they don't return; `host:end` closes it at once. Two reserved namespaces in opposite directions: `room:` (server → client presence + facts: `welcome`/`join`/`leave`/`update`/`kicked`/`closed`/`error`) and `host:` (client → server host-only requests: `rename`/`game`/`kick`/`end`, answered and never relayed; anyone else gets `not_host`). Move validation, scores and a late joiner's state resync stay client concerns. Rooms are **in-memory and ephemeral** (DMs persist forever, rooms persist nothing): `GET /rooms/{roomID}/ws` joins one. That join sits OUTSIDE the `RequireAuth` group on purpose -- a webview can't put an Authorization header on a socket, so the access token rides the **`bearer` subprotocol** and the handler verifies it with the same issuer. Joining is friend-gated against the host (opaque `404 room_not_found` for every refusal). Per-member **token-bucket rate cap** (burst 120, 60/s -- sized so 30Hz position sync for a 3D activity never trips it) drops the message and tells the sender rather than closing the socket; a slow peer's frames are dropped instead of stalling the room; frames cap at 64 KiB; the `room:` type namespace is reserved so a client can't forge presence, and `from` is stamped server-side so it can't be spoofed. An empty room is reaped after a 2-minute grace (so a reconnect, or the gap between create and socket, survives). The hub is testable without a network via the `Sender` seam -- `hub_test.go` covers presence, host transfer, rejoin-replaces-connection, the rate cap (including a 30Hz stream), reaping, and the spoofing guards.

- `activities/` -- the **game store** (`handler.go`/`store.go`, tables `activities` + `activity_assets`): the games this server publishes for the client's Play screen. Mirrors the node store deliberately -- an opaque `{manifest, files}` bundle the server never interprets, operator-curated through the console, **no HTTP publish**. The difference is ASSETS: sprites/audio (later models) whose BYTES live in object storage under `activities/<id>/<path>` and whose manifest (path, content type, size) lives in `activity_assets`. `GET /activities` is the catalogue (id/name/version + description/category/players read out of each manifest, plus asset count and total download size -- never the files), `GET /activities/{id}/bundle` returns the bundle spliced in byte-for-byte alongside its asset manifest so an installer needs one round trip, and `GET /activities/{id}/assets/*` streams a single asset **through the server** (never a bucket/presigned URL, same rule as profile media). Asset paths are validated by a narrow positive rule (`ValidAssetPath`: forward slashes, no leading slash, no dot segments, bounded) because a traversal through a published bundle would be the worst kind of bug -- tested. Console: `games` (list), `publish-game <folder>` (reads `manifest.json` + source + `assets/**`, uploads the bytes, records the manifest; republishing REPLACES the asset folder so a removed file stops being served), `unpublish-game <id>` (drops the row -- assets cascade -- and removes the stored bytes). `GET /activities` also surfaces `icon`/`cover` -- asset PATHS read out of the manifest, and only when the game really shipped that file, so a client can show art before installing without chasing 404s.

**Publishing is admin-only over HTTP as well as from the console**, in two steps like the pack store because a game carries binary assets: `POST /activities` (the bundle), `PUT /activities/{id}/assets/*` (one raw-bytes PUT per file), `DELETE /activities/{id}`. Publishing REPLACES -- same id, new bundle, and the asset folder starts over, so art must be re-uploaded after every publish. A `version` is required and is checked here as well as at compile: two players must be on matching builds, and a mismatch produces a desync mid-match rather than an error anyone can read.

`examples/sample-game/` is a working folder to copy and `examples/word-sprint/` is a real game. Connect Four and Chess live in `bombers/games/` -- outside both repos on purpose, like the launcher scripts. The client ships with **zero** games; every one of them installs from this store.

- `packs/` -- the **pack store** (`handler.go`/`store.go`, tables `packs` + `pack_assets`): downloadable look-and-feel bundles for the client -- a theme (colors/fonts/roundness as CSS variables), a set of sounds, an optional wallpaper, or any mix. A "theme with its own sounds" is just a pack carrying both. Same shape as the activity store (bundle + assets, operator-curated); `bundle` is `pack.json` (theme vars live inside it, stored verbatim), and the sounds/wallpaper BYTES live in object storage under `packs/<id>/<path>`. Reading, open to any authed user: `GET /packs`, `GET /packs/{id}/bundle`, `GET /packs/{id}/assets/*`. Publishing is **operator-only, two ways into the same operation**: the console (`packs` list, `publish-pack <folder>` -- `pack.json` + `sounds/**` + a top-level `wallpaper.*`, everything else skipped -- `unpublish-pack <id>`), or the **admin-gated** HTTP path, so an operator can publish from the CLIENT instead of copying a folder onto the server box. That path takes TWO steps where the node store took one, because a pack carries binary assets: `POST /packs` (the body IS `pack.json`; upserts the record and CLEARS the previous asset set, rows and stored bytes both, so a republish replaces rather than accumulates), then one `PUT /packs/{id}/assets/*` per file (raw bytes, `Content-Type` from the header, `packs.UpsertAsset` records the single row), and `DELETE /packs/{id}` to unpublish (idempotent -- `removed: false`, never a 404, matching the node store). Caps: `packs.BundleLimit` 4 MiB (413 `bundle_too_large`), `packs.AssetLimit` 8 MiB (413 `asset_too_large`) -- both shared with the console path so the two can't disagree. `ValidAssetPath` **and** `ValidPackID` are checked before anything touches storage: between them they are the whole of the object key, and a traversal through a published pack would be the worst kind of bug (tested). `examples/sample-pack/` is a working folder to copy.

- `releases/` — **the app's own updates** (`handler.go`/`store.go`, table `releases`): the desktop client's installers, read by the updater inside a running copy rather than by a person browsing. The fourth store and the smallest — one file and a signature where the others carry a bundle plus assets. `signature` is NOT NULL with no default, because every installed copy verifies a download against the public key compiled into it and a row without one is a release nobody can take. The installer's BYTES live in object storage under `releases/<version>/<file>` and stream **through** the server (never a bucket URL, same rule as media). `GET /releases/latest` emits the **Tauri updater's** manifest shape verbatim — field names and the RFC 3339 `pub_date` are that plugin's contract, not ours — and answers `204` both when the caller is current and when nothing is published, deliberately not distinguishing them. **"Latest" is the most recently PUBLISHED row, not the highest version**, which matches the client's store rule ("different from what's published") and makes republishing an older build the rollback mechanism; a row whose bytes never arrived is skipped, so a half-finished publish keeps serving the previous release instead of offering a download that 404s. The manifest's `url` is derived from the incoming request (honouring `X-Forwarded-Proto`/`-Host`) rather than from configuration, so a LAN server hands out LAN URLs with nothing set. Publishing is **admin-gated HTTP only** — `POST /releases` (metadata + signature; resets the version's size to zero and clears its stored bytes so a republish can't serve the old installer under a new signature), `PUT /releases/{version}/artifact` (raw bytes, 128 MiB cap; the filename comes from step one so the two steps can't disagree), `DELETE /releases/{version}` (idempotent). **A publish PRUNES**: only the newest `KeepReleases` (3) survive, rows and stored bytes both — nothing pointed at the older ones and their installers sat in object storage forever. Three rather than one because `unpublish-release` is the fast rollback and it drops clients onto the newest release STILL published; delete aggressively and you delete the undo. Pruning runs after the new release lands and is never fatal — the publish already succeeded. There is deliberately no `publish-release` console command: the build happens on the operator's Windows desktop and this server is Linux, so a console publish would mean copying the installer onto the box first — the exact step the HTTP path removes. The console keeps `releases` (list, `→` marks the one being offered, `(no file yet)` marks an interrupted publish) and `unpublish-release <version>`.

**Routes (wired in `cmd/bombers/main.go`):** `GET /health`, `POST /auth/{register,login,refresh}` (register/login live on the `users` handler), and an auth-gated group (`issuer.RequireAuth`): `GET /me`, `GET /friends`, `GET /friends/code`, `GET|POST /friends/requests`, `POST /friends/requests/{requesterID}/{accept,reject}`, `DELETE /friends/{userID}`, `POST /friends/{userID}/{block,unblock}`, `GET|PUT /me/profile`, `GET /profiles/{userID}`, `PUT /me/card`, `GET /cards/{ownerID}`, `PUT /me/presence`, `GET /presence`, `GET /me/about`, `GET|PUT|DELETE /me/about/{subjectID}`, `GET /about/{authorID}`, `POST /messages`, `GET /messages/unread`, `GET /messages/{userID}`, `POST /messages/{userID}/read`, `POST /sync/push`, `GET /sync/pull`, `GET /sync/status`, `GET /nodes`, `GET /nodes/{id}/bundle`, `POST /nodes` (admin), `DELETE /nodes/{id}` (admin), `POST /nodes/send`, `GET /nodes/received`, `DELETE /nodes/received/{id}`, `PUT|DELETE /me/media/{kind}`, `GET /media/{userID}/{kind}`, `POST /rooms`, `GET /activities`, `GET /activities/{id}/bundle`, `GET /activities/{id}/assets/*`, `GET /packs`, `GET /packs/{id}/bundle`, `GET /packs/{id}/assets/*`, `POST /packs` (admin), `PUT /packs/{id}/assets/*` (admin), `DELETE /packs/{id}` (admin), `GET /releases/latest`, `GET /releases/{version}/download`, `POST /releases` (admin), `PUT /releases/{version}/artifact` (admin), `DELETE /releases/{version}` (admin) — plus the unauthenticated-group `GET /rooms/{roomID}/ws` (subprotocol-authenticated WebSocket). Store publishing (`POST /nodes`, `DELETE /nodes/{id}`, `POST /packs`, `PUT /packs/{id}/assets/*`, `DELETE /packs/{id}`) is nested inside the auth group behind `admin.RequireAdmin`, one `r.Group` per store.

**Migrations (`migrations/`):** `users`, `refresh_tokens`, `friendships`, `profiles`, `messages`, `about_cards`, `published_items` (+ `sync_state`), `nodes` (created `20260625`, reshaped to SDK `{manifest, files}` JSONB bundles by `20260706130000_rework_nodes_store`), `node_transfers`, `user_media`, `message_reads`, `profile_shares` (+ the `nickname`/`city`/`notes` profile columns; the table and `notes` were later dropped by `create_published_cards`), `add_user_bans` (`users.banned_at`/`ban_reason`), `create_activities` (+ `activity_assets`), `create_packs` (+ `pack_assets`), `add_user_admin` (`users.is_admin` + a partial index), `create_api_tokens`, `discord_auth`, `signin_attempts`, `settings`, `create_releases`, `create_published_cards`, `create_presence`.

**Not yet built (still spec in `SERVER.md`):** `sharing/`, `events/`. `rooms/` now exists as the WebSocket relay for activities (see above); a general `realtime/` layer (DM push, sync nudges) can reuse the same dependency and pattern. Published-content storage now exists (`sync/`); sharing and any real-time layer do not yet. The next domains follow the same package-per-domain shape. (Profiles now covers both self-card and about-card; *specific-friend* note sharing now exists as the per-viewer published card; what remains is sharing an about-card with friends other than its subject. Messaging is REST persistence only — its real-time delivery lands with `realtime/`.)

## What this repo is

The **Bombers Server** — the Go backend half of Bombers. The Tauri/React client lives in a **separate codebase**; this repo is server-only. The API is the product, and the desktop client is just one consumer — never bake client-specific assumptions into endpoints.

Scope is the owner and his friends (tens of users, permanently). Don't over-engineer for scale or federation.

**It runs on Linux.** Developed on a Windows desktop, deployed to an Arch laptop and an Ubuntu VPS — that's the whole world. Go will cross-compile elsewhere; nothing there is tested and the install path assumes a Unix filesystem. Don't add platform-specific code for Windows or macOS.

**Self-hosting works but isn't a goal** (2026-08-06). The `bombers` CLI, the setup wizard and the server picker all exist and function — the owner uses them to run his own instance — but making that good *for strangers* stopped being something to invest in. Keep the capability, don't build an audience for it. Every server (official or private) is an **isolated island**: no cross-server anything (per `PRODUCT.md`).

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

Built today: `cmd/bombers/` (wiring) + `internal/{config,store,httpx,logx,types,auth,admin,apitokens,users,discord,settings,friends,profiles,messaging,sync,nodes,nodeshare,packs,activities,releases,presence,rooms,media,console,setup,embeddedpg,migrate,svc}` (`svc` wraps `kardianos/service` for the P5 background-service mode). Planned per `SERVER.md`: `internal/{sharing,events,rooms,realtime}`.

Each `internal/<domain>` owns its own routes, logic, and queries (typical files: `handler.go` HTTP, `service.go` logic, `store.go` pgx queries). Domains stay loosely coupled — don't reach into another domain's internals; depend on `auth` for tokens/middleware and `httpx`/`types`/`store` for shared plumbing. New routes are registered in `cmd/bombers/main.go` (auth-gated ones inside the `RequireAuth` group).

## Mental model that drives everything

- **Local client = working copy. Server = published copy.** The server never edits; it stores what clients have published, and is the substrate for sharing. Don't invert this.
- The server is a **versioned key-value store of published items keyed by ULID, scoped to an account, plus a sharing/permission layer.** Keep this framing when designing sync endpoints.
- **Sync is triggered, not real-time.** Push/pull are explicit operations. The "feels live" layer is a WebSocket nudge → client auto-pulls (except the note the user is actively editing). See `SERVER.md` §Sync.
- **DMs persist forever in Postgres. Rooms persist nothing past expiry.** This asymmetry is intentional.
- **Media ships as slices of one S3 phase — profile media (avatar/banner) and ACTIVITY ASSETS are built; nothing else is.** Photo libraries, DM attachments, note-attachment sync, and room files wait for their own slices on the same `media/` plumbing. Don't build them piecemeal, and never serve media via direct/presigned bucket URLs — authenticated pass-through only.

## Commands

Standard Go workflow (no `Makefile`):

The binary is a `bombers <command>` CLI. **Running a server is five commands and
never mentions Go or goose:**

```bash
./bombers install    # once, from a fresh clone: builds + puts `bombers` on your PATH
bombers setup        # configure database + media, and migrate. Re-run to change config
bombers start        # run in the background
bombers restart      # stop + start; the command to use after `bombers update`
bombers              # the admin console against it (exiting leaves it running)
bombers stop         # stop it
bombers backup       # everything in one portable file
bombers restore FILE # put it back (REPLACES what's there)
```

`./bombers` in the repo root is a **launcher script**: it compiles the server if
the binary is missing or stale, then execs it — which is why a fresh clone needs
no `go build`. After `install`, it's out of the picture.

- **`install`** — builds from the checkout, installs to `/usr/local/bin` when
  that's writable (a container running as root) else `~/.local/bin` (never sudo;
  prints the PATH line if that directory isn't on it), and records the source +
  binary paths in `<dataDir>/install.json` so `update` works from any directory.
  It configures nothing and touches no database — that's `setup`'s job, kept
  separate so reconfiguring never means reinstalling.
- **`setup`** — the config wizard, then migrates (it now knows where the database
  is). This is the only migration a first install needs.
- **`update`** — pulls the recorded source, rebuilds from it, replaces the
  installed binary, then applies migrations. It PULLS so that "I updated and
  nothing changed" stops being a normal experience, and REFUSES on a dirty tree
  rather than stashing: a binary built from code that exists nowhere else is one
  nobody can reproduce. Not a git checkout, or no git installed → it says so and
  builds what's there.
  That handoff is required, not decoration: migrations are embedded in the
  binary, so the running copy has no idea a new one exists.
- **`start`** backgrounds itself at a terminal (pidfile + `server.log` in the data
  dir; `stop` / `status` / `logs` manage it) and stays in the FOREGROUND when
  stdout isn't a terminal, which is what makes it correct under systemd and in a
  container. `start --foreground` forces the attached path.
- **A bare `bombers`** opens the admin console. A service-manager launch passes no
  arguments either, so that case is detected BEFORE the bare default is applied —
  otherwise systemd would start a console instead of a server.
- `service` (systemd/launchd/Windows Service) remains for start-on-boot and
  restart-on-failure; it is no longer needed just to run in the background.
  **`update` REFUSES while the server is running** — it checks both the pidfile
  and the embedded Postgres port, because a service-launched server never
  backgrounds itself and so writes no pidfile. Without that check, `update`'s
  stale-port cleanup reads a LIVE database as a crash leftover and stops it,
  killing the database out from under the running server.
  A service whose **startup fails now EXITS non-zero** instead of asking the
  service manager to stop it: `s.Stop()` under systemd means `systemctl stop`,
  which the service's own unprivileged user may not do — so the call failed, the
  error was discarded, and the process sat there alive serving nothing while
  systemd reported it running. The unit carries `Restart=on-failure` (not
  `always`, so a deliberate stop stays stopped) with a 10s delay, which lets a
  transient cause like a port still held by something shutting down clear itself.
  **`service install` sets the unit's user** (`svc.Config(user)` → `User=`),
  taken from `--user` or `SUDO_USER`, and **REFUSES to install a root-run
  service**. Installing needs root, so without this systemd would run the server
  as root — where the embedded Postgres refuses to start (`initdb: cannot be run
  as root`) and the data dir resolves to root's home instead of the account
  holding it. The failure would be invisible until the first reboot, which is
  why it's a refusal rather than a warning.

- **`backup` / `restore`** — one portable file, and portable is the point: a
  backup that only restores onto an identical setup isn't a backup. The archive
  holds a plain `pg_dump` plus the media as ordinary files, so it goes back into
  whatever the NEW machine runs — laptop's embedded Postgres to a VPS's system
  one, filesystem media to MinIO. `pg_dump`/`psql` are borrowed from the
  embedded distribution when there is one (`<dataDir>/pg/runtime/bin`) and from
  PATH otherwise; shelling out rather than serialising rows here is a deliberate
  refusal to be clever, since a hand-rolled dumper that gets one escaping rule
  wrong produces an archive that looks fine and restores wrong. `restore`
  REPLACES everything, makes you type RESTORE, and refuses outright when stdin
  isn't a terminal (the `deluser` rule).

Underneath, the plumbing commands still exist: `migrate`, `doctor`, `console`,
`version`, `uninstall`. `migrate` works on BOTH backends — with embedded
Postgres it starts the database, migrates, and stops it again, since nothing is
listening between server runs (refusing in that case made `update` impossible on
a self-hosted install).

```powershell
go build ./...                          # compile everything (binary: bombers)
go run ./cmd/bombers                    # run the server (needs DATABASE_URL + TOKEN_SECRET);
                                        # bare = `start`, opens the interactive `bombers>` console
go run ./cmd/bombers start --headless   # daemon mode: no console, stops on SIGINT/SIGTERM
                                        # (bare `go run ./cmd/bombers --headless` still works)
go run ./cmd/bombers setup              # (re)configure local self-host, then exit (does not serve)
go run ./cmd/bombers doctor             # check the local setup for problems (never serves); exits 1 on any failure
go run ./cmd/bombers console            # open the admin console against a running (headless/service) server — same DB, separate process
go run ./cmd/bombers service install    # register as an OS background service (needs an ADMIN/root shell)
go run ./cmd/bombers service start      # also: stop / restart / status / uninstall the service
go run ./cmd/bombers uninstall          # remove the service + delete the data dir (--yes skips the confirm)
go run ./cmd/bombers version            # print the version
go run ./cmd/bombers help               # list the subcommands
go test ./...                           # run all tests
go test ./internal/auth -run TestRotate   # single package / test
go vet ./...
gofmt -w .
```

A local Postgres + MinIO are available via `docker compose up -d` (Postgres uses `DB_NAME`/`DB_USER`/`DB_PASSWORD`, binds 5432; MinIO uses `S3_ACCESS_KEY`/`S3_SECRET_KEY`, binds 9000 + console 9001 — the server creates the media bucket itself on startup). A `server.exe` may be sitting in the repo root on the Windows dev machine: it's an untracked leftover, not a committed artifact (`.gitignore` excludes `*.exe`) — ignore it and rebuild with `go build`.

### Container hosts (Railway, Fly, plain Docker)

`install` / `setup` / `update` / the admin console are the SELF-HOSTED path and
never run on a platform host: there is no PATH to install into, no wizard to
answer, and no git pull to react to. A platform builds the binary from the repo
and runs it; configuration is env vars.

Two behaviours make `bombers start` the correct start command there with nothing
special configured:

- **It stays in the foreground when stdout isn't a terminal.** Backgrounding is
  for a human shell. In a container, forking away looks exactly like the process
  exiting — the platform would call the deploy dead and restart forever. The same
  check keeps a systemd-launched process in the foreground.
- **`AUTO_MIGRATE=true` applies pending migrations at startup**, so a deploy
  carries its own schema. Opt-in on purpose: on a managed database, schema
  changes shouldn't be a side effect of a restart. Leave it unset and use a
  a pre-deploy `bombers update` instead if you'd rather a failed migration block
  the deploy. (The embedded backend always migrates itself and ignores this.)

`PORT` is read from the environment (platforms inject it), `/health` is the
health-check path, and SIGTERM triggers the graceful shutdown a redeploy needs.

### Migrations (goose)

Migrations live in `migrations/` at the repo root, written as `-- +goose Up` / `-- +goose Down` SQL files.

**The easy path — `bombers update`.** The binary embeds the migrations and applies them through the goose LIBRARY (the same path embedded Postgres uses at startup), so updating any database — external/dockerised included — needs no goose CLI, no exported `DATABASE_URL`, and no `-dir` flag. It reads `.env` exactly like the server, applies what's pending, and exits; an unreachable database reports one clean line instead of pgx's address dump. Deliberately NOT run by `start`: applying schema changes should be something you asked for. There is no separate `migrate` COMMAND — migrating is plumbing, so `update` (after a pull) and `setup` (first install) are the two things that do it. `bombers update` is the after-a-pull command: migrate, then serve (`--no-start` when a systemd service owns the process). It must be the FRESHLY BUILT binary — migrations are embedded, so the old binary would apply the old set and report success. The Windows workspace's `./server.sh update` chains containers → build → migrate → start for the same routine.

The goose CLI remains available for the things a library call can't do (`down`, `status`, `create`):

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
- No media beyond the built profile avatar/banner slice (no photo libraries, DM attachments, or room files yet), and no direct/presigned bucket URLs — media serves through the server only.
- No OS push infrastructure — in-app indicators only.
- No persisting rooms past expiry.
- No baking Tauri-client assumptions into the API.
- No making the server authoritative over the working copy.
