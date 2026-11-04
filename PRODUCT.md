# Bombers — Product Vision

This is the master product document for **Bombers**. Read it before any architecture or code work. It describes *what we're building and why*. Technical specs (`CLAUDE.md`, `SERVER.md`) describe *how* — and must stay consistent with this document. If a technical decision contradicts this vision, this document wins.

Everything in this document is **decided**. Do not re-litigate the product direction or ask the user to reconsider the social model, the sharing model, or the client/server split. Build to this.

## One-line pitch

Bombers is what you'd get if Discord, Notion, and Obsidian had a baby: a local-first personal notebook organized around the *people* in your life, with a social layer for sharing, messaging, and hanging out in real time.

## The core idea

Most note apps organize around documents. Bombers organizes around **people**. The original seed: keeping notes and reminders *about your friends* — what they like, what's going on with them, things you want to remember. That grew into a full personal notebook that also tracks moods, habits, and important dates — and lets you selectively share any of it with the specific friends you choose.

It's a private notebook first. It's a social app second. The social part only ever touches what you explicitly allow.

## The two halves

Bombers is a **standalone client** plus a **server**. Separate codebases, clear boundary.

### Bombers Client (Tauri + React + TypeScript)

The desktop app. Local-first. Everything you create lives on your machine as the working copy — your vault of Markdown notes, mood history, habits, friend profiles, photos. Almost the entire app works offline. The notebook foundation is already substantially built (see "Current state").

The client is **only** a client. It has no web viewer, renders nothing for browsers, and is just one consumer of the server's API.

### Bombers Server (Go)

A real backend that handles everything social and everything that must outlive a single device:

- **Accounts & auth** — you create an account on first launch.
- **Friends** — friend-code based (Nintendo style), no public discovery.
- **Sharing & permissions** — grant specific friends live read-access to specific published content.
- **Messaging** — real-time DMs.
- **Rooms** — ephemeral real-time spaces to chat, hang out, and play games.

The server does the heavy lifting for anything multi-user. The client never talks peer-to-peer; the server is always the hub.

## Who this is for

**The owner and his friends.** Not a product, not a platform, not something
courting users. The repository is public and nobody is stopped from running it,
but strangers are not an audience to design for — if a decision is a trade
between "better for us" and "better for a hypothetical self-hoster", it goes to
us every time.

That matters most in what it stops: no onboarding funnels for people who don't
exist, no documentation written to persuade, no feature justified by adoption.
Scale is tens of users on one machine, permanently.

## The official server vs. self-hosting

There is **one official Bombers server**, hosted by the project owner on a VPS. By default the client connects to it.

**Self-hosting works — it just isn't a goal.** The server picker, the setup
wizard and the whole `bombers` CLI exist and function, because the owner uses
them himself to run his own instance. What changed (2026-08-06) is that making
that experience good *for other people* stopped being something to invest in.
Don't remove the capability; don't build for an audience of self-hosters either.
Every server — official or private — is a **fully disconnected island**:

- **No federation. No cross-server anything.** No cross-server friends, sharing, or messaging. Accounts, friend codes, and content exist only on the server that created them; a session/token from one server means nothing to another.
- Moving between servers means registering fresh on the new one. The project does not migrate accounts or content between servers.
- There will be **other ways to connect to the server that are outside this project's scope.** Therefore the server must treat its API as the product and this client as just one consumer — never bake client-specific assumptions into the server.

For all build purposes: design for ONE server (one binary, one Postgres, one circle of users). "Multi-server" only ever means separate islands running the same code — don't over-engineer for federation or cross-server scenarios.

## Mental model: the notebook is local, the server is social

The single most important concept. Internalize it.

- **The notebook is the user's and it is on their machine.** Notes, moods,
  events, todos, every node's data, the themes they made — all of it lives in
  the data root. Nothing copies it anywhere.
- **The server is the social half.** Accounts, friends, DMs, rooms, presence,
  profiles and the facts on person cards, the store, release publishing, and the
  signalling that lets two machines send each other a file. It will take on more
  jobs over time — each one a thing decided and built, not a mirror of the
  notebook that reappears.
