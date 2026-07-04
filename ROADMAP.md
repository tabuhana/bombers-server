# Bombers Server — roadmap (mirror)

> **Master roadmap lives in the client repo:** `bombers/client/docs/ROADMAP.md`.
> Plans + docs are kept synced across both repos (owner convention) — this file
> mirrors the **server-relevant slice**. Update both when direction shifts.

## ✅ Recently shipped (server)

- [x] **`internal/nodes` domain** — the node store. `GET /nodes` (catalog metadata),
  `GET /nodes/{id}/bundle` (the node.js bytes, hash in the ETag), `POST /nodes`
  (publish/replace, sha256-verified). Bundle bytes + metadata in a `nodes` table
  (goose migration `20260625160000_create_nodes.sql`). Full protocol: client
  `docs/NODE_INSTALL.md`.
- [x] **Docs carve-out** — `SERVER.md` (Node store section + decision #9) and
  `CLAUDE.md` (nodes in the Built list, routes, migrations) now record that node
  bundles are Postgres `bytea` (the app's own *code* distribution), distinct from
  the v1 "no user media/blob" rule. The client↔server node endpoints are also now
  in the client's `docs/API.md` (§9).
- [x] **Client node builder** — the desktop builder that produces nodes for `POST
  /nodes` (and local installs) is built (client Phase 4.1–4.3). The server side it
  feeds is unchanged.

## Server-relevant TODO (from the master roadmap)

- [ ] **Owner-gating on `POST /nodes`** — publishing is open to any authed user now;
  restrict to owner/curated.
- [ ] **Server-side permission re-derivation on publish** — the client auto-detects a
  node's permissions from its code, but the server stores them as-sent; re-derive /
  validate server-side so a published node can't over-declare. (Static `host.*` scan,
  mirroring the client's `core/nodes/permissions.ts`.)
- [ ] **Sharing** (`internal/sharing`) — let a client pull items friends shared with
  it (unbuilt; today sync pull is own-items only).
- [ ] **Real-time** (`internal/realtime`, WebSocket) — the "feels live" sync nudge +
  live DM delivery (messaging is durable REST only today).
- [ ] **Move all core nodes to the server** — publish the currently-bundled nodes so
  clients download everything (LOW priority in the master list).

## Doc-sync convention

- The full product roadmap + Editor/Node plans live in the **client** repo docs:
  `ROADMAP.md`, `EDITOR_PLAN.md`, `EDITOR_FEATURES.md`, `NODES.md`, `NODE_INSTALL.md`.
- This `ROADMAP.md` mirrors the **server** slice; the node-store contract is
  authoritative in the client's `NODE_INSTALL.md`. Keep them in step on every change.
