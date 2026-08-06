# Setup

From a clean checkout to a working Skein with a file uploaded to your own
Google Drive.

Every command here was run as written before this page was published. Where a
step has a failure mode, the actual error message is quoted so you can match
what you see against what it means.

> **The binaries do not read `.env`.** `make run` sources it for you; running
> `./bin/skein` or `./bin/skein-desktop` directly does not. This trips people
> up, so the run commands below load it explicitly.

---

## 1. Prerequisites

| | Server | Desktop |
|---|---|---|
| Go 1.25+ | yes | yes |
| Git | yes | yes |
| Docker | yes (Postgres) | no (uses SQLite) |
| GTK3 + WebKitGTK 4.1 | no | yes |
| Wails v2.13 | no | yes |

Desktop build dependencies on Debian/Ubuntu:

```bash
sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev libsoup-3.0-dev
go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
```

`wails doctor` may report `webkit2gtk-4.0` missing. That is a false positive:
`make desktop` targets the 4.1 ABI via `-tags webkit2_41`.

---

## 2. Configuration

```bash
git clone https://github.com/mridul249/Skein.git
cd Skein
cp .env.example .env
```

**`cp .env.example .env` alone does not produce a working config.** Two values
ship empty and are required. Generate both:

```bash
openssl rand -base64 32   # -> SKEIN_MASTER_KEY
openssl rand -base64 48   # -> SKEIN_JWT_SECRET
```

Paste each into the matching line in `.env`.

If you skip `SKEIN_JWT_SECRET`, startup fails with:

```
skein: configuration: SKEIN_JWT_SECRET must be at least 32 characters
```

### What every variable does, and what happens without it

**Required — the process will not start:**

| Variable | Missing means |
|---|---|
| `SKEIN_MASTER_KEY` | `SKEIN_MASTER_KEY must decode to 32 bytes, got 0`. Encrypts every shard. **Lose it and your files are permanently unreadable.** |
| `SKEIN_JWT_SECRET` | `must be at least 32 characters`. Signs access tokens. Rotating it signs everyone out; it does not touch stored files. |
| `SKEIN_DATABASE_URL` | `SKEIN_DATABASE_URL is required` — **server only**. The desktop build uses SQLite and ignores it. |

**Required for Drive to work — the app starts without them, but connecting a
drive fails:**

| Variable | Missing means |
|---|---|
| `SKEIN_GOOGLE_CLIENT_ID` / `_SECRET` / `_REDIRECT_URL` | Server only. Drive endpoints return a clear error instead of a 500. |
| `SKEIN_GOOGLE_DESKTOP_CLIENT_ID` | Desktop only. "No Google client is configured for this app." |
| `SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET` | Desktop only. "No Google client secret is configured for this app." |

**Optional, but worth knowing:**

| Variable | Default | Effect |
|---|---|---|
| `SKEIN_BACKUP_TOKEN` | unset | Unset means `/api/system/backup` and the manifest repair actions **return 404** — the routes do not exist rather than being locked. Not needed on desktop. |
| `SKEIN_ROUTING_POLICY` | `most-available` | How shards spread over drives. See §6. |
| `SKEIN_DOWNLOAD_DIR` | XDG Downloads | Desktop "Save to disk" target. |
| `SKEIN_ENCRYPTION_ENABLED` | `true` | Turning it off leaves files written meanwhile in plaintext **permanently** — turning it back on does not re-encrypt them. |

---

## 3. Google OAuth

**The server and the desktop app need two different clients, and they are not
interchangeable.** Using the web client for desktop fails at token exchange
with `unauthorized_client`, which reads like a broken client rather than the
wrong type. This has already cost one live debugging session.

At <https://console.cloud.google.com> → **APIs & Services → Credentials**,
enable the **Google Drive API** first, then:

### Server — "Web application"

- Authorised redirect URI: exactly `SKEIN_GOOGLE_REDIRECT_URL`, default
  `http://localhost:8080/api/accounts/google/callback`. A trailing slash is a
  mismatch.
- Fill `SKEIN_GOOGLE_CLIENT_ID` and `SKEIN_GOOGLE_CLIENT_SECRET`.

### Desktop — "Desktop app"

