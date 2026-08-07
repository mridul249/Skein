# Docker

Three commands to a running Skein, with Docker as the only dependency.

```bash
git clone https://github.com/mridul249/Skein.git && cd Skein
cp .env.example .env      # set the two Google OAuth values
docker compose up
```

Then open <http://localhost:8080> and read `data/skein-setup-info.txt`.

**OAuth credentials are the only thing you must supply.** They cannot be
generated. Everything else - the JWT secret, the master key, the database,
the migrations - is created for you and reported in that file.

---

## What happens on first run

1. Postgres starts and the server waits for it to report healthy. Starting
   before Postgres accepts connections would make migrations fail and read as
   a broken image.
2. Migrations run.
3. If `SKEIN_MASTER_KEY` or `SKEIN_JWT_SECRET` are unset, they are generated
   and written to `data/skein-setup-info.txt` (mode 0600), and a banner is
   printed to the logs.
4. The server listens on 8080.

### The master key

If Skein generated the key, `data/skein-setup-info.txt` is **the only copy**.
Lose it and every file you have uploaded becomes permanently unreadable - there
is no reset and no recovery.

Copy it into a password manager, then move or delete the file. It currently
sits beside your Postgres volume, which is the one place it should not stay: a
single lost disk would take both the key and the database.

The file is gitignored and written 0600, but that is not a substitute for
moving it.

**Restarting does not regenerate it.** The entrypoint reads the key back from
the setup file before generating, so `docker compose down && docker compose up`
keeps your data readable. If `/data` is not writable, the container refuses to
start rather than run with a key that exists only inside it.

To supply your own instead, put them in `.env`:

```bash
openssl rand -base64 32   # SKEIN_MASTER_KEY
openssl rand -base64 48   # SKEIN_JWT_SECRET
```

---

## Where your data lives

| | Where | Back it up with |
|---|---|---|
| Database | Docker volume `skein-pgdata` | `docker compose exec -T postgres pg_dump -U skein skein \| gzip > backup.sql.gz` |
| Master key | `data/skein-setup-info.txt` | Password manager |
| File contents | Your Google Drive | Already there |

```bash
docker volume inspect skein-pgdata   # find it on disk
```

**The key alone cannot restore your files, and neither can the database
alone.** The key decrypts shard contents; the database records which shard
belongs to which file. Keep both, in different places. See
[BACKUP.md](BACKUP.md).

---

## Common adjustments

All via `.env` or the environment:

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_PORT` | `8080` | Host port |
| `SKEIN_PUBLIC_URL` | `http://localhost:8080` | Must match your OAuth redirect URI |
| `SKEIN_ENV` | `development` | `production` requires an **https** public URL: refresh cookies are Secure-only |
| `SKEIN_UID` / `SKEIN_GID` | `1000` | The container runs as this uid so `./data` is writable and readable by you |
| `POSTGRES_PASSWORD` | `skein` | Change it if the host is not single-user |
| `SKEIN_BACKUP_TOKEN` | unset | Unset means the backup and manifest-repair routes return 404 |

Postgres is **not** published to the host. Inspect it with:

```bash
docker compose exec postgres psql -U skein -d skein
```

### Behind a TLS proxy

```bash
SKEIN_ENV=production
SKEIN_PUBLIC_URL=https://skein.example.com
SKEIN_GOOGLE_REDIRECT_URL=https://skein.example.com/api/accounts/google/callback
SKEIN_TRUSTED_PROXIES=172.16.0.0/12
```

The redirect URI must match what you registered in Google Cloud exactly.

---

## Building the desktop binary

**The desktop app is not containerised and cannot run in one.** `skein-desktop`
is a windowed Wails application; a container has no display, and giving it one
helps nobody.

The `desktop` target is a **build** target: it produces a binary for your host
so you can build it without installing Go, Node, GTK and WebKit yourself.

```bash
docker build --target desktop --output type=local,dest=./bin .
./bin/skein-desktop
```

That writes `bin/skein-desktop` and nothing else. It is a Linux binary; on
macOS or Windows, build natively with `make desktop`.

The desktop build needs its own **Desktop app** OAuth client, which is
different from the server's Web client and not interchangeable. See
[SETUP.md](SETUP.md) §3.

---

## Reproducible builds

The image build carries the same flags as the release build - `-trimpath` and
`-buildvcs=false` - so the binary inside the image is byte-identical to one
built from the same commit elsewhere. Without `-buildvcs=false`, Go stamps
`vcs.modified`, which differs between a clean checkout and a dirty working
tree; that was the actual cause of two "identical" builds differing, measured
2026-08-06.

The frontend is built **inside** the image rather than copied from the host.
`internal/web/dist` is gitignored, so copying it would either embed whatever
happened to be in your working tree or embed nothing at all - producing a
server that starts cleanly and serves no UI, silently.

---

## Troubleshooting

| Symptom | Cause |
|---|---|
| `FATAL: /data is not writable` | The bind mount is root-owned. `mkdir -p data` before `docker compose up`, or set `SKEIN_UID`/`SKEIN_GID`. Refusing to start is deliberate: a generated key that exists only inside the container is a guaranteed future data loss. |
| `SKEIN_PUBLIC_URL must be https in production` | `SKEIN_ENV=production` with an http URL. Either use `development` locally or put Skein behind TLS. |
| `No drive is connected` on upload | Expected until you connect a Google Drive in Settings. |
| `unauthorized_client` when connecting a drive | The Web OAuth client was used for the desktop build, or vice versa. [SETUP.md](SETUP.md) §3. |
| Container restarts repeatedly | `docker compose logs skein`. The entrypoint prints the reason before exiting. |

More: [TROUBLESHOOTING.md](TROUBLESHOOTING.md) for exact error strings, and
[SETUP.md](SETUP.md) for the non-Docker paths and the full list of settings.
