# Bombers Server

The Go backend for **Bombers**. See [`PRODUCT.md`](./PRODUCT.md) for the product vision and [`SERVER.md`](./SERVER.md) for the server spec.

The Tauri/React client lives in a separate repo — this codebase is server-only.

## Run your own server

Five commands, from inside the folder you cloned. You never type `go` or `goose`.

```bash
git clone https://github.com/tabuhana/bombers-server.git
cd bombers-server

./bombers install    # once: builds and puts `bombers` on your PATH
bombers setup        # configure the database + media, then migrate
bombers start        # run it in the background
bombers              # open the admin console (exiting leaves the server running)
bombers stop         # stop it
```

`./bombers` in the repo root is a launcher script: it compiles the server if the
binary is missing or out of date, then runs it. That's why a fresh clone needs no
`go build`. After `install`, the script is out of the picture — you just type
`bombers` from anywhere.

> **On Windows**, use `.\bombers install` in PowerShell or cmd — the `.\` matters.
> A bare `bombers` in cmd.exe finds the extensionless POSIX script sitting next to
> the Windows one, can't execute it, and stops there with a confusing error.

`install` and `setup` are deliberately separate: reconfiguring later never means
reinstalling. `install` builds and records where your checkout lives (in
`<dataDir>/install.json`, so `update` works from any directory); it configures
nothing and touches no database.

The binary lands in `/usr/local/bin` when that's writable, otherwise
`~/.local/bin` — and on Windows, `%LOCALAPPDATA%\Programs\Bombers`, which
`install` adds to your user PATH for you (no admin needed; the machine-wide PATH
is never touched). **Open a new terminal after installing** — the one you ran it
in still has the PATH it started with. On Linux and macOS it prints the `export`
line to add instead, since there's no shell config it can safely edit for you.

### Requirements

- **Go 1.25+** — the server is built from source on your machine. There are no
  release binaries.
- **A 64-bit machine** if you want the built-in database. The embedded Postgres
  is published for amd64/arm64 only; on 32-bit, `setup` warns you at the pick and
  `start` refuses with an explanation. Point it at your own Postgres instead.

Nothing else. No Docker, no Postgres install, no S3 account, no `goose` CLI —
`bombers setup` offers a bundled Postgres and plain-files media storage, and both
are the default answers.

### What `setup` asks

A short sequence, each step a choice and then its details:

1. **Where you're running it** — a computer on your network, or a server with a domain name
2. **Port**
3. **Database** — a built-in Postgres it manages for you, or a connection URL you provide
4. **Media** — files on disk, or an S3-compatible store (MinIO, Cloudflare R2)

At a terminal these are menus you arrow through. Piped or scripted input falls
back to typed prompts answering the same four questions, so an install can be
automated.

Answers are saved to `config.json` in the data directory and it migrates on the
way out. Re-run `bombers setup` any time to change them.

**Environment variables always win.** They're read first, and the saved config
only fills what they don't set — so a fully configured environment (a container,
a systemd unit with `Environment=` lines) never sees the wizard at all.

### Putting it on the internet (a rented VPS)

Pick **"Anyone, at a domain name"** and setup asks for the domain, keeps the
server bound to `localhost`, and finishes by printing the three steps it can't do
for you. In short:

1. Point an **A record** for your domain at the machine's IP address.
2. [Install Caddy](https://caddyserver.com/docs/install).
3. Put two lines in `/etc/caddy/Caddyfile` and reload it:

```
bombers.example.com {
	reverse_proxy 127.0.0.1:1337
}
```

Caddy obtains the HTTPS certificate and renews it on its own — no account, no
paperwork. The DNS record is the proof of ownership the certificate authority
checks, which is why it has to exist first. If the domain is on Cloudflare, set
that record to **DNS only** (grey cloud), or the check never reaches your
machine.

**Why a proxy rather than HTTPS in the server.** Certificates are issued against
ports 80 and 443, and binding those on Linux needs root. Nothing in this install
path needs sudo, and a reverse proxy is already a system service holding exactly
those privileges — so Bombers stays an ordinary unprivileged process listening on
localhost, and only Caddy faces the internet.

You'll also want it running after a reboot — see **Start on boot** below.

> **Don't skip this and just open the port.** Binding `0.0.0.0` on a public VPS
> works, and every password and token then crosses the internet readable. The
> client refuses a bare public IP for that reason: anything that isn't loopback
> or a private LAN address gets `https://` forced onto it.

