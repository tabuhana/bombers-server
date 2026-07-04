# Bombers Server — Spec

The **Bombers Server** is a Go backend, a **separate codebase** from the Tauri client. It's the social/backbone half of Bombers. Read `PRODUCT.md` first — this spec must stay consistent with it.

This document defines the server's *shape, contracts, and decisions*. Detailed endpoint-by-endpoint and schema design will be planned interactively before implementation — this is the architectural frame, not the final blueprint.

## What the server is responsible for

- **Accounts & auth** — registration, login, sessions/tokens.
- **Friends** — friend codes, friend requests, the friend graph.
- **Published content** — server-side copies of users' published notes (and later mood/habits/events), used for cross-device restore and as the substrate for sharing.
- **Node store** — the catalog of installable **nodes** (the deck's buildable units): list metadata, serve a node's `node.js` bundle, and accept publishes. Built; see "Node store" below.
- **Sharing & permissions** — who can see what; granting/revoking friend access to specific published content.
- **Profiles** — the two-sided profile-card system (facts you keep about friends; facts they publish about themselves with visibility rules).
- **Messaging** — real-time DMs.
- **Rooms** — ephemeral real-time spaces, expiring when empty. v1: chat + presence. Deliberately open-ended for later (files, shared editing, games, etc.).
- **Notification fan-out** — feeding the client's in-app ambient indicators (no OS push).

*(Photos/memories and any other heavy media are NOT a v1 server responsibility — they're deferred to a single later S3 / blob-storage phase; see the decisions sections. There is no photo pass-through.)*

## What the server is NOT

- Not authoritative over a user's *working* copy. Local client is the editing source of truth; the server holds the *published* copy.
- Not a federation hub. One official server; private self-hosted servers are isolated and unsupported (see `PRODUCT.md`).
- Not a media/blob host **in v1** — no photo pass-through, no image storage of any kind. All heavy media (photo libraries, DM image attachments, room files) waits for a single later **S3 / blob-storage phase**. Until then the server handles text and metadata only.
- Not coupled to the Tauri client specifically. The API is the product; the client is one consumer. Other (out-of-scope) consumers may exist, so keep the API clean and client-agnostic.
- Not built for massive scale. Tens of users. Don't over-engineer.

## Tech stack (proposed — confirm during planning)

- **Language:** Go.
- **HTTP:** standard library `net/http` with a light router (`chi` is a good fit — idiomatic, minimal), or `net/http` alone if we keep routes simple.
- **Real-time:** WebSocket (`nhooyr.io/websocket` or `gorilla/websocket`) for messages, rooms, presence, live indicators.
- **DB:** PostgreSQL (accounts, friends, permissions, published-content metadata, profile cards). Access via `sqlc` (typed queries from SQL) or `pgx` directly. Avoid heavy ORMs.
- **Content storage:** published **note bodies live in Postgres** (decided). Notes are small text (a long note is ~5–10 KB); Postgres offloads large text values out-of-line automatically (TOAST), so big bodies don't bloat the main table. At tens of users this is a tiny database that fits in RAM on a cheap VPS — there's no scale at which note-text-in-Postgres becomes a problem for this product. The text/blob dividing line is what goes to object storage: **heavy blobs (photos, DM image attachments, note image-attachments, room files) are the S3-phase case, not notes.** The `PublishedItem` model keeps metadata separable from body ("content or pointer"), so if a body ever needed to move to object storage it's a mechanical change — but it won't.
- **Auth:** token-based (see "Auth model" below).
- **Migrations:** `goose` or `golang-migrate`.
- **Config:** environment variables, twelve-factor style.

These are recommendations to ratify when we plan, not locked.

## Core concepts and data model (sketch)

To be fleshed out during planning. The major entities:

- **User** — id, handle/display name, friend code (stable, shareable), auth credentials, created-at.
- **Session/Token** — auth artifact tying a device to a user.
- **Friendship** — a bidirectional link between two users, established via friend code + acceptance. State: pending / accepted / blocked.
- **PublishedItem** — a server-side copy of a published client item. Keyed by the client's ULID. Has: owner, type (note for now; later mood/habit/event), content (or pointer), version/updated-at, and a soft-delete flag. The client's ULID is the join key so the same item is recognizable across devices and for sharing.
- **Share** — a grant: (publishedItem, granteeUser, permission=read). Revocable. Drives "friend views your live copy."
- **ProfileCard** — structured **base fields** (name, birthday, country, time zone) plus **freeform text** the user adds on top. **Age is derived from birthday, never stored** (a stored age drifts out of date the day after a birthday). Two kinds, different owners:
  - *About-card*: authored by user A about user B, private to A by default, optionally shared.
  - *Self-card*: authored by user A about themselves, with a visibility rule (all friends / specific friends).
- **Event** — an event/date with an owner and invitees; invitee acceptance copies it to the invitee's calendar.
- **Message** — a DM between two users. **Text persisted indefinitely in Postgres** (DMs aren't ephemeral; rooms are). Small relational rows — forever-retention costs essentially nothing at this scale, and any device that authenticates pulls history from the DB, so this is already a true cross-device sync with no extra infrastructure. **Image/file attachments are deferred to the S3 phase** (no image messaging in v1) and, when added, will live in **S3 and persist in history just like text** — we don't want gaps in conversation context.
- **Room** — ephemeral. Exists while occupied; expires when empty. Membership + transient state, not persisted past expiry.

Note the asymmetry: **DMs persist, rooms don't.** DM history is retained **indefinitely** (decided) — it's cheap relational text.

## Auth model (decided)

- **Credentials: unique username + password.** The username is the *login identity* and must be unique. It is **distinct from the friend code** — the friend code is the shareable add-me token (per `PRODUCT.md`, no public username search/discovery; uniqueness for login is fine, but never expose a "look up user by username" endpoint).
- Registration creates a user + a friend code.
- Login returns a short-lived **access token** + a long-lived **refresh token**. Standard lifetimes (access ~15 min; refresh long).
- **The desktop client should effectively never log the user out.** Achieved by **rotating the refresh token on use**: as long as the app opens periodically it silently refreshes and the user never sees a login screen again after first sign-in. "Never logs out" without an immortal token.
- **Other (web / out-of-scope) consumers get a real expiration.** Same auth system; the client *kind* decides how aggressively to retain tokens (e.g. shorter-lived or non-rotating refresh for browser sessions). Token issuance can be tagged with a client kind — design when those consumers actually exist.
- Tokens stored in **secure OS storage** (keychain via Tauri), never localStorage.
- Access token on every HTTP request (`Authorization: Bearer`) and on WebSocket connect.
- **Google / Discord OAuth — anticipated but deferred.** Not in v1. Build username+password first; keep the auth design from painting us into a corner so a third-party identity path can be added later without rework.

## Sync / publish contract (the heart of client↔server)

This is the most important contract to get right. High-level shape:

- **Push (publish):** client sends its published items (those without `publish:false`) — id (ULID), content, `updated` timestamp, type. Server upserts by id, keeping the latest by `updated`.
- **Pull (restore / refresh):** client asks for everything in its account (optionally "changed since T"). Server returns items; client writes them into the local vault. Used for new-machine restore and load-after-a-while refresh.
- **Triggered, not streamed:** sync is a request/response operation the client initiates. Not a live mirror. Server records and returns a per-account "last synced" so the client can show "last synced X ago."
- **Push triggers (when a push fires):** on explicit "Sync now", on app-load-after-a-while, and — **debounced-autosync** — after the user pauses editing for a few seconds (the same debounce the client already uses for saves/revisions). This is a *trigger*, not a new sync mode: push is push, and the server doesn't care why one fired. It makes the push side feel continuous (Google-Drive-ish) and keeps "last synced" fresh, but **it does not change the pull model** — the *other* device still only sees changes on its next pull. Bombers is still triggered sync, not live cross-device mirroring (per `PRODUCT.md`). A side benefit: more frequent pushes mean smaller divergence windows, so conflicts get *less* likely, not more.
- **Conflicts:** last-write-wins by `updated` for v1, but the server should detect divergence (same id, both sides changed since last common sync) and return a conflict marker so the client can warn the user rather than silently clobber. Exact mechanism is a planning topic.
- **Nudge-based auto-pull (the "feels live" layer):** the server sends a lightweight **"item X changed" nudge** down the client's existing WebSocket (the same socket used for messages/presence/indicators — no new infrastructure). On receiving a nudge, the client **auto-pulls that item in the background** so other devices stay fresh within ~a second of a push, without the user manually syncing. **Critical safety rule: the client must NOT auto-pull a note the user is currently editing** (a note open/focused in a pane). Background notes refresh silently; the actively-edited note is left alone and only reconciled through the normal conflict path. This deliberately stops short of full live mirroring — it gives the *feel* of cross-device liveness for everything you're not touching, while never clobbering in-progress edits out from under the user. Last-write-wins stays safe precisely because the one note that could collide is excluded from auto-pull. (Auto-pull is still a *pull*; the pull model is unchanged, just triggered by a nudge instead of a manual action.)
- **Deletes:** soft-delete propagation — a note trashed on the client should mark the published item deleted on the server (so it doesn't resurrect on the next pull to another device).

A clean way to frame it: the server is a **versioned key-value store of published items, keyed by ULID, scoped to an account,** plus a sharing/permission layer on top. Keep that framing in mind.

## Sharing contract

- A user grants a friend `read` on a specific published item → creates a Share.
- The friend's client, on sync, receives items shared *with* them (read-only), alongside their own.
- Friend sees the owner's current server copy; updates appear on the friend's next sync (as-of-last-sync "live").
- Revoking deletes the Share; the item no longer appears in the friend's pulls.
- **Save-a-copy:** an explicit client action where the friend duplicates a shared item into their own vault as a new item (new ULID, owned by them, published to their account). Server just sees a normal new item from that user.
- **No photo/media sharing in v1.** Photo libraries and any image sharing wait for the S3 phase (see below). The sharing contract above covers text/metadata items only for now.
- **Note attachments are local, not server-synced yet.** Pasting an image into a note stores it as a file in the vault's `attachments/` folder and links to it (client notes concern — works offline, on disk, today). When such a note is published, only its **text** rides to the server in v1; syncing the referenced **image bytes** across devices is part of the S3 phase. So the sync contract does not pretend to move image binaries before then — a published note may reference an attachment that other devices can't yet pull.

## Real-time contract (messages, rooms, presence, indicators)

Over WebSocket:

- **Messages:** deliver DMs in real time to online recipients; queue for delivery on next connect if offline. Persisted server-side.
- **Rooms:** join/leave (and **rejoin while the room is still alive**), presence, and in-room chat. **Ephemeral** — short-lived spaces that exist while occupied and tear down when the last member leaves; nothing persists past expiry. **v1 scope: messaging in a room, and that's it.** But rooms are a **deliberately open-ended surface** — much more is anticipated (in-room file sharing, file viewing, possibly shared/collaborative editing, games, etc.). **None of it is set in stone; the point is to not foreclose it.** Build the room lifecycle and chat now; design the rest when each piece is actually picked up.
- **Games (deferred, design undecided):** one of the future room capabilities above, called out separately because it's the one we've discussed. Games run inside a room but are **not specified here**. What's known so far: games are **small** (text-based, or simple things like Connect 4 / Battleship with minimal graphics), distributed as **bundled game files the client downloads and runs locally**, and they will involve **some server-side component** (so the server is *not* assumed to be a pure relay — relay-opaque-state vs. validate-turns vs. hold-authority is an open question). Don't build or pre-decide this now.
- **Presence:** which friends are online (for the in-app indicators / room invites).
- **Indicators:** server pushes lightweight "you have something new" signals (a new message, an event invite, a friend's shared update) so the client can show ambient badges. No OS notifications — the client decides how to surface them, quietly.

## Node store (catalog / bundle / publish) — built

Nodes are the installable, user-buildable unit on the client (the deck items). The server hosts the **node store** so a client can browse, download, and publish them. Implemented in `internal/nodes` (`handler.go` / `store.go`), table `nodes`.

- **A published node = metadata + a bundle.** Metadata is small catalog fields (id, name, version, description, author, width, height, can_rotate, permissions, hash). The bundle is the `node.js` ESM (a node factory) the client dynamic-imports and runs.
- **Endpoints** (all under the authed group):
  - `GET /nodes` — catalog metadata for every published node (no bundle bytes), ordered by name.
  - `GET /nodes/{id}/bundle` — the `node.js` bytes; integrity hash in the `ETag` header. The client re-hashes on install and refuses a mismatch.
  - `POST /nodes` — publish (or replace) a node + bundle. Body: the metadata fields + base64 `bundle`. The server computes the bundle's sha256, **rejects a supplied `hash` that doesn't match**, and stores the computed hash. Body cap 4 MiB.
- **Permissions** are stored as the client sends them. The client auto-detects them from the node's `host.*` usage (a static scan in the builder); the server **re-deriving/validating** them on publish is a planned hardening — today it trusts the client list.
- **Curated, not open.** Publishing requires auth; **owner-only gating is a follow-up** (any authenticated user can publish today). This matches the "curated now, community later" trust model — untrusted-code publishing is the future case that needs a real review/sandbox gate.
- **Bundle storage is a deliberate carve-out from "no media/blob in v1."** A node bundle is a tiny JS file (kilobytes) stored as Postgres `bytea` — the same "small text/code in Postgres" reasoning as note bodies. This is **not** the heavy-media (photos, attachments, room files) the S3 phase covers; the no-blob-in-v1 rule is about user media, not code bundles.

## API surface (to be detailed during planning)

The contract will be split into HTTP (request/response) and WebSocket (real-time). Rough groupings:

**HTTP**
- Auth: register, login, refresh, logout.
- Friends: get my friend code, send friend request (by code), list/accept/reject requests, list friends, remove/block.
- Sync: push published items, pull items (mine + shared-with-me), report last-synced.
- Nodes: list catalog, download a node bundle, publish a node. **(Built — see "Node store".)**
- Sharing: grant share, revoke share, list shares.
- Profiles: CRUD about-cards, CRUD self-cards + visibility, fetch a friend's profile (resolves what they've made visible to me).
- Events: create, invite, accept/decline, list my calendar.

*(No Photos/media endpoints in v1 — added in the S3 phase.)*

**WebSocket**
- Connect (authed), presence, DM send/receive, room create/join/leave/state, indicator pushes.

Exact paths, payloads, and schemas are the next planning task — don't invent them yet.

## Project structure (proposed)

```
bombers-server/
├── cmd/
│   └── server/
│       └── main.go            wiring, config, start
├── internal/
│   ├── auth/                  tokens, sessions, middleware
│   ├── users/                 accounts, friend codes
│   ├── friends/               friend graph, requests
│   ├── sync/                  publish/pull, versioning, conflicts
│   ├── nodes/                 node store: catalog, bundle download, publish
│   ├── sharing/               shares, permission checks
│   ├── profiles/              about-cards + self-cards + visibility
│   ├── events/                events + invites
│   ├── messaging/             DM persistence + delivery
│   ├── rooms/                 ephemeral room lifecycle
│   ├── realtime/              WebSocket hub, presence, indicators
│   └── store/                 Postgres access (sqlc/pgx), migrations
├── migrations/
├── go.mod
└── README.md
```

Each `internal/<domain>` package owns its own routes, logic, and queries — kept loosely coupled so domains don't reach into each other's internals (mirrors the client's module discipline).

## Decisions resolved (this planning round)

These are settled — build to them, don't re-litigate:

1. **Auth:** unique username + password for v1. Short access token (~15 min) + long **rotating** refresh token. Desktop effectively never logs out (rotation); web/other consumers get real expiration. Tokens in secure OS storage. Google/Discord OAuth anticipated but deferred. (See "Auth model".)
2. **Published content storage:** note bodies in **Postgres**. Heavy blobs are NOT a v1 concern — they wait for the S3 phase. (See "Tech stack".)
3. **DM retention:** **forever**, in Postgres (text). Image attachments deferred to the S3 phase and will persist like text.
4. **Sync trigger:** debounced-autosync (push after an editing pause) is a trigger, not a new mode.
5. **Cross-device auto-update:** server nudges over the existing WebSocket; client **auto-pulls in the background**, EXCEPT the note the user is actively editing (never auto-pulled, to keep last-write-wins safe). Feels live without full mirroring. (See "Sync / publish contract".)
6. **Profile-card shape:** structured base fields (name, birthday, country, time zone; **age derived from birthday, not stored**) + freeform text. (See data model.)
7. **No v1 media:** photo pass-through dropped entirely. All heavy media (photos, DM images, room files) deferred to a single **S3 / blob-storage phase**. Note image-paste is local-vault-only until then.
8. **Rate limiting:** standard **floor** — enough to keep the server from being knocked over (token-bucket on auth endpoints to stop brute-force; looser limits elsewhere). Not precision-tuned; friends-and-family scale.
9. **Node store:** node bundles (small `node.js` files) live in **Postgres `bytea`** — a deliberate carve-out from the no-blob-in-v1 rule (that rule is about heavy *user media*, not code bundles). Publishing is auth-gated and **curated**; owner-only gating + server-side permission re-derivation are follow-ups. (See "Node store".)

## Decisions still open (to resolve before/while writing Go)

1. **Conflict mechanism:** exact divergence detection (per-item version vector? last-common-sync timestamp?). The active-note auto-pull exclusion keeps the common case safe, but the underlying divergence-detection mechanism still needs picking.
2. **Games (server side):** deferred entirely. When picked up: how game bundles are stored/served, and whether the server relays opaque state, validates turns, or holds authority. Don't foreclose any of these now.
3. **Rooms beyond messaging:** file sharing, file viewing, shared editing, etc. are anticipated but unspecified — design each when picked up. v1 is room-lifecycle + chat only.
4. **S3 / blob-storage phase:** one later phase covering photo libraries, DM image attachments, syncing note attachments across devices, and any room files. Bucket layout, per-file caps, lifecycle, and which features it unblocks all get designed then.

## What to refuse / push back on (server-level)

- ❌ Federation or cross-server features. One official server; private servers are isolated.
- ❌ Making the server the authoritative editing copy. It holds the *published* mirror.
- ❌ Public discovery / username search. Friend codes only.
- ❌ Any media/blob handling in v1 — no photo pass-through, no image storage. All heavy media is deferred to one later S3 phase. Don't build piecemeal media handling before then.
- ❌ Persisting rooms past expiry.
- ❌ OS push infrastructure. The server feeds in-app indicators only.
- ❌ Baking Tauri-client assumptions into the API. The API is client-agnostic.
- ❌ Over-engineering for scale. Tens of users.
- ❌ Heavy ORMs / magic frameworks. Keep Go boring and explicit (`net/http` + chi + sqlc/pgx).

## Note about friends/blocking

v1 limitation: blocks are single-row-per-pair, so a user can't independently maintain a block against someone who has blocked them — if the original blocker unblocks, no block remains. Acceptable at friends-and-family scale; future fix is a separate directional blocks table. Put it near the rooms/photos deferred-decisions area or wherever limitations best fit.

Then run the end-to-end suite for remove/block/unblock and report the table: unfriend accepted (200, then 404 repeat), block from no-relationship / pending / accepted, self-block (400), block unknown user (404), double-block-by-me (200 idempotent), unblock as blocker (200), unblock when not blocker (404), unfriend non-friend (404), unauth on all three (401).