# Configuration

Every setting is an environment variable, loaded once at boot
(`internal/config/config.go`) and validated fail-fast — a bad value stops
the process before it listens, never a runtime surprise. `.env` is read in
development; in production, set these however your deployment does.

Both `skein serve` and `skein-desktop` read the same variables. The desktop
build additionally reads `SKEIN_GOOGLE_DESKTOP_CLIENT_ID` and
`SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET` (§6).

## Core

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_ENV` | `development` | `development`, `test`, or `production`. Gates `SKEIN_PUBLIC_URL`'s https requirement (§4). |
| `SKEIN_ADDR` | `:8080` | Listen address. Ignored by `skein-desktop`, which always binds `127.0.0.1:0` and picks its own port. |
| `SKEIN_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `SKEIN_LOG_JSON` | `false` | Structured JSON logs. Set `true` in production. |

## Database

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_DATABASE_URL` | *(required)* | PostgreSQL connection string. Migrations run automatically on boot. |

## Secrets

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_MASTER_KEY` | *(required)* | Base64, must decode to exactly 32 bytes: `openssl rand -base64 32`. Every stored file's key derives from this via HKDF-SHA256. Losing it makes every file permanently unreadable — see [BACKUP.md](BACKUP.md). |
| `SKEIN_JWT_SECRET` | *(required)* | Signs access tokens. At least 32 characters. Independent of the master key so either can rotate without the other. |

## Sessions

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_ACCESS_TOKEN_TTL` | `15m` | Must be `> 0` and `<= 1h`. |
| `SKEIN_REFRESH_TOKEN_TTL` | `720h` (30 days) | Must exceed the access token TTL. |

## Google OAuth — server build

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_GOOGLE_CLIENT_ID` | *(empty)* | From your own Cloud Console **Web application** client. See [INSTALL.md](INSTALL.md#4-connecting-a-google-drive). |
| `SKEIN_GOOGLE_CLIENT_SECRET` | *(empty)* | Paired with the client ID above. |
| `SKEIN_GOOGLE_REDIRECT_URL` | *(empty)* | Must match the Console's **Authorised redirect URI** exactly, including path. |

All three empty means Drive connection is unavailable and `BeginGoogleConnect`
returns a clear error naming the missing variable — not a panic, not a silent
no-op.

## Public URL and cookies

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_PUBLIC_URL` | `http://localhost:8080` | Used to build the OAuth redirect and to decide whether refresh cookies get `Secure`. |

**The `Secure` cookie flag is set when either `SKEIN_ENV=production` or
`SKEIN_PUBLIC_URL` starts with `https`.** In `production` with a plain-`http`
`SKEIN_PUBLIC_URL`, config validation refuses to boot — `Secure` cookies over
plain HTTP would be silently dropped by the browser, which is worse than
refusing to start. The desktop build serves plain HTTP on loopback
deliberately (§6) and is not subject to this check because it never sets
`SKEIN_ENV=production` with a non-loopback `SKEIN_PUBLIC_URL`.

## Previews, proxies, CORS

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_PREVIEW_ORIGIN` | *(empty)* | When set, inline previews are served from this separate origin instead of the API's own, so a malicious upload's script content can't reach the app's cookies even if the content-type allowlist were ever bypassed. Strongly recommended in production. |
| `SKEIN_TRUSTED_PROXIES` | *(empty)* | Comma-separated CIDRs. `X-Forwarded-For` is honoured **only** from these peers — empty means every request's `RemoteAddr` is used as-is. Getting this wrong lets a client forge its own IP into audit logs. |
| `SKEIN_CORS_ORIGINS` | *(empty)* | Comma-separated origins allowed to call the API with credentials. Leave empty when the UI is served from the same origin as the API — the normal case for both builds, since each embeds its own frontend. |

## Upload limits

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_MAX_UPLOAD_BYTES` | `107374182400` (100 GiB) | Enforced before any provider call, not just at the end. |
| `SKEIN_MAX_UPLOADS_PER_USER` | `10` | Concurrent upload slots per user (Rules.md §2.13: uploads are capped so one client can't occupy every worker). |

## Timers

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_SHUTDOWN_TIMEOUT` | `30s` | How long `skein serve` drains in-flight requests on SIGTERM/SIGINT before forcing exit. **Not used by `skein-desktop`**, which has its own fixed 4s drain budget sized for a human watching the window close — see [ARCHITECTURE.md](ARCHITECTURE.md#desktop-shutdown). |
| `SKEIN_QUOTA_SYNC_EVERY` | `5m` | Background Drive-quota refresh interval. Never on the upload hot path — uploads read the cached figure and rely on the atomic reservation to stay correct regardless of staleness. |
| `SKEIN_RECLAIM_EVERY` | `60s` | How often the janitor reclaims expired quota reservations from crashed or abandoned uploads. |

## Striping and routing

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_SHARD_SIZE_BYTES` | `268435456` (256 MiB) | Must be a whole multiple of the 64 KiB AEAD frame size, and at least 1 MiB — enforced at boot, not discovered at upload time. |
| `SKEIN_ROUTING_POLICY` | `most-available` | `most-available`, `priority`, or `round-robin`. Which connected drive a new shard lands on when more than one has room. |

## Encryption

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_ENCRYPTION_ENABLED` | `true` | Turning this off is for local debugging of the storage path only. Files written while `false` stay plaintext at rest; re-enabling does **not** retroactively encrypt them. The server logs a warning on every boot with this off. |

## 6. Desktop-only

| Variable | Default | Notes |
|---|---|---|
| `SKEIN_GOOGLE_DESKTOP_CLIENT_ID` | *(empty; falls back to the compiled-in default)* | Overrides the Google Desktop app OAuth client id compiled into `skein-desktop` at build time, for anyone who wants their own API quota instead of Skein's shared one. Re-read on every connect attempt, so setting it takes effect without restarting the app. **Must be set together with `_SECRET`** — setting only this one is rejected rather than silently pairing your client id with the built-in secret of a different client. |
| `SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET` | *(empty; falls back to the compiled-in default)* | The secret Google issues for the Desktop app client above. Google requires it at the token exchange even though the client is public, so it must be set whenever `_ID` is. It is **not confidential** — it ships inside the distributed binary and PKCE is what actually secures this flow; see [SECURITY.md](SECURITY.md#pkce-desktop-oauth). |

Not read from `.env.example` by default; export it in your shell or launch
`skein-desktop` with it set if you want your own client.
