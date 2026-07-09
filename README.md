# Bombers Server

The Go backend for **Bombers**. See [`PRODUCT.md`](./PRODUCT.md) for the product vision and [`SERVER.md`](./SERVER.md) for the server spec.

The Tauri/React client lives in a separate repo — this codebase is server-only.

## Requirements

- Go 1.25+
- PostgreSQL (reachable via `DATABASE_URL`)
- An S3-compatible object store for profile media (MinIO locally — see below; MinIO-on-VPS or Cloudflare R2 in prod)
- [`goose`](https://github.com/pressly/goose) CLI for migrations:
  ```powershell
  go install github.com/pressly/goose/v3/cmd/goose@latest
  ```

Both Postgres and MinIO come up together for local dev:

```bash
docker compose up -d
```

MinIO serves the S3 API on `localhost:9000` (web console on `localhost:9001`, log in with `S3_ACCESS_KEY`/`S3_SECRET_KEY`). The server creates the media bucket itself on startup — no manual MinIO setup needed.

## Configuration

The server is configured via environment variables. On startup it loads variables from a `.env` file in the working directory (if present) before reading the process environment. A missing `.env` is fine; any other read error will abort startup.

| Variable        | Required | Default          | Description                                                        |
| --------------- | -------- | ---------------- | ------------------------------------------------------------------ |
| `PORT`          | no       | `8080`           | TCP port the HTTP server binds to.                                 |
| `DATABASE_URL`  | **yes**  | —                | PostgreSQL connection string, e.g. `postgres://...`.               |
| `TOKEN_SECRET`  | **yes**  | —                | Secret key for signing JWTs (32+ random bytes, base64).            |
| `S3_ACCESS_KEY` | **yes**  | —                | Object-storage access key (doubles as MinIO's root user locally).  |
| `S3_SECRET_KEY` | **yes**  | —                | Object-storage secret key (doubles as MinIO's root password).      |
| `S3_ENDPOINT`   | no       | `localhost:9000` | S3-compatible endpoint, `host:port` without a scheme.              |
| `S3_BUCKET`     | no       | `bombers-media`  | Bucket for profile media; created on startup if missing.           |
| `S3_USE_SSL`    | no       | `false`          | `true` for https endpoints (VPS MinIO / Cloudflare R2).            |

A minimal `.env`:

```
PORT=1337
DATABASE_URL=postgres://user:password@localhost:5432/bombers?sslmode=disable
TOKEN_SECRET=replace-with-a-32-byte-base64-secret
S3_ACCESS_KEY=minioadmin
S3_SECRET_KEY=minioadmin
```

`.env` is gitignored — do not commit local secrets.

## Database migrations

Migrations live in [`migrations/`](./migrations) as goose SQL files.

```powershell
# apply all pending migrations
goose -dir migrations postgres $env:DATABASE_URL up

# see what's applied / pending
goose -dir migrations postgres $env:DATABASE_URL status

# roll back the most recent migration
goose -dir migrations postgres $env:DATABASE_URL down

# scaffold a new migration
goose -dir migrations postgres $env:DATABASE_URL create <name> sql
```

On bash/zsh swap `$env:DATABASE_URL` for `"$DATABASE_URL"`.

After running `goose ... up`, connect with `psql` (or any client) and verify:

```sql
\dt              -- should list `users` and `goose_db_version`
\d users         -- should show id / username / password_hash / friend_code / created_at
```

## Running

```bash
go run ./cmd/server
```

You should see:

```
listening on :1337
```

You can also override variables inline:

**PowerShell**

```powershell
$env:PORT = "8080"
go run ./cmd/server
```

**bash / zsh**

```bash
PORT=8080 go run ./cmd/server
```

## Health check

Once running, confirm the server is up and connected to the database:

```bash
curl http://localhost:1337/health
```

Expected response: `{"status":"ok","db":"up","media":"up"}` (HTTP 200). If Postgres is unreachable you'll get `{"status":"degraded","db":"down",...}` (HTTP 503). The `media` field reports object-storage reachability and is informational — only the database governs the status/HTTP code.

## Build

```bash
go build ./...
```
