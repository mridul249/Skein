# Install

## Requirements

- Go 1.25+
- Docker
- Git

For the desktop build:

- GTK3
- WebKitGTK 4.1
- Wails

---

## Quick Start (Server)

Clone the repository.

```bash
git clone https://github.com/mridul249/Skein.git
cd Skein
```

Copy the example configuration.

```bash
cp .env.example .env
```

Generate the required secrets.

```bash
openssl rand -base64 32
```

Paste it into:

```
SKEIN_MASTER_KEY=
```

Generate the JWT secret.

```bash
openssl rand -base64 48
```

Paste it into:

```
SKEIN_JWT_SECRET=
```

Start PostgreSQL.

```bash
make dev-db
```

Build Skein.

```bash
make build
```

Run it.

```bash
./bin/skein
```

Open

```
http://localhost:8080
```

Register.

Sign in.

Connect your Google Drive.

Upload your first file.

---

## Desktop

Install dependencies.

```bash
sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev libsoup-3.0-dev

go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
```

Build.

```bash
make desktop
```

Run.

```bash
./bin/skein-desktop
```

Click **Connect Drive**.

Authorize Google.

Done.

---

## Need your own Google OAuth client?

See [OAUTH](docs/OAUTH.md)

---

## Production deployment

See [DEVELOPMENT](docs/DEVELOPMENT.md)