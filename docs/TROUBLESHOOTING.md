# Troubleshooting

Exact error strings, so they are searchable. If you hit something not here,
`docker compose logs skein` or the server's own startup output usually names
the cause directly - Skein tries to fail loudly rather than subtly.

---

## Docker

### `exec /usr/local/bin/docker-entrypoint.sh: no such file or directory`

Also seen as:

```
skein-1 exited with code 255
skein-1  | exec /usr/local/bin/docker-entrypoint.sh: no such file or directory
```

with the container restarting in a loop.

**The script is not missing. The interpreter is.** This happens when the
repository was cloned on Windows with `core.autocrlf=true` (Git for Windows'
default), which rewrites text files to CRLF on checkout. The entrypoint's first
line then reads `#!/bin/sh\r`, and the kernel looks for an interpreter literally
named `/bin/sh\r`, which does not exist. Docker reports the path of the
*script*, not of the missing interpreter, which is what makes this cost an hour.

Confirm it:

```bash
docker run --rm --entrypoint /bin/sh <image> -c \
  "head -c 12 /usr/local/bin/docker-entrypoint.sh | od -c"
```

Broken shows `# ! / b i n / s h \r \n`. Correct shows `# ! / b i n / s h \n`.

**Fixed in the repository as of the `.gitattributes` commit** - `* text=auto
eol=lf` forces LF on checkout regardless of platform, and the Dockerfile also
runs `sed -i 's/\r$//'` on the entrypoint before `chmod`, so an image builds
correctly even from a clone whose local git config overrides `.gitattributes`.

If you cloned before that and are still hitting it:

```bash
git rm --cached -r . && git reset --hard    # re-checkout with the new rules
docker compose build --no-cache
```

### `Found multiple config files with supported names: compose.yaml, docker-compose.yml`

```
Found multiple config files with supported names: compose.yaml, docker-compose.yml
Using compose.yaml
```

**Fixed** - the development file is now `compose.dev.yaml`, which Docker does
not auto-discover, so there is nothing to disambiguate. If you still see this,
you have a stale `docker-compose.yml`; delete it.

It mattered more than a warning suggests: the two files were different, and
Docker silently used `compose.yaml` while `make dev-db` was written expecting
the other. A contributor editing `docker-compose.yml` was editing a file
nothing read.

### `FATAL: /data is not writable`

The `./data` bind mount was created by the Docker daemon as root, and the
container runs as a non-root user. Either:

```bash
mkdir -p data          # create it yourself, before `docker compose up`
```

or set `SKEIN_UID`/`SKEIN_GID` in `.env` to your own ids.

Refusing to start is deliberate. A generated master key that exists only inside
a container is guaranteed future data loss, so Skein will not run when it
cannot record one.

### `SKEIN_PUBLIC_URL must be https in production`

`SKEIN_ENV=production` with an `http://` URL. Refresh cookies are `Secure`-only,
so production requires TLS. Use `SKEIN_ENV=development` locally, or put Skein
behind a TLS proxy and set both `SKEIN_PUBLIC_URL` and
`SKEIN_GOOGLE_REDIRECT_URL` to the `https://` address.

---

## Running from source

### `dial tcp 127.0.0.1:5433: connect: connection refused`

The development Postgres is not running. Start it:

```bash
make dev-db
```

It publishes `127.0.0.1:5433`, matching `SKEIN_DATABASE_URL` in `.env.example`.
Note that `make dev-db` uses `compose.dev.yaml`, not `compose.yaml` - the
production stack keeps Postgres unpublished and on its own volume.

### The binary ignores `.env`

`./bin/skein` does not read `.env`; only `make run` does. Load it explicitly:

```bash
set -a && source .env && set +a && ./bin/skein
```

**That line is Linux and macOS only.** `set -a` and `source` are POSIX shell
builtins; PowerShell has neither, and pasting it there fails with
`The term 'set -a' is not recognized`. The equivalent, which parses `KEY=value`
lines and skips comments and blanks:

