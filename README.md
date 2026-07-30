# skein

```
         one file  ─┬─►  ████████  drive 1
                    ├─►  ████████  drive 2
                    └─►  ████      drive 3

                    s k e i n
```

**75 GB of free cloud storage that cannot hold a 30 GB file.**

Five Google accounts at 15 GB each is 75 GB of real capacity. But it behaves
like five separate 15 GB drives - you have to remember which one holds what,
and a single large file simply has nowhere to go.

Skein pools them, splits files across them when no one account fits, and
encrypts everything before it leaves your machine. One static binary, frontend
included.

```
skein status
──────────────────────────────────────────────────────
 ▓▓▓▓▓▓▓▓▒▒▒▒▒░░░░░  38.2 / 75 GB          4 drives

 archive.tar.zst   28.4 GB   ●●●    3 shards, 3 drives
 video.mkv          4.1 GB   ●      1 drive
 notes.md            12 KB   ●      1 drive
```

skein = a coiled length of yarn. Files are woven across multiple accounts.

---

## Why not just use rclone

`rclone union` is the honest answer to "combine my drives," and this does not
try to beat it at that.

| | Pooling | Large-file striping | Encrypted by default | Web UI | Deploy |
|---|---|---|---|---|---|
| rclone `union` | Yes | No | Opt-in overlay | No | Binary + config |
| 9drive | Yes | No | No | Yes | MySQL + 2 npm + Compose |
| **Skein** | Yes | **Yes** | **Yes** | Yes | **One binary** |

Striping is the thing neither alternative does. A 28 GB archive lands on three
drives and comes back as one file.

---

## Three claims, each with proof

### Memory never scales with file size

Not a design goal - a test that fails the build.

```
$ go test -run TestUploadHoldsConstantMemory -v ./internal/files/
    uploaded 2147483648 bytes; peak HeapAlloc 677368 bytes (0.6 MiB), ceiling 150 MiB
--- PASS
```

2 GiB through the real upload path. 0.6 MiB of heap. The whole route is
`io.Reader` composition over one fixed 256 KiB buffer:

```
request body ─► TeeReader (SHA-256) ─► StreamEncrypter ─► ShardWriter ─► provider
```

A 2 GB upload on a 512 MB VPS is a supported case, not a caveat. If a change
breaks that test, the change reintroduced buffering - raising the number is not
a fix.


### You can scrub a video that is encrypted and split across three accounts

This one is harder than it sounds. AES-256-GCM runs in 64 KiB frames rather
than one message per file, so frame *i* sits at a computable offset. A range
request maps a plaintext byte range to the frames containing it, locates those
frames across shards, and fetches **64 KiB instead of 256 MiB**.

Whole-file GCM would mean buffering gigabytes before a single byte could be
trusted. Framing keeps it streaming, localises corruption, and makes seeking
cheap.

### Skein cannot see your existing Drive files

`drive.file` scope only - it sees exactly what it created and nothing else. It
cannot index, read or touch anything already in your Drive.

That is deliberate. It also keeps the project out of Google's restricted-scope
verification review.

> **The top FAQ, up front:** Skein will not show you files already in your
> Google Drive. Not a missing feature - the point of the narrow scope.

---

## Getting started

```bash
git clone <this repo> && cd skein
cp .env.example .env
openssl rand -base64 32   # -> SKEIN_MASTER_KEY
make dev-db && make web && make build
./bin/skein
```

Then connect a Google account. Full walkthrough in
[INSTALL.md](docs/INSTALL.md) - you supply your own OAuth client, by design.

---

## Where to find things

| You want to | Read |
|---|---|
| Install it and connect a drive | [docs/INSTALL.md](docs/INSTALL.md) |
| Configure it | [docs/CONFIGURATION.md](docs/CONFIGURATION.md) |
| **Back it up, and restore it** | [docs/BACKUP.md](docs/BACKUP.md) |
| Understand striping, quota and crypto | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) |
| Review the security posture | [docs/SECURITY.md](docs/SECURITY.md) |
| Build, test and contribute | [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) |

**Read the backup page before you store anything you care about.** Two things
have to survive, not one: the master key decrypts shard contents, and the
database records which shard belongs to which file. Either alone is useless.

---

## Status

**v0.2.0.** Single-tenant by design - no multi-tenancy, no team sharing, no
SSO, no mobile apps, no sync client. Those are
non-goals, not a roadmap.

Not built yet: share links, resumable uploads surviving a refresh, S3/R2/B2
backends, WebDAV, content-addressed dedup.

## Credits

The idea of a web UI over pooled Drive accounts comes from
[9drive](https://github.com/topics/9drive). Skein is a different codebase with
different goals: striping, encryption by default, and one binary.