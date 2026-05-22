# Bombers Server

The Go backend for **Bombers**. See [`PRODUCT.md`](./PRODUCT.md) for the product vision and [`SERVER.md`](./SERVER.md) for the server spec.

The Tauri/React client lives in a separate repo — this codebase is server-only.

## Requirements

- Go 1.25+

## Configuration

The server is configured via environment variables. On startup it loads variables from a `.env` file in the working directory (if present) before reading the process environment. A missing `.env` is fine; any other read error will abort startup.

| Variable | Required | Description                  |
| -------- | -------- | ---------------------------- |
| `PORT`   | yes      | TCP port the HTTP server binds to. |

A minimal `.env`:

```
PORT=1337
```

`.env` is gitignored — do not commit local secrets.

## Running

```bash
go run ./cmd/server
```

With the `.env` above you should see:

```
listening on :1337
```

You can also override or supply variables inline:

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

Once running, confirm the server is up:

```bash
curl http://localhost:1337/health
```

Expected response: `ok` (HTTP 200).

## Build

```bash
go build ./...
```
