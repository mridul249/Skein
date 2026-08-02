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
| `skein-desktop` | Built-in **Desktop application** client | Works out of the box |

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

Unlike the server build, **no Google Cloud Console setup is required**.

Choose:

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

The desktop application includes a working OAuth client by default.

If you prefer to use your own Google API quota, create a **Desktop application**
OAuth client in Google Cloud Console.

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