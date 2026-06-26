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

## Server-relevant TODO (from the master roadmap)

- [ ] **Owner-gating on `POST /nodes`** — publishing is open to any authed user now;
  restrict to owner/curated.
- [ ] **Docs carve-out** — record that node bundles are stored as Postgres `bytea`
  (the app's own *code* distribution), distinct from the v1 "no user media/blob"
  rule (photos/S3). Update `SERVER.md` + `CLAUDE.md` (and add the `nodes` domain to
  CLAUDE.md's "Built" section).
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
