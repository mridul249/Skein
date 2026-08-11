<samp>

# Google OAuth

Skein stores file data in your own Google Drive accounts. It never asks for
full Drive access; every Google client requests only the **`drive.file`**
scope.

That means Skein can:

- Upload files it creates.
- Read files it previously uploaded.
- Delete files it previously uploaded.

It **cannot** browse, modify, or read the rest of your Google Drive.

---

# Two OAuth flows

Skein has two different OAuth flows depending on how it is run.

| Build | OAuth client | Setup |
|--------|--------------|-------|
| `skein` (server) | Your own **Web application** client | Required |
| `skein-desktop` | Your own **Desktop application** client | Required for released binaries |

**Released binaries ship with no credentials compiled in.** The build accepts
them as `-ldflags` values, but the published release does not set them - it is
built from a public workflow, and a client secret baked in there would be a
secret published in build logs. Verified against
`skein-desktop-v1.0.0-rc2-windows-amd64.exe`: it contains no client id at all.

So on a downloaded desktop binary you must supply
`SKEIN_GOOGLE_DESKTOP_CLIENT_ID` and `SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET`
yourself, or connecting a drive fails with
`No Google client secret is configured`. That error is this requirement, not a
bug. See [Using your own Desktop OAuth client](#using-your-own-desktop-oauth-client)
below - for a released binary it is the only path, not an alternative one.

---

# Server build

The headless server does **not** ship with a Google OAuth client.

Create one in Google Cloud Console.

## 1. Enable the Drive API

Open:

```
Google Cloud Console
→ APIs & Services
→ Library
→ Google Drive API
→ Enable
```

---

## 2. Configure the OAuth consent screen

Open:

```
APIs & Services
→ OAuth consent screen
```

Choose **External**.

Fill in the required application information.

For personal use you do **not** need Google verification. Simply add your own
Google accounts as **Test Users**.

---

## 3. Create an OAuth client

```
APIs & Services
→ Credentials
→ Create Credentials
→ OAuth client ID
```

Application type:

```
Web application
```

---

## 4. Configure the redirect URI

Add:

```
http://localhost:8080/api/accounts/google/callback
```

Or, in production:

```
https://your-domain/api/accounts/google/callback
```

The URI **must exactly match** `SKEIN_GOOGLE_REDIRECT_URL`.

---

## 5. Copy the credentials

Set:

```
SKEIN_GOOGLE_CLIENT_ID
SKEIN_GOOGLE_CLIENT_SECRET
SKEIN_GOOGLE_REDIRECT_URL
```

Restart Skein.

---

## 6. Connect a drive

Open:

```
Settings
→ Connect Drive
```

Sign in with Google.

Once complete, the drive becomes available immediately.

---

# Desktop build

The desktop application uses Google's **Desktop application** OAuth flow
(RFC 8252).

**Set your credentials first.** A released binary has none compiled in, so
connect will fail without these:

```bash
export SKEIN_GOOGLE_DESKTOP_CLIENT_ID=...apps.googleusercontent.com
export SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET=...
```

On Windows PowerShell:

```powershell
$env:SKEIN_GOOGLE_DESKTOP_CLIENT_ID = "...apps.googleusercontent.com"
$env:SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET = "..."
```

Both are read on every connect attempt, so you can set them without
restarting the app. Creating the client is described below. Then choose:

```
Settings
→ Connect Drive
```

Your system browser opens.

Sign in to Google.

Approve access.

Close the browser tab.

The desktop application automatically completes the connection.

---

# Using your own Desktop OAuth client

**Required for any released binary**, which ships with no credentials. It is
optional only if you built the binary yourself and passed
`DESKTOP_CLIENT_ID`/`DESKTOP_CLIENT_SECRET` as build arguments.

Create a **Desktop application** OAuth client in Google Cloud Console.

Application type:

```
Desktop application
```

No redirect URI needs to be configured.

Set both environment variables before starting Skein:

```
SKEIN_GOOGLE_DESKTOP_CLIENT_ID
SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET
```

If only one is set, Skein refuses to use it rather than mixing credentials from
different OAuth clients.

---

# Why does Skein open the system browser?

The desktop application intentionally performs authentication in your system's
default browser instead of embedding Google's login page.

This follows Google's recommended OAuth flow for native applications (RFC 8252)
and avoids the application ever handling your Google password directly.

---

# Why `drive.file`?

Google Drive exposes several OAuth scopes.

Skein intentionally requests the smallest one that still allows it to function.

With `drive.file`, Skein can only access files that it created itself.

It cannot browse your existing Drive, read personal documents, or modify files
outside its own storage.

---

# Reconnecting a drive

If an access token expires or is revoked:

1. Remove the affected drive.
2. Connect the same Google account again.

Skein matches the account using Google's stable account identifier, so existing
shards remain linked to the account and become accessible again after
re-authentication.

---

# Security notes

- OAuth tokens are encrypted before being stored.
- File encryption is independent of Google OAuth.
- Revoking Google access does not delete your uploaded shards.
- Losing your Google account does **not** reveal encrypted file contents.
- Losing `SKEIN_MASTER_KEY` permanently prevents decryption of stored files.

For the complete security model, see:

- `docs/SECURITY.md`
- `docs/BACKUP.md`


</samp>