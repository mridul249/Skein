<samp>

# skein

```
         one file  ─┬─►  ████████  drive 1
                    ├─►  ████████  drive 2
                    └─►  ████      drive 3

                    s k e i n

```

**75 GB of free cloud storage that cannot hold a 30 GB file.**

Five free Google accounts give you 75 GB of total storage, but they operate as isolated 15 GB silos. You are forced to manually manage file splits, and a single large file-like a 30 GB raw video archive or virtual machine image-simply has nowhere to land.

If you have ever tried to combine multiple free Google Drive accounts into a single pool of storage, you have likely run into the same hurdles as existing options: **files cannot span across accounts, encryption breaks media seeking, tools demand full access to your personal drive, or losing your local index database permanently destroys access to your data.**

I built Skein to solve these structural issues in a single static binary.

**Skein** pools your fragmented cloud drives into a unified storage array. It automatically stripes large files across multiple accounts when no single drive fits, encrypts everything client-side before it leaves your machine, and packages the entire runtime into a single Go binary with an embedded web interface.

```
$ skein status
──────────────────────────────────────────────────────
 ▓▓▓▓▓▓▓▓▒▒▒▒▒░░░░░  38.2 / 75 GB          4 drives

 archive.tar.zst   28.4 GB   ●●●    3 shards, 3 drives
 video.mkv          4.1 GB   ●      1 drive
 notes.md            12 KB   ●      1 drive

```

*Skein (noun): a coiled length of yarn. Files are woven across multiple accounts.*

---

## Technical Comparison

Existing storage aggregators force a trade-off between deployment complexity, file striping capabilities, encryption, and data safety:

| Feature | rclone (`union` + `crypt`) | 9drive / DriveConnect | **Skein** |
| --- | --- | --- | --- |
| **Storage Pooling** | Yes | Yes | **Yes** |
| **Large-File Striping** | No | No | **Yes** |
| **Encrypted by Default** | Opt-in overlay | No | **Yes (Framed GCM)** |
| **Media Seeking & Previews** | High latency (large chunks) | Full download required | **Instant (64 KiB frames)** |
| **Data Recovery Architecture** | Lost if local index is wiped | Central DB reliant | **Self-Describing Manifests** |
| **Privacy Scope** | Full Drive Access | Full Drive Access | **`drive.file` scope only** |
| **Deployment Footprint** | Binary + complex config | MySQL + 2x Node + Compose | **One single binary** |

---

## Core Architecture Pillars

### 1. Self-Describing Drives (Zero Database Lock-In)

Most storage aggregators rely exclusively on a centralized local database to track where file chunks reside. If that database becomes corrupted or lost, your data is permanently unrecoverable.

Skein writes an encrypted sidecar manifest directly alongside every file's shards on your cloud drives. Your storage accounts become completely **self-describing**. The local database acts as a high-performance index cache rather than a single point of failure. If your machine crashes, an end-to-end restore can rebuild the state index straight from the connected drives.

### 2. Zero-Allocation Memory Streaming

Memory footprint is bound by strict performance limits enforced directly in CI unit tests:

```bash
$ go test -run TestUploadHoldsConstantMemory -v ./internal/files/
    uploaded 2147483648 bytes; peak HeapAlloc 677368 bytes (0.6 MiB), ceiling 150 MiB
--- PASS

```

Streaming a 2 GiB upload consumes only **0.6 MiB of heap memory**. The entire throughput relies on pure `io.Reader` stream composition over a fixed 256 KiB buffer:

```
request body ─► TeeReader (SHA-256) ─► StreamEncrypter ─► ShardWriter ─► provider

```

Multi-gigabyte uploads remain supported even on resource-constrained 512 MB VPS instances.

### 3. Framed AES-256-GCM Encryption for Instant Seeking

Standard whole-file GCM encryption forces you to buffer and decrypt entire gigabyte-scale ciphertexts before verifying a single byte. Skein slices data into granular **64 KiB encrypted frames** with computable offsets:

* **Instant Video Scrubbing:** HTTP range requests map requested byte ranges directly to their containing 64 KiB frames. Seeking a video fetches **64 KiB instead of 256 MiB**.
* **In-App Previews:** Generates image and document previews directly from remote encrypted shards without downloading the full payload.

### 4. Restricted Security Scope (`drive.file`)

Skein operates strictly under Google's narrow `drive.file` OAuth scope. It can only access, modify, or delete files that were created by Skein itself. It cannot index, read, or touch pre-existing files in your personal Google Drive accounts.

---

## Server Architecture & Background Engine

Skein’s core backend (`app/app.go`) unifies headless server deployments (`cmd/skein`) and desktop GUI wrappers (`cmd/skein-desktop` via Wails):

* **Dynamic Network Binding:** Supports arbitrary port binding (`127.0.0.1:0`) via `Listen`, enabling desktop runtimes to automatically acquire open ports.
* **Autonomous Background Workers:** Dedicated loops manage continuous background processes, including `quota-sync`, `purge-oauth-states`, `reclaim-reservations`, and `purge-sessions`.
* **Graceful Lifecycle Management:** Coordinates HTTP server draining (`httpSrv.Shutdown`), background worker completion (`workers.Wait()`), and safe database connection teardown.
* **Single-Binary Web Embedding:** Embeds the compiled Vite frontend directly into the Go binary using `//go:embed all:dist` (`web/embed.go`), serving static assets with immutable caching (`Cache-Control: public, max-age=31536000`) and SPA routing fallbacks.

---

## Quickstart

### Headless Server (`cmd/skein`)

```bash
git clone https://github.com/your-username/skein.git && cd skein
cp .env.example .env
openssl rand -base64 32   # Set output as SKEIN_MASTER_KEY in .env
make dev-db && make web && make build
./bin/skein

```

### Desktop Application (`cmd/skein-desktop`)

The desktop wrapper runs the core server inside a native Wails window, using an ephemeral localhost port and native system browser OAuth (RFC 8252 PKCE):

```bash
sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev libsoup-3.0-dev
go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
make desktop   # Outputs bin/skein-desktop

```

> **Build Note:** On modern Linux distributions (e.g., Ubuntu 24.04+), `wails doctor` may report a missing `webkit2gtk-4.0` package. This is a false positive-`make desktop` explicitly targets the modern `4.1` ABI using `-tags webkit2_41`.

---

## Documentation Index

| Guide | Description |
| --- | --- |
| [docs/INSTALL.md](https://www.google.com/search?q=docs/INSTALL.md) | Step-by-step setup and OAuth client setup |
| [docs/CONFIGURATION.md](https://www.google.com/search?q=docs/CONFIGURATION.md) | Environment variables and runtime configuration |
| [docs/BACKUP.md](https://www.google.com/search?q=docs/BACKUP.md) | Disaster recovery, master key management, and manifest recovery |
| [docs/ARCHITECTURE.md](https://www.google.com/search?q=docs/ARCHITECTURE.md) | Shard routing, crypto specs, and quota calculations |
| [docs/SECURITY.md](https://www.google.com/search?q=docs/SECURITY.md) | Threat model, zero-knowledge guarantees, and scope isolation |
| [docs/DEVELOPMENT.md](https://www.google.com/search?q=docs/DEVELOPMENT.md) | Build steps, test runner execution, and contributing guidelines |

---

## Status & Scope

**v0.2.0** - Skein is designed strictly as a single-tenant storage engine. Multi-tenancy, team sharing, mobile clients, and background directory synchronization are intentionally out of scope.


</samp>