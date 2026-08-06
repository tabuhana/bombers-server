# LOCAL_MODE.md — running the Bombers server: managed vs local self-host

> Design doc + build plan for how the server is configured and launched. Two ways
> to run it: **managed** (cloud/VPS/Railway — env-driven, zero interaction) and
> **local** (self-host — an interactive first-run wizard that can provision its
> own Postgres + media storage with no Docker). Written before implementation;
> the phased plan at the bottom is how it gets built. This describes **direction**
> the same way `SERVER.md` does — the owner's live steering wins over stale text.

---

## 1. The two modes

| | **Managed (default)** | **Local (self-host)** |
| --- | --- | --- |
| Who | VPS / Railway / any cloud host | Someone running it at home / on a LAN |
| Config source | environment variables only | interactive wizard → saved config file (env still overrides) |
| Interaction | none — boots and serves | a first-run wizard; silent on every run after |
| Postgres | external `DATABASE_URL` you provide | **embedded** (server runs its own) *or* external, your pick |
| Media | external S3 (Cloudflare R2, etc.) | **filesystem** *or* local S3 (embedded MinIO) *or* external S3, your pick |
| Downloads on boot | **none** — tiny binary, just runs | only what your picks need (see §6) |
| TLS | terminated by the platform | plain HTTP on the LAN (fine on a trusted network) |

The single rule that keeps both honest:

> **Environment variables always win. The wizard only fills what env didn't set.**

So on Railway you set env and never see a prompt; on a laptop you run `bombers setup`
(or just `bombers start`, which auto-detects an incomplete config) and it
hand-holds you, writing your answers to a config file so the next run is
silent. Nothing the wizard does can override an explicit env var.

---

## 2. Managed mode (the default) — already almost entirely built

`bombers` with no subcommand (a bare `bombers`, i.e. `start`) is today's behavior
and stays the cloud path — `start` auto-detects local config, so there is no
separate `local` command:

- `config.Load()` reads env (`DATABASE_URL`, `TOKEN_SECRET`, the S3 block, `PORT`,
  `CORS_ALLOWED_ORIGIN`). Missing required vars → a clear fatal listing them.