### Where things live

The data directory is `%AppData%\Bombers` on Windows,
`~/Library/Application Support/Bombers` on macOS, `~/.config/Bombers` on Linux —
or wherever `BOMBERS_DATA_DIR` points. It holds `config.json`, `install.json`,
`server.log`, the pidfile, filesystem media, and the embedded Postgres data
directory and cached binaries.

### The rest of the commands

| Command | What it does |
| ------- | ------------ |
| `bombers` | Open the admin console against the running server |
| `bombers install` | Build and put `bombers` on your PATH — once, from your checkout |
| `bombers setup` | The config wizard, then migrate. Re-run to reconfigure |
| `bombers start` | Run in the background (`--foreground` to stay attached, `--headless` for no console) |
| `bombers stop` | Stop the background server |
| `bombers status` | Is it running, and where are its logs |
| `bombers logs [lines]` | Tail the background server's log |
| `bombers update` | After a `git pull`: rebuild and migrate. Does not serve |
| `bombers doctor` | Check the local setup for problems (exits 1 on any failure) |
| `bombers console` | Admin console against a headless/service server — same DB, separate process |
| `bombers service …` | Register as an OS background service (see below) |
| `bombers uninstall` | Remove the service and delete the data directory |
| `bombers version` · `bombers help` | Print and exit |

### After a `git pull`

```bash
bombers update
```

It rebuilds from the checkout it recorded at install time, then applies pending
migrations from that same checkout — so it works from any directory. It does not
start the server; that stays a separate act. Start it again with `bombers start`.

### Start on boot

`bombers start` is enough to run in the background day to day. Register an OS
service (Windows Service / systemd / launchd) when you want start-on-boot and
restart-on-failure:

```bash
bombers service install     # ADMIN PowerShell on Windows, sudo on Linux/macOS
bombers service start
```

Also: `service stop | restart | status | uninstall`. Run `bombers setup` first —
a service has no terminal to answer the wizard on, so it refuses to start with an
incomplete config.

> **Windows:** the service runs as LocalSystem, which resolves a *different* data
> directory than your user account. Set a machine-level `BOMBERS_DATA_DIR`
> (`setx BOMBERS_DATA_DIR C:\ProgramData\Bombers /M`), then run `bombers setup`,
> then install the service — otherwise it starts, finds no config, and stops.

`bombers uninstall` is the full teardown: it deregisters the service, then —
after a confirmation, or `--yes` for scripts — deletes the entire data directory
(config, filesystem media, embedded-Postgres data and cached binaries).

## Health check

```bash
curl http://localhost:1337/health
```

Expected: `{"status":"ok","db":"up","media":"up"}` (HTTP 200). If Postgres is
unreachable you get `{"status":"degraded","db":"down",...}` (HTTP 503). The
`media` field reports object-storage reachability and is informational — only the
database governs the status and the HTTP code.

## Configuration reference

The server reads a `.env` file in the working directory (if present) before the
process environment. A missing `.env` is fine; any other read error aborts
startup. Anything the environment doesn't set falls back to the saved
`config.json`, then to the defaults below.