- No redirect URI to configure; it uses a loopback port (RFC 8252 PKCE).
- Fill `SKEIN_GOOGLE_DESKTOP_CLIENT_ID` **and**
  `SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET`. Google requires a secret at token
  exchange even for Desktop-type clients, despite the client being public.

Skein requests **`drive.file` only**: it can see the files it created and
nothing else already in your Drive. While your OAuth consent screen is in
Testing, add your own address under **Audience → Test users** or consent fails.

More detail: [OAUTH.md](OAUTH.md).

---

## 4. Run

### Desktop

```bash
make desktop
set -a && source .env && set +a && ./bin/skein-desktop
```

### Server

```bash
make dev-db      # Postgres in Docker
make build
set -a && source .env && set +a && ./bin/skein
```

Then open <http://localhost:8080>.

`set -a` exports every variable the file sets; `set +a` stops that applying to
the rest of your shell. Without it the binary sees none of `.env` and fails as
though nothing were configured.

For development, `make run` does the same thing and prints which database it is
about to touch.

---

## 5. First use

1. **Register.** The first account is not special — registration is open, so on
   anything reachable from a network, set `SKEIN_BACKUP_TOKEN` and put Skein
   behind something that restricts who can reach it.
2. **Connect Google Drive** in Settings. Repeat for each account you want to
   pool.
3. **Upload.** Files larger than `SKEIN_SHARD_SIZE_BYTES` (256 MiB default) are
   split across drives.
4. **Download** one and confirm it opens. That round trip is what proves the
   install: it exercises encryption, striping, and reassembly together.

### Verify your setup is recoverable

Settings → **Recovery** should show your master key ID and full manifest
coverage. If coverage is not full, click **Write missing manifests**.

Manifests are what make a lost database survivable. Without them, the shards in
your Drive are unlabelled encrypted blobs.

---

## 6. How files are placed

`SKEIN_ROUTING_POLICY` controls which drive each shard goes to:

- **`most-available`** (default) — emptiest drive first. A striped file also
  prefers a drive not already holding one of its shards, so a two-shard file
  lands on two drives rather than filling one.
- **`priority`** — connection order; fills the first drive until it is full.
- **`round-robin`** — rotates per upload, for per-drive request quotas.

---

## 7. Backups

**The master key alone cannot restore your files, and neither can the database
alone.** The key decrypts shard contents; the database records which shard
belongs to which file and in what order. You need both.

Skein has a third layer that covers losing the database entirely: **sidecar
manifests**, written next to your shards in Drive. Recovery reads them back and
rebuilds the file list from the drives alone. Verified end to end — database
moved aside, account re-registered, drives reconnected, files restored and
downloaded byte-for-byte.

What to keep, and where:

| | Holds | Keep it |
|---|---|---|
| `SKEIN_MASTER_KEY` | decryption | Password manager. **Not** on the same disk as the database. |
| `make backup` dump | shard→file mapping | Anywhere the key is not. |
| Manifests | shard→file mapping, again | Automatic, in your Drive. |

Full procedure, including restore: [BACKUP.md](BACKUP.md).

---

## Troubleshooting

| Symptom | Cause |
|---|---|
| `SKEIN_JWT_SECRET must be at least 32 characters` | Copied `.env.example` without filling it in. §2. |
| `SKEIN_MASTER_KEY must decode to 32 bytes, got 0` | Same. |
| `dial tcp 127.0.0.1:5433: connect: connection refused` | `make dev-db` not run, or Docker is not up. Server only. |
| `unauthorized_client` on connecting a drive | Web client used for the desktop build. §3. |
| "No Google client secret is configured for this app." | `SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET` unset. §3. |
| Everything starts, but nothing is configured | `.env` not loaded — the binaries do not read it. §4. |
| Backup or manifest repair returns 404 | `SKEIN_BACKUP_TOKEN` unset. That is by design. |

---

## Where to go next

| | |
|---|---|
| [INSTALL.md](INSTALL.md) | Shorter install, no explanation |
| [OAUTH.md](OAUTH.md) | Google client setup in detail |
| [BACKUP.md](BACKUP.md) | Backup and restore procedure |
| [CONFIGURATION.md](CONFIGURATION.md) | Every setting |
| [SECURITY.md](SECURITY.md) | Threat model |
| [DEVELOPMENT.md](DEVELOPMENT.md) | Building and testing |
