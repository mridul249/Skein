# Security

## Threat model

Skein assumes:

- **The database and the connected-drive contents are not equally trusted.**
  Google Drive is treated as an untrusted storage substrate — everything
  written to it is encrypted before it leaves the machine running Skein,
  and Skein never grants a provider-side public permission on anything it
  writes (no `role: 'writer', type: 'anyone'`, ever, under any
  circumstance).
- **The operator running Skein is trusted; a network attacker and another
  authenticated user are not.** Every database read of a user-owned row
  filters by the requesting user's id — never fetched first and checked
  after — so one user cannot reach another's files, accounts, or shards by
  guessing an id.
- **Client-declared values are claims, not facts.** Declared upload size is
  checked against bytes actually received; a mismatch fails the upload and
  cleans up rather than trusting the header. Client-declared MIME type is
  stored as a label only — what actually gets served inline is decided by
  sniffing the real bytes against a fixed allowlist.

## Zero-knowledge guarantees, precisely stated

**What "zero-knowledge" means here:** Google (or anyone with read access to
a connected Drive account) sees only encrypted shard bytes and cannot
reconstruct file contents, names, or structure from them alone.

**What it does not mean:** the PostgreSQL database is not zero-knowledge.
It holds file names, folder structure, sizes, and the shard-to-file mapping
in plaintext (except OAuth tokens, which are themselves encrypted at rest —
see below). Anyone with direct database access sees your file names and
knows how your storage is organized, even though they cannot read file
*contents* without also having `SKEIN_MASTER_KEY`. Do not conflate
"encrypted before it leaves the machine" with "the operator's own database
is opaque to the operator" — it deliberately is not, because Skein is
self-hosted for one operator's own use, not a service where the operator
is also an adversary.

## Cryptography

- **Master key:** 32 bytes from `SKEIN_MASTER_KEY`, base64. Every other key
  in the system derives from it via HKDF-SHA256 — never `sha256(passphrase)`
  used directly as a key.
- **Per-file keys:** `HKDF-SHA256(master, salt=file_id, info="skein-file-v1")`.
  A compromised single file's key does not expose any other file.
- **Content encryption:** AES-256-GCM in fixed 64 KiB frames, never one GCM
  message per whole file. Each frame's nonce derives deterministically from
  `(file_id, frame_index)` — unique by construction under a fixed key,
  never reused, and what makes range reads computable without decrypting
  the whole object first.
- **Envelope format:** every ciphertext (file content and stored OAuth
  tokens alike) carries a version byte and key id ahead of the ciphertext
  and its authentication tag, which is what makes key rotation possible
  later without a flag day.
- **Passwords:** argon2id (`time=3, memory=64MB, threads=4, keyLen=32`).
  Secret comparisons use `subtle.ConstantTimeCompare`, never `==`.

## Sessions

- **Access tokens:** JWT, 15 minutes (`SKEIN_ACCESS_TOKEN_TTL`), HS256,
  carrying only `sub` and a session id — pinned against `alg=none`.
- **Refresh tokens:** 32 random bytes, opaque, stored as a SHA-256 hash
  (never the token itself), single-use. Every refresh rotates the token
  and records a `family_id`/`prev_id` chain.