| Variable              | Required                    | Default                  | Description |
| --------------------- | --------------------------- | ------------------------ | ----------- |
| `PORT`                | no                          | `8080`                   | TCP port to bind. |
| `HOST`                | no                          | all interfaces           | Bind address. `127.0.0.1` for this machine only. |
| `TOKEN_SECRET`        | **yes**                     | —                        | JWT signing key (32+ random bytes, base64). `setup` generates one. |
| `DB_BACKEND`          | no                          | `external`               | `external` (your Postgres) or `embedded` (the bundled one). |
| `DATABASE_URL`        | when `DB_BACKEND=external`  | —                        | e.g. `postgres://user:pass@localhost:5432/bombers?sslmode=disable`. |
| `MEDIA_BACKEND`       | no                          | `s3`                     | `s3` or `fs` (plain files on disk). |
| `MEDIA_DIR`           | when `MEDIA_BACKEND=fs`     | —                        | Filesystem root for media. |
| `S3_ACCESS_KEY`       | when `MEDIA_BACKEND=s3`     | —                        | Access key (doubles as MinIO's root user locally). |
| `S3_SECRET_KEY`       | when `MEDIA_BACKEND=s3`     | —                        | Secret key (doubles as MinIO's root password). |
| `S3_ENDPOINT`         | no                          | `localhost:9000`         | `host:port`, no scheme. |
| `S3_BUCKET`           | no                          | `bombers-media`          | Created on startup if missing. |
| `S3_USE_SSL`          | no                          | `false`                  | `true` for https endpoints (VPS MinIO / R2). |
| `CORS_ALLOWED_ORIGIN` | no                          | `http://localhost:1420`  | The client's origin (the Vite dev server by default). |
| `ADMIN_USERNAME`      | no                          | —                        | Promotes that account to admin at boot. An unknown name warns, it doesn't fail. |
| `AUTO_MIGRATE`        | no                          | `false`                  | Apply pending migrations at startup (for container hosts). |
| `LOG_TIME_FORMAT`     | no                          | `datetime`               | `time` · `datetime` · `iso`. The console's `logtime` switches it live. |
| `BOMBERS_DATA_DIR`    | no                          | per-OS config dir        | Override where config, logs, media and the embedded DB live. |

A minimal `.env` for the external-Postgres + S3 path:

```
PORT=1337
DATABASE_URL=postgres://user:password@localhost:5432/bombers?sslmode=disable
TOKEN_SECRET=replace-with-a-32-byte-base64-secret
S3_ACCESS_KEY=minioadmin
S3_SECRET_KEY=minioadmin
```

`.env` is gitignored — do not commit local secrets.

## Container hosts (Railway, Fly, plain Docker)

`install` / `setup` / `update` and the admin console are the self-hosted path and
don't apply here: there's no PATH to install into, no wizard to answer, no git
pull to react to. Build the binary from the repo, run `bombers start`, configure
with environment variables.

Two behaviours make that work with nothing special configured:

- **`start` stays in the foreground when stdout isn't a terminal.** Backgrounding
  is for a human shell; in a container, forking away looks like the process
  exiting and the platform restarts forever. The same check keeps a
  systemd-launched process attached.
- **`AUTO_MIGRATE=true` applies pending migrations at startup**, so a deploy
  carries its own schema. Opt-in on purpose — on a managed database, schema
  changes shouldn't be a side effect of a restart. Leave it unset and run
  `bombers update` pre-deploy if you'd rather a failed migration block the
  deploy.

`PORT` is read from the environment, `/health` is the health-check path, and
SIGTERM triggers a graceful shutdown.

## Developing on the server

Working *on* this codebase, rather than running it, is the plain Go workflow:

```bash
go build ./...
go vet ./...
go test ./...
gofmt -w .
go run ./cmd/bombers start --headless
```

A local Postgres + MinIO come up together with Docker if you'd rather develop
against the same pair as production:

```bash
docker compose up -d
```

Postgres binds 5432 (`DB_NAME`/`DB_USER`/`DB_PASSWORD`); MinIO serves the S3 API
on 9000 with a web console on 9001 (`S3_ACCESS_KEY`/`S3_SECRET_KEY`). The server
creates the media bucket itself on startup — no manual MinIO setup.

### Migrations

Migrations live in [`migrations/`](./migrations) as goose SQL files, and are
**embedded in the binary**. `bombers setup` applies them on a first install and
`bombers update` applies them after a pull — through the goose *library*, so
neither needs the CLI, an exported `DATABASE_URL`, or a `-dir` flag.

The CLI is still useful for what a library call can't do — rolling back, checking
status, scaffolding a new file:

```bash
go install github.com/pressly/goose/v3/cmd/goose@latest

goose -dir migrations postgres "$DATABASE_URL" status          # applied / pending
goose -dir migrations postgres "$DATABASE_URL" down            # roll back the last one
goose -dir migrations postgres "$DATABASE_URL" create <name> sql
```

New migration filenames are timestamped (`YYYYMMDDHHMMSS_<name>.sql`).