```powershell
Get-Content .env | Where-Object { $_ -match '^\s*[^#\s]' } | ForEach-Object {
    $name, $value = $_ -split '=', 2
    [Environment]::SetEnvironmentVariable($name.Trim(), $value.Trim())
}
.\skein.exe
```

This sets the variables for the current PowerShell session only, which matches
what `set -a` does for a shell. In `cmd.exe`, use
`for /f "delims=" %i in (.env) do set %i` - though note it does not skip
comment lines, so strip them first.

The **desktop app reads no `.env` at all**, on any platform - it takes
`SKEIN_GOOGLE_DESKTOP_CLIENT_ID` and `_SECRET` from the environment, or uses
credentials compiled in at build time. On Windows, set them through
System Properties → Environment Variables if you want them to persist across
reboots, rather than per-session as above.

### `SKEIN_JWT_SECRET must be at least 32 characters`

`cp .env.example .env` leaves both secrets empty. Generate them:

```bash
openssl rand -base64 32   # SKEIN_MASTER_KEY
openssl rand -base64 48   # SKEIN_JWT_SECRET
```

---

## Windows desktop app

The Windows binary is cross-compiled from Linux and tested on Windows 11.
Windows 10 is not tested. If you hit something not listed here, it is worth
reporting rather than assuming it is your setup.

### The v1.0.0-rc1 binary opens a Wails error dialog and exits

"Wails applications will not build without the correct build tags." That build
was made without the `production` build tag, so it linked Wails' own guard stub
instead of the application. It failed this way for every user on every launch -
there is no workaround and nothing wrong with your setup. Download v1.0.0-rc2
or later.

### Nothing happens, or "WebView2 Runtime not installed"

Skein draws its entire interface in WebView2, so without the runtime there is
no window to show and the process may exit with no visible error. Windows 11
bundles it; **Windows 10 often does not**.

Install the Evergreen Runtime from
<https://developer.microsoft.com/microsoft-edge/webview2/> and run Skein again.

### "Windows protected your PC" (SmartScreen)

```
Microsoft Defender SmartScreen prevented an unrecognized app from starting.
```

The binary is not code-signed. Verify the checksum first, then **More info** →
**Run anyway**:

```powershell
Get-FileHash .\skein-desktop-v1.0.0-rc1-windows-amd64.exe -Algorithm SHA256
```

Compare against `SHA256SUMS` from the release. If it does not match, do not run
it. A signing certificate is a recurring cost this project does not currently
carry; the checksum and the Sigstore signature over `SHA256SUMS` are what
establish provenance instead.

### A console window opens behind the app

It should not: the release binary is linked with `-H=windowsgui`, and the build
fails if the PE subsystem field is not `2` (GUI). If you see one, you are
probably running a self-built binary without that flag. Report it if it came
from a release.

### Where the database lives

`%AppData%\skein\skein.db` - that is
`C:\Users\<you>\AppData\Roaming\skein\skein.db`. Downloads land in
`%USERPROFILE%\Downloads`.

---

## Google Drive

### `unauthorized_client` when connecting a drive

The server and the desktop app need **different** OAuth clients, and they are
not interchangeable:

- Server: a **Web application** client, with a redirect URI.
- Desktop: a **Desktop app** client, using a loopback port (RFC 8252 PKCE).

Using one for the other fails at token exchange with this error, which reads
like a broken client rather than the wrong type. See [SETUP.md](SETUP.md) §3.

### `No Google client secret is configured for this app.`

`SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET` is unset. Google requires a secret at
token exchange even for Desktop-type clients, despite the client being public.

### `No drive is connected` on upload

Expected until you connect a Google Drive in Settings. Uploads need somewhere
to put shards.

---

## More

- [SETUP.md](SETUP.md) - full setup, every variable, both OAuth client types
- [DOCKER.md](DOCKER.md) - Docker specifics, volumes, TLS
- [BACKUP.md](BACKUP.md) - backup and recovery procedure