- **Reuse detection:** presenting an already-used refresh token revokes the
  **entire family**, not just that one session, and logs a security event.
  This closed a real race (project history: known issue #11) where
  revocation enumerated only the sessions that existed at the instant it
  ran, missing a successor inserted a moment later — the fix marks the
  family itself as revoked, and validity is now two-part: the session
  isn't revoked *and* its family isn't.
- **Frontend token handling:** the access token lives in a JS variable in
  memory, never `localStorage` or `sessionStorage`. The refresh token lives
  in an `httpOnly; SameSite=Strict` cookie, `Secure` whenever
  `SKEIN_ENV=production` or `SKEIN_PUBLIC_URL` is https (see
  [CONFIGURATION.md](CONFIGURATION.md#public-url-and-cookies) for exactly
  when).

## OAuth account linking

**Never linked by email alone.** Identity for linking a Google account to a
Skein user is the provider's own stable account id (`sub`), never the email
address — an address can be re-registered by someone else, and merging on
email alone is the account-takeover pattern this rule exists to close.

**Scope: `drive.file` only.** Skein can see, create, and modify only the
files it created itself. It cannot index, read, or touch anything already
in a connected Drive account. This is also what keeps Skein out of Google's
restricted-scope verification review — a real, deliberate tradeoff, not an
oversight: it means Skein cannot offer "pool my existing Drive files,"
because it structurally cannot see them.

### PKCE (desktop OAuth)

The desktop build uses a Desktop app (RFC 8252) OAuth client — **no client
secret is compiled into the binary, because this client type does not have
one.** A client secret's confidentiality assumption doesn't hold for
something distributed to every user's machine; RFC 8252 treats the client
ID as non-secret and relies on PKCE instead:

- A random verifier is generated fresh for every connect **attempt**, never
  reused across attempts — even a retried connection gets a new one.
- The S256 challenge derived from it goes in the authorization request; the
  verifier itself is presented only at the token exchange, and only after
  being read back from server-side state (the single-use OAuth state row) —
  never trusted from the callback's own query string, which an attacker
  controls.
- The redirect target is an ephemeral loopback listener
  (`http://127.0.0.1:<random-port>/callback`), opened fresh per attempt and
  closed — releasing the port — on every exit path: success, provider
  error, timeout, or the caller giving up. RFC 8252 explicitly permits
  arbitrary loopback ports for this client type.

### Why the system browser

The authorization request opens the operating system's default browser,
never the app's embedded webview. Two reasons, both real: Google's own
policies discourage — and increasingly block outright — OAuth consent
screens rendered inside embedded webviews, since an embedded view can't
show the user a real, unspoofable address bar; and a real browser is where
saved passwords, passkeys, and existing Google sessions actually work.

## Content delivery and the capability URL

`GET/HEAD /api/files/{id}/content` accepts a normal bearer token **or** a
short-lived, HMAC-signed capability URL minted by `POST
/api/files/{id}/content-url`. The second credential exists only because a
browser-managed download (`<a download>`, which is what keeps a
multi-gigabyte transfer off the JS heap — see
[ARCHITECTURE.md](ARCHITECTURE.md)) cannot set an `Authorization` header.

- **Scope:** one file, one user. Signature binds a purpose tag, the file id,
  the user id, and an expiry — verified against the file id in the request
  **path**, not a query parameter, so a grant minted for one file
  structurally cannot be replayed against another.
- **Lifetime:** ~15 minutes, checked once at request start. A signature
  accepted at that moment authorizes a stream of any duration — the TTL
  bounds how long a *leaked, unused* URL stays dangerous, not how long an
  in-progress transfer can run.
- **Not single-use.** A video element scrubbing issues many range requests
  against the same grant by construction; a single plain download is one
  request that may stream for an hour. A redemption counter would break
  the first case and buy nothing against the second.
- **Not revocable.** A stateless signature has no server-side record to
  delete. This is a real, accepted gap: an outstanding capability URL
  survives logout and even survives the refresh-token-family revocation
  above, for up to its 15-minute TTL. It is the same class of exposure the
  access token already has, and is judged acceptable on the same
  reasoning — but it is a *second* place that exposure exists, and closing
  one without the other in any future change is closing half a gap.
- **Persisted where you might not expect.** Chrome (and likely other
  browsers) writes the full download URL, signature included, into its own
  downloads database — not browsing history, but the downloads list, which
  syncs across signed-in devices. An expired URL is harmless regardless of
  where it ends up, which is the actual defense here, not secrecy of the
  URL itself.

## Content type and inline preview

Client-declared MIME type is stored as metadata only. Serving decides
independently: `http.DetectContentType` sniffs the real bytes, and only an
explicit allowlist (`image/png|jpeg|gif|webp|avif`, `video/mp4|webm`,
`audio/mpeg|ogg|wav`, `application/pdf`, `text/plain`) is ever served
`inline`. Everything else is `application/octet-stream` with
`Content-Disposition: attachment`. `X-Content-Type-Options: nosniff` is set
on every response; `text/html` and any `*+xml` (including SVG) are never
served inline under any circumstance — a file named `photo.png` that
actually contains HTML downloads as an attachment, it does not execute.

## Rate limiting

Per-key (per-IP or per-user, depending on the route), enforced by default
rather than opt-in:

| Group | Budget |
|---|---|
| Auth endpoints (login, register, refresh) | 5/min |
| General authenticated API | 300/min |
| Public/unauthenticated routes | 30/min |
| Content byte-serving routes | 600/min (sized separately from general API traffic — a page of thumbnails or a scrubbing video issues many range requests in quick succession, and the route is already scoped to one file per capability grant with a 15-minute TTL as the primary defense) |

Every JSON request body is capped at 1 MB via `http.MaxBytesReader`,
independent of the configured upload ceiling (`SKEIN_MAX_UPLOAD_BYTES`),
which governs only the streaming upload route.

## Trusted proxies

`X-Forwarded-For` is honoured only from peers listed in
`SKEIN_TRUSTED_PROXIES`. An empty list — the default — means every
request's IP is taken from `RemoteAddr` directly. Setting `trust proxy:
true`-equivalent behavior without an explicit allowlist would let any
client forge its own IP into audit records; this is why the variable
exists and why it defaults closed.

## Reporting a vulnerability

This is a self-hosted, single-tenant tool without a dedicated security
contact process yet. If you find a real vulnerability, open an issue
describing the class of problem without publishing exploit details in the
public tracker, or reach the maintainer through whatever contact method is
listed in the repository at the time.
