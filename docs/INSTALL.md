# Install

Two binaries, two setups. Pick the one you actually want before you start:

| | `skein serve` (headless) | `skein-desktop` (native window) |
|---|---|---|
| Runs where | a server, container, VPS | your own machine |
| Database | PostgreSQL — you run it | PostgreSQL — you still run it (see the note below) |
| Google OAuth setup | you create a Cloud Console client | **zero Console steps** |
| Build | `CGO_ENABLED=0`, static, cross-compiles | requires cgo, built per-platform |

**The desktop build talks to PostgreSQL too.** It does not embed a database of
its own yet — that lands after an owner-written rewrite of the shard router,
tracked in `PLAN/Phase7.md`. If you were expecting a single file with nothing
else to run, that is not this release; point `SKEIN_DATABASE_URL` at a
Postgres exactly as you would for the server build. See
[docs/ARCHITECTURE.md](ARCHITECTURE.md) for why.

---

## 1. Prerequisites

- Go 1.25 or later (`go version`)
- PostgreSQL 15+, reachable from wherever you run Skein
- Node 18+ and npm, to build the frontend once (`make web`)
- For `skein-desktop` only: see §4 below

## 2. Clone and configure

```bash
git clone <this repo> && cd skein
cp .env.example .env
```

Generate the two secrets `.env` needs:

```bash
openssl rand -base64 32   # -> SKEIN_MASTER_KEY
openssl rand -base64 48   # -> SKEIN_JWT_SECRET
```

Paste both into `.env`. **`SKEIN_MASTER_KEY` decrypts every file you ever
upload — read [docs/BACKUP.md](BACKUP.md) before you store anything you'd
miss.**

## 3. Database

Point `SKEIN_DATABASE_URL` at a running Postgres. For local development,
`docker-compose.yml` gives you one:

```bash
make dev-db   # postgres on 127.0.0.1:5433, matches .env.example
```

Migrations run automatically on boot (`db.Migrate`, embedded in the binary
via `goose`) — there is no separate migrate step for a fresh install. `make
migrate` / `make migrate-status` exist for operating on a database the
binary isn't currently running against.

## 4. Connecting a Google Drive

**This is the one place the two builds genuinely differ**, and it's the
reason the desktop build exists.

### Server build (`skein serve`)

You supply your own OAuth client — there is no shared one, by design
(`drive.file` scope only, so Skein can never see files it didn't create):

1. [console.cloud.google.com](https://console.cloud.google.com) → new project
   (or reuse one) → **APIs & Services → Library** → enable the **Google
   Drive API**.
2. **APIs & Services → OAuth consent screen** → External → fill in the
   required fields. You do not need Google's verification review for
   personal use; an unverified app works for accounts you explicitly add as
   test users.
3. **APIs & Services → Credentials → Create Credentials → OAuth client ID**
   → application type **Web application**.
4. **Authorised redirect URI** must match `SKEIN_GOOGLE_REDIRECT_URL`
   exactly, including the path: `http://localhost:8080/api/accounts/google/callback`
   for the default `.env.example`, or your real `SKEIN_PUBLIC_URL` +
   `/api/accounts/google/callback` in production.
5. Copy the client ID and client secret into `.env`:
   `SKEIN_GOOGLE_CLIENT_ID`, `SKEIN_GOOGLE_CLIENT_SECRET`.
6. Start Skein, sign in, **Settings → Connect a drive**. The browser
   navigates to Google's consent screen and back — nothing else to
   configure.

### Desktop build (`skein-desktop`)

No Console steps. The desktop binary is built as a **Desktop app** (RFC
8252) OAuth client — a client type Google does not require a secret for —
and Skein ships with a working one compiled in.

**Connect a drive → Settings → Connect a drive** opens your system's
default browser (not the app window — see
[docs/SECURITY.md](SECURITY.md#why-the-system-browser) for why), you sign
in and grant access, and the tab tells you to close it. The desktop app
picks up the connection automatically.

If you want your own client instead of Skein's shared one — for your own
Drive API quota, or because you don't want to trust a shared client —
create a **Desktop app** OAuth client the same way as steps 1–3 above (type
**Desktop app**, not Web application; no redirect URI to register — Skein's
loopback listener supplies one per attempt), then set
`SKEIN_GOOGLE_DESKTOP_CLIENT_ID` in your environment before launching
`skein-desktop`. No client secret field exists for this client type; if a
flow ever appears to want one, the client was registered as the wrong type.

## 5. Build and run

### Server

```bash
make web && make build
./bin/skein
```

`make build` builds the frontend first — `make build-go` skips that step if
`internal/web/dist` is already populated.

### Desktop

```bash
sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev libsoup-3.0-dev
go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
make desktop
./bin/skein-desktop
```

`wails doctor` may report `libwebkit: Not Found` even with all three
packages installed — it checks for the package name `webkit2gtk-4.0`, which
recent Ubuntu releases (26.04, verified) do not ship; only `4.1` does. This
is a false positive. `make desktop` already builds with the correct tag
(`-tags webkit2_41`) regardless of what `wails doctor` says — do not chase
that warning.

## 6. First run

Both builds land on the same login screen — registration is open, there is
no invite gate. Register, sign in, connect a drive (§4), and you're pooling
storage. `docs/CONFIGURATION.md` covers every environment variable if you
need to tune limits, ports, or proxy trust before going further.