- **There is no backup** (2026-11-03). The publish/pull engine was deleted on
  both halves. Lose the machine, lose the notebook — that is the honest state of
  the app, and nothing may imply otherwise.

## Sharing model: facts, not documents

What a friend can see is what has deliberately been put on the social half: the
profile and its facts, what is said in DMs and rooms, what a node puts in front
of named friends, and what is published to the store. Notes are not shared
(removed 2026-09-14) and there is no server copy of a vault to grant access to.

## The pillars

### 1. Notebook (largely built)

The Obsidian-class Markdown notebook: vault of `.md` files, CodeMirror editor, live preview, wikilinks, backlinks, graph, search, tags, templates, revision history, themes, plugins. Being rebuilt from scratch as the **Editor** (a core screen in the client).

### 2. Friends

People are first-class entities.

- Add friends via **friend code** (generated per account, shared out-of-band — you text it to someone). No username search, no discovery, no "people you may know." Privacy by default.
- Friends list lives on the server (inherently social).
- Each friend has a **profile** you open to see everything about your relationship with them.

### 3. Profile cards (the signature feature)

A friend's profile shows small structured facts — NOT editor notes, but little cards/tidbits. **Two distinct kinds**, different owners:

- **Notes you keep about them (private to you by default):** "favorite food: pizza", "anniversary: June 3", "allergic to shellfish". You author these. You *can* choose to share specific ones back to them or to others.
- **Things they publish about themselves:** "loves strawberry boba", "currently reading Dune". *They* author these on their own profile and set visibility — broadcast to all friends, or only specific friends. When you open their profile you see the ones they've made visible to you.

So a profile for, say, your girlfriend shows: the private notes you keep about her + the tidbits she's shared with you + your shared memories (photos) + entry points to her shared mood/calendar if she's granted access.

### 4. Events / dates

- Create an event, invite specific friends.
- Invited friends get an in-app notification.
- If they accept, the event lands on **their** calendar.
- Calendar is shareable for view-access like everything else.

### 5. Mood tracking

- Log your mood over time (a mood calendar/history).
- Grant specific friends live view-access ("share how I've been feeling with my girlfriend").
- Sharing a mood calendar is not built: there is no server copy of it to grant.

### 6. Habit tracking

- Track habits/streaks.
- Shareable for view-access like mood (accountability with a friend).

### 7. Memories (photos)

- Per-friend photo albums. **Photos stored locally on the user's system** (not auto-published like notes — images are heavy and private).
- View like a photo book on a friend's profile.
- Sharing copies the photo/album to the friend via temporary server pass-through (deleted after delivery). Both vaults keep the memory. (S3-backed storage is a possible *later* upgrade for the pass-through; not now.)

### 8. Messaging

- Real-time DMs with friends (server-mediated, WebSocket).
- Genuinely live — messaging and rooms are the only realtime surfaces.

### 9. Rooms

- Ephemeral real-time spaces, TeamSpeak-style.
- Gather friends to chat, interact, play games together.
- **Non-permanent:** when everyone leaves, the room expires and everything in it is gone. Nothing persists after expiry. No save-to-vault for rooms.

### 10. Full customization

- Style, fonts, themes — fully user-customizable.
- **User-built plugins in TypeScript** — write your own, use them yourself, or share with friends. Plugins are NEVER globally hosted or published to a marketplace. Local or peer-shared only (sharing a plugin uses the same share path as anything else).

## Notifications — keep it chill

Ambient and quiet, Discord-style. Explicitly NOT spammy.

- **In-app indicators only.** A small icon on the dashboard with a badge — a count or a simple dot — when there's something new (a message, an event invite, a friend's shared update).
- **No OS-level / Windows toast notifications.** Do not fire system notifications. Do not interrupt the user. The indicator lives inside the app and waits to be noticed.
- The goal is a calm app that surfaces things without nagging.

## Current state (what's built)

The notebook module is well underway. As of last status:

