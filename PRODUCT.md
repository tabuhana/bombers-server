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
- **Published content** — local content syncs up to the server as a server-side copy. This is both your cross-device backup AND the thing that makes sharing possible.
- **Sharing & permissions** — grant specific friends live read-access to specific published content.
- **Messaging** — real-time DMs.
- **Rooms** — ephemeral real-time spaces to chat, hang out, and play games.

The server does the heavy lifting for anything multi-user. The client never talks peer-to-peer; the server is always the hub.

## The official server vs. self-hosting

There is **one official Bombers server**, hosted by the project owner on a VPS. By default the client connects to it. Accounts are tied to the official server.

The code is open enough that someone *could* self-host their own private server, but:

- This is **not** an advertised or supported feature. Don't build onboarding, UI, or docs around it.
- Private servers are **fully disconnected islands.** No federation. No cross-server friends, sharing, or messaging. A private server shares nothing with the official one.
- If a user wants to move from the official server to their own, they migrate their data themselves. The project does not manage, assist, or care about that migration.
- There will be **other ways to connect to the server that are outside this project's scope.** Therefore the server must treat its API as the product and this client as just one consumer — never bake client-specific assumptions into the server.

For all build purposes: assume the official single server. Don't over-engineer for federation or multi-server scenarios.

## Mental model: local working copy + server published copy

The single most important concept. Internalize it.

- **Local is your working copy.** Source of truth for editing. Always available offline. Where you actually write and track.
- **Server is your published copy.** A synced-up mirror of everything *except* what you mark local-only. Two purposes at once:
  1. **Cross-device backup / restore** — wipe your laptop, log in on a new machine, pull your published content back down.
  2. **The sharing substrate** — anything on the server can be shared with friends. Nothing can be shared unless it's on the server.

"Published" does **not** mean public. It means "synced to your account on the server, where you *could* share it if you choose." By default nothing is shared with anyone.

### Publish rules

- **Auto-publish by default.** New content is queued and synced to the server on the next sync.
- **Local-only opt-out.** A global setting (default publish on/off) plus a per-note override. Anything marked local-only never leaves the device — and therefore can never be restored or shared. **The UI must clearly warn** that local-only content can't be restored or shared, at the moment the user marks it local-only.

### Sync behavior (be honest about what this is)

Sync is **triggered, not real-time mirroring.** The user understands this and it's fine.

- Sync runs: on user request ("Sync now"), and automatically on app load if it's been a while since the last sync.
- It is NOT live cross-device mirroring. Editing on your PC does not instantly appear on your laptop. You sync the PC (push), then sync the laptop (pull), and now they match.
- The UI must always show **"last synced X ago"** so the user knows how fresh their state is before they rely on it — especially before restoring on a new machine.
- Conflicts: same user owns both ends and edits are rarely simultaneous, so last-write-wins by `updated` timestamp is acceptable for v1. Flag conflicts to the user rather than silently clobbering when content has diverged. Details in `SERVER.md`.

## Sharing model: friends view your live server copy

When you share something with a friend, you grant them **live read-access to your server-side copy.** The server is authoritative for shared content.

- They see your content as it currently is on the server. When you edit and re-sync, their view updates.
- Revoke access and their window closes — they can no longer see it.
- They are *viewing*, not *forking*. No independent copy by default.
- **Optional "save a copy":** a friend can explicitly save shared content into their own vault to keep it permanently (survives you unsharing). Deliberate action, not the default. A saved copy becomes their content — their working copy, published to *their* account.

"Live" here means **as-of-last-sync live** (consistent with the sync model) — not sub-second realtime. Only **messaging and rooms** are true realtime.

This applies uniformly: shared note → friend sees your current published note; shared mood calendar → friend sees your mood as you log it; shared event → lands on friend's calendar.

**Exception — photos/memories copy instead of live-view.** Photos are local-first and heavy. Sharing a photo/album pushes a copy to the friend (temporary pass-through on the server, deleted after delivery) so "memories of each other" can live in both vaults. This is the one place the copy model beats live-view — you both want to *keep* the memory.

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
- They see it update as you log, as-of-last-sync.

### 6. Habit tracking

- Track habits/streaks.
- Shareable for view-access like mood (accountability with a friend).

### 7. Memories (photos)

- Per-friend photo albums. **Photos stored locally on the user's system** (not auto-published like notes — images are heavy and private).
- View like a photo book on a friend's profile.
- Sharing copies the photo/album to the friend via temporary server pass-through (deleted after delivery). Both vaults keep the memory. (S3-backed storage is a possible *later* upgrade for the pass-through; not now.)

### 8. Messaging

- Real-time DMs with friends (server-mediated, WebSocket).
- Genuinely live, unlike content sync.

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
- **Refactor:** the vault now syncs a *published copy* to the server. Add the publish/local-only concept. Add account/auth on launch. Replace any "anonymous backup daemon" notion with account-based sync to the Bombers server.
- **Add (new):** accounts/auth, friends + friend codes, profiles + profile cards, events, mood, habits, memories, messaging, rooms — and the entire Go server.
- **Cut:**
  - The standalone **sync daemon** concept (`SYNC_DAEMON.md` is obsolete — delete it). Sync is now a feature of the client talking to the authenticated server.
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
- ❌ Federation / cross-server anything. One official server; private servers are isolated islands.
- ❌ A global plugin marketplace or hosted plugins. Local/peer only.
- ❌ Persisting rooms after expiry. Ephemeral by design.
- ❌ OS-level notification spam. In-app ambient indicators only.
- ❌ Treating "live shared view" as sub-second realtime for notes/mood — it's as-of-last-sync. Only messages and rooms are true realtime.
- ❌ Building for millions of users. Friends-and-family scale.
- ❌ A web viewer in the client. The client is a client; it renders nothing for browsers.
- ❌ Throwing away the notebook work. This is a re-scope; the notebook is the foundation.