- Binds `0.0.0.0:$PORT` (Go's `":"+port`). Railway sets `$PORT`; you paste the
  platform's public URL into the client's server picker.
- Non-TTY stdin (a real deploy) already falls back to headless signal-waiting, so
  the admin console never blocks a container.
- **Downloads nothing.** External Postgres + external S3 means neither the
  embedded Postgres binary nor MinIO is ever fetched — the image stays lean.

The only friendliness gap on this path is that `TOKEN_SECRET` is required — which
is *correct* for cloud (you want a stable secret set once, so restarts don't
invalidate every token). Auto-generating a secret is a **local-only** convenience
(§4); managed still expects you to set it.

**Managed mode is not getting an interactive layer.** Predictable beats clever for
a headless deploy.

---

## 3. Config precedence (one model, three layers)

Highest wins:

1. **Explicit environment variable** — always authoritative.
2. **The local config file** — written by the wizard (local mode only).
3. **Generated / built-in default** — e.g. an auto-made `TOKEN_SECRET`, the `PORT`
   default, the "all-in-one" backend defaults.

**Mechanism (keeps `config.Load` untouched):** in local mode, before `config.Load`
runs, a setup step resolves the config file + wizard answers and writes each
resolved value into the process environment **only if that var isn't already set**.
Then `config.Load()` reads a fully-populated environment exactly as it does on
cloud. Real env vars are never overwritten, so precedence falls out for free and
there's one loader, not two.

---

## 4. The local config file

- **Format:** JSON (stdlib, no new dependency). Machine-written by the wizard;
  humans rarely hand-edit it — env is the human override.
- **Location:** `<dataDir>/config.json`, where `dataDir` is `BOMBERS_DATA_DIR` if
  set, else `os.UserConfigDir()/Bombers` (`%AppData%\Bombers` on Windows,
  `~/.config/Bombers` on Linux, `~/Library/Application Support/Bombers` on macOS).
  Everything the local server owns lives under `dataDir`: the config file, the
  embedded Postgres data dir, filesystem media, and downloaded binaries.
- **Holds:** bind host, port, `TOKEN_SECRET`, CORS origin, the DB choice
  (embedded vs an external URL), the media choice (fs / local-s3 / external-s3)
  and any S3 endpoint/creds, plus the resolved data dir.
- **Secret:** `TOKEN_SECRET` is generated once (crypto/rand → base64) and saved
  here so tokens survive restarts. It's plaintext in a file on the user's own
  machine — acceptable for local self-host; documented as such.

---

## 5. The first-run wizard

Runs when: **local mode + interactive terminal + config still incomplete.** On a
non-TTY (or when env already supplies everything) it does **not** prompt — it just
uses what's there or fails with guidance. Re-runnable any time via `bombers setup`
(a.k.a. reconfigure) when you want to change your mind.

It's a **straight step-by-step walk-through** — one decision at a time, each a
choice plus its configuration where the choice needs one: a
select → configure → select → configure flow (hermes/openclaw style), **not** a
bundled preset. In order:

1. **Where are you running this?** — a computer on my network (`0.0.0.0`, plain
   HTTP on a trusted network) or a server with a domain name (then enter the
   domain; binds `127.0.0.1`, because a reverse proxy owns the public port and
   forwards here — setup prints the Caddyfile at the end).

   > There was a third answer, `127.0.0.1` with no domain — "this machine only".
   > It went because it answered a question nobody asks: a personal notebook
   > server you can't reach from your own laptop isn't the setup anyone wants.
2. **Port** — default `1337`.
3. **Database** — run Postgres for me (embedded, the default — nothing to install)
   or use my own (then enter a `DATABASE_URL`).
4. **Media** — store as local files (the default — no daemon/download) or use
   S3/MinIO (then enter endpoint + keys). *(local-S3-via-embedded-MinIO is a later
   add; today the media pick is files or external S3.)*

`TOKEN_SECRET` is generated silently. The data dir isn't asked — it's
`os.UserConfigDir()/Bombers` (or `BOMBERS_DATA_DIR`). After the last question it
resolves dependencies (downloads what's needed — §6), runs migrations (embedded
DB), and boots. The existing `bombers>` admin console stays for live ops on top.

> **Design note:** an earlier draft opened with a preset menu (all-in-one /
> bring-your-own / customize); the owner overrode that in favor of this
> per-decision walk-through. Presets are not coming back unless he asks.

---

## 6. Backends — two DB adapters, three media adapters

The point of "pickable" is cheap **because each backend is just an adapter behind
one interface** — you write a handful of small adapters and any combination
composes; you do not write a code path per combination.

### Database
- **External** — a `DATABASE_URL` (managed default; a valid local pick too).
- **Embedded** — the server runs a **real Postgres binary as a subprocess** (via
  the `fergusstrange/embedded-postgres` library — a **new dependency**, added in
  P3). `pgx` connects over a normal localhost URL, so every query/domain is
  unchanged. Data lives under `<dataDir>/pg`, bound to `127.0.0.1` only. Started
  before serving, stopped on graceful shutdown.

Why embed Postgres rather than "just use files": a relational DB needs an engine —
there's no filesystem shortcut. So the simplest *real* local DB is a real Postgres
we run for you.

### Media (the `media.Storage` seam)
`media/` already hides object storage behind a wrapper. It becomes an interface
(`Put` / `Get` / `Delete` / `Stat`, streaming) with these implementations —
**metadata (`user_media` table), the authenticated pass-through, friend-gating,
and ETags are all backend-agnostic; only the raw byte I/O differs:**

- **Filesystem** — blobs under `<dataDir>/media/users/<id>/<kind>`. No process, no
  download, ~a hundred lines. The local default. (Reinforces the "never presigned
  URLs" rule — there's nothing to presign, you stream the file.)
- **External S3** — the existing minio-go client, pointed at **R2** / MinIO-on-VPS
  purely via env (`S3_ENDPOINT` + keys + bucket + SSL). Managed default. R2 speaks
  the same S3 API, so "MinIO → R2" is env only, never code.
- **Local S3 (embedded MinIO)** — for someone who specifically wants object
  storage locally without Docker: the server runs the `minio` binary as a
  subprocess (bound to localhost, own data dir) and the External-S3 adapter points
  at it. Opt-in only.

Why filesystem is the local default (not embedded MinIO): media is just blobs, and
the filesystem *is* blob storage — no daemon needed. Embedding MinIO pulls a
~100 MB binary + a long-running daemon + AGPL strings for the common case; the
filesystem sidesteps all of it. So embedded-MinIO stays a *pick*, never the default.

---

## 7. Lazy dependency downloads

The base binary ships with **no** embedded Postgres and **no** MinIO. A dependency
is fetched only when your answers require it, once, then cached. Cost scales with
your picks:

| Setup | Downloads |
| --- | --- |
| Managed (external PG + R2) | **nothing** |
| Local all-in-one (embedded PG + filesystem) | Postgres binary only (~30–60 MB) |
| Local + "I want local S3" | Postgres binary **+** MinIO (~100 MB) |

Rules:
- **Cache** binaries under `<dataDir>/bin/` — one-time cost; later boots are
  instant. (`embedded-postgres` caches for us; MinIO we cache ourselves.)
- **Pin + verify** — lock to a known-good version and check the download's
  checksum before executing it (we're auto-running a fetched binary).
- **Right build per machine** — correct OS/arch (the PG library handles this;
  MinIO we map ourselves).
- **Fail soft offline** — a fetch that can't reach the network stops with a clear
  message (and, where sensible, offers "use filesystem instead?"), never a
  half-start.
- **Flow:** ask all questions → *then* resolve + download → *then* boot. One clean
  "setting up…" step, never mid-questionnaire.

**Licensing:** lazy-fetching (the user's machine downloads MinIO from MinIO at
their request) means we do **not** bundle/redistribute AGPL software — we just run
a tool they fetched. That sidesteps the AGPL concern that bundling would raise.

---

## 8. Networking / bind

- Managed: `0.0.0.0:$PORT` (the platform must reach it). Unchanged.
- Local: `Host` is configurable — `127.0.0.1` (this machine) or `0.0.0.0` (LAN).
  Default host stays empty (`":"+port` = all interfaces) so current behavior is
  preserved when nothing sets it.
- Firewall + "same-network-only" is the OS/router's job (the user opens TCP on the
  port, scoped to the local subnet; not port-forwarding keeps it off the
  internet). The paired client tweak: treat private LAN IPs as `http://` in the
  server picker so a bare `192.168.x.x` doesn't get forced to `https`.

---

## 9. Migrations

Migrations are **embedded into the binary** (Go `embed`) and run programmatically
on startup in local mode, so a self-hoster never installs goose or runs a separate
step. Managed can also auto-run on boot (with a guard so concurrent instances
don't race) — **open decision** (§12): auto-on-boot vs a deliberate deploy step.

---

## 10. Security notes

- **Plain HTTP on the LAN** — passwords/tokens travel unencrypted on the local
  network. Fine for a trusted home LAN; not for a shared/public one. TLS is a
  later option, not required for v1 self-host.
- **`TOKEN_SECRET` in the config file** — plaintext on the user's own machine.
  Acceptable for local; documented.
- **Embedded Postgres / MinIO bind to `127.0.0.1` only** — never exposed on the
  LAN, even when the API itself is. That's the "locked down, Bombers-only" part.

---

## 11. Phased build plan

Each phase compiles + is verifiable on its own (`go build ./... && go vet ./...`;
the API's external path is exercised the whole way). The interactive wizard can't
be driven headless here, so its prompts get a human run-through as they land.

- **P1 — config + mode + bind foundation (external backends only).**
  Layered config (env > file > default) via the pre-populate-env mechanism; a
  `setup` command (reconfigure) + `start` auto-detecting local config (no
  separate `local` command); JSON config file + data-dir
  resolution; auto-generated + persisted `TOKEN_SECRET`; configurable bind host
  (localhost/LAN); the wizard skeleton (bind + port; backend picks are stubbed to
  the current external path). No new dependencies. Current behavior preserved
  exactly when env is set.
- **P2 — filesystem media backend.** Turn `media.Storage` into an interface; add
  the filesystem adapter; select it when the media pick is `fs`. Unit-testable
  (pure byte I/O), no downloads.
- **P3 — embedded Postgres + auto-migrations + lazy download.** Add
  `fergusstrange/embedded-postgres` (**new dep**); subprocess lifecycle;
  `embed` the migration SQL + run on start; the pin/verify/cache download rules.
  The biggest piece.
- **P4 — embedded MinIO option + polish. — MOSTLY DONE.** Done: the client
  http-for-LAN-IP tweak (RFC1918 addresses default to `http://`) and the wizard
  cleanup — a step-by-step select→configure walk-through (an earlier preset draft
  was reverted at the owner's request; see §5). **Still pending — only the
  embedded-MinIO local-S3 pick** (`minio` subprocess + lazy download): held on
  purpose (AGPL, no turnkey Go library, niche opt-in) — filesystem + external S3
  already cover local media, so it's optional, not a blocker.
- **P5 — run as a background service. — DONE (service + `bombers uninstall`).**
  `service install / start / stop / restart / status / uninstall` subcommands
  register the binary with the OS service manager (Windows Service / systemd /
  launchd) via `kardianos/service` (the single new dep) so it runs **detached** —
  survives closing the terminal + logout, auto-starts on boot. The "configure
  once, close the terminal, it's just there" model. **How the launch is routed:**
  the binary self-detects an SCM launch through `service.Interactive()` (no marker
  flag or special Arguments needed) — false only under the service manager, which
  then runs headless through `s.Run()`; a human terminal (including
  `start --headless`) keeps the interactive `bombers>` console path exactly as
  before. The serve logic is factored into `buildAndServe()` + `(*app) shutdown()`
  in `main.go`, shared unchanged by the CLI and the service, so a service stop runs
  the SAME graceful shutdown (HTTP → `pool.Close` → embedded-PG stop). The
  service's `Start` returns immediately and does the real work (which on a first
  run may download the embedded-Postgres binary) in a goroutine, because the
  Windows SCM kills a slow Start. Installing/uninstalling needs admin/root — the
  CLI surfaces that hint on a permission failure. Also lands the top-level
  **`bombers uninstall`** — full teardown: remove the OS service if installed
  (best effort — a missing service or a privilege problem is reported, not fatal),
  then (after an explicit `--yes` flag or a `y/N` confirm; refused when
  non-interactive without `--yes`) delete the data dir (config, filesystem media,
  embedded-PG data, cached binaries) and nothing outside it. The counterpart to
  download-and-install. (Service-stopping any embedded **MinIO** is moot until the
  P4 pick above exists; embedded **Postgres** is stopped today.)

> **Companion — `bombers doctor`.** A non-destructive diagnostics command that
> resolves config the way `start` does (the local-config layer under env, then
> `config.Load`) but **never fatals**, then runs a checklist — data dir writable,
> config complete, build arch vs. embedded-PG (a 386 build can't run embedded
> Postgres), port free, external DB reachable, media writable/reachable — printing
> one ASCII-marked line each (`[ ok ]` / `[FAIL]` / `[warn]` / `[skip]`) and
> exiting non-zero if anything FAILed, so `bombers doctor && bombers start` gates a
> launch on a clean bill of health. It starts nothing (embedded Postgres/MinIO are
> left for `start`).

> **Companion — `bombers console`.** Once the server runs detached (`--headless`
> or the OS service, §P5), `bombers console` opens the same interactive `bombers>`
> admin panel **against that running server** by connecting to the **same
> database** it uses — a separate process, DB-connected, **not** an attach to the
> server process. It resolves config the way `start` does (local-config layer under
> env → `config.Load`) and dials the embedded-Postgres URL or your external
> `DATABASE_URL` automatically. Run the usual admin commands (users / status /
> address / node-store publish) live; because it is DB-connected it **cannot** stop
> the server — `exit` / `quit` / `stop` just leave the console and the server keeps
> serving (stop that with `bombers service stop` or a signal to its process).

---

## 12. Decisions

**Decided (this design):**
- Env-first; managed stays pure env, downloads nothing, no interactive layer.
- Local media default = **filesystem**; local DB default = **embedded Postgres**;
  both fully **pickable** (external, or local-S3 / your-own-PG).
- Embedded MinIO is a **pick, never the default** (weight + AGPL).
- **Lazy, cached, verified** dependency downloads gated on picks.
- Config file = **JSON** under `os.UserConfigDir()/Bombers` (override
  `BOMBERS_DATA_DIR`); env overrides it; re-runnable via `bombers setup`.
- **Background-service mode is wanted** (P5): configure once, then run detached and
  survive closing the terminal (a Windows Service for the owner), not just
  `--headless` which stays attached to the terminal.

**Open:**
- Auto-run migrations on the **managed** path (on-boot vs deploy step)? Local = yes.
- Embedded Postgres port — reuse 5432 (may clash with a system Postgres) or a
  Bombers-specific port.
- Split config-dir vs data-dir later (Roaming AppData isn't ideal for large PG
  data on Windows), or keep one `dataDir` for simplicity.

---

## 13. What does NOT change

- The HTTP API + every domain — backends swap underneath a stable interface.
- The managed/cloud path — still pure env, still downloads nothing.
- The "media serves through the server, never presigned URLs" rule.
- The local-working-copy / published-copy model, friend-codes-only, isolated-island
  server — all untouched.