- **Complete:** vault + read (picker, file tree, watcher), write + edit (CodeMirror, debounced save, CRUD, frontmatter UI), history + safety (revisions, diff, trash).
- **Partial:** rendering (GFM/math/mermaid/code done; callouts, footnotes, richer embeds pending), links + graph (most in; heading/block targeting pending), search (FTS5 + quick switcher in; full operators + vault-wide replace pending), organization (tags, status+kanban, table view, templates in; bases/queries + favorites pending), editor polish (tabs, command palette, outline, vim toggle, hotkeys in; split panes, extra keymaps, spellcheck, distraction-free pending), extensibility (theme/snippet loading, plugin API + one built-in plugin in; on-disk plugin loading pending).
- **Not started:** the social layer (everything in pillars 2-9 above) and the Go server.

This is a **re-scope, not a restart.** The notebook is the foundation and stays.

## What survives / changes / is cut

- **Keep:** the entire notebook foundation — vault model, Markdown files on disk, editor, rendering, links/graph, search, tags, templates, revisions, themes, plugin system, local-first editing.
- **Refactor:** add account/auth on launch. (The *published copy* and publish/local-only concepts were built and then deleted on 2026-11-03 — see §Mental model.)
- **Add (new):** accounts/auth, friends + friend codes, profiles + profile cards, events, mood, habits, memories, messaging, rooms — and the entire Go server.
- **Cut:**
  - The standalone **sync daemon** concept (`SYNC_DAEMON.md` is obsolete — delete it), and with it, since 2026-11-03, sync itself.
  - The **`VaultStorage` abstraction** (never needed).
  - The **static-site publisher** sibling project (no web viewer; the server handles sharing; a static generator no longer fits the product).
  - **MCP server / AI panel** — parked indefinitely in the backlog. Not part of the current product. Don't build it; don't ask about it.

## Client / server boundary

**Client owns:** local vault and all local editing, local mood/habit/event data, local photo storage, rendering, plugins, themes, the entire offline experience. Queues changes for sync. Holds an auth token for the server.

**Server owns:** identity/accounts, friend graph, published copies of content, sharing permissions (who can see what), message delivery, room lifecycle + real-time transport, notification fan-out (for in-app indicators).

**The contract:** an authenticated API. HTTP for sync, publishing, sharing, friends, profiles, events. WebSocket for real-time (messages, rooms, presence, live indicators). Detailed in `SERVER.md`.

## Scope reminder

Still **the owner's personal app + a few friends onboarded manually.** Not a public product.

- No public sign-up funnels, discovery, growth features, or moderation systems.
- Server runs on the owner's VPS; friends connect to it.
- Friend codes are enough; no elaborate identity.
- Build for tens of users, not millions. Don't over-engineer for scale that won't happen.
- Security still matters (multi-user, private data) but auth and infra stay simple.

## Build roadmap (high level — detailed phases live in the technical specs)

1. **Stabilize the notebook** as-is (mostly built). Solid local-first editing.
2. **Go server skeleton:** accounts, auth, friend codes, friend graph.
3. **Client ↔ server sync:** publish local content up, restore down, "last synced" UI, local-only flag.
4. **Sharing & permissions:** grant/revoke friend access; friend's live read-view.
5. **Profiles & profile cards:** the two-sided fact system.
6. **Mood + habits + events**, with sharing.
7. **Memories** (local photos + share-as-copy via server pass-through).
8. **Messaging** (real-time).
9. **Rooms** (ephemeral real-time + games).
10. **Customization polish:** plugin-sharing, theme-sharing.

## What to refuse / push back on (vision-level)

- ❌ Making the server authoritative over your *working* copy. Local is where you edit; server is the published mirror. Don't invert this.
- ❌ Auto-sharing anything. Sharing is always explicit and per-friend.
- ❌ Public discovery, username search, friend suggestions. Friend codes only.
- ❌ Federation / cross-server anything. Every server — official or self-hosted — is an isolated island.
- ❌ A global plugin marketplace or hosted plugins. Local/peer only.
- ❌ Persisting rooms after expiry. Ephemeral by design.
- ❌ OS-level notification spam. In-app ambient indicators only.
- ❌ Treating "live shared view" as sub-second realtime for notes/mood — it's as-of-last-sync. Only messages and rooms are true realtime.
- ❌ Building for millions of users. Friends-and-family scale.
- ❌ A web viewer in the client. The client is a client; it renders nothing for browsers.
- ❌ Throwing away the notebook work. This is a re-scope; the notebook is the foundation.
