# Frontend 
FROM node:20-alpine AS frontend

WORKDIR /src/web

COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
COPY internal/web/dist/.gitkeep /src/internal/web/dist/.gitkeep
RUN npm run build \
    && npm cache clean --force \
    && rm -rf /root/.npm /src/web/node_modules

# Go build 
FROM golang:1.26-alpine AS gobuild

RUN apk add --no-cache git

WORKDIR /src

COPY go.mod go.sum ./
COPY third_party/ ./third_party/
RUN go mod download

COPY . .

COPY --from=frontend /src/internal/web/dist /src/internal/web/dist

ARG VERSION=docker
ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build \
      -trimpath \
      -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/skein \
      ./cmd/skein \
    && go clean -cache -modcache -testcache

# web: the runnable image
FROM alpine:3.20 AS web

RUN apk add --no-cache ca-certificates tzdata \
    && rm -rf /var/cache/apk/*

RUN addgroup -g 10001 -S skein \
    && adduser -u 10001 -S -G skein -h /home/skein skein

COPY --from=gobuild /out/skein /usr/local/bin/skein
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN sed -i 's/\r$//' /usr/local/bin/docker-entrypoint.sh \
    && chmod 0755 /usr/local/bin/skein /usr/local/bin/docker-entrypoint.sh

RUN mkdir -p /data && chown skein:skein /data
VOLUME /data

USER skein
EXPOSE 8080

ENV SKEIN_ADDR=:8080 \
    SKEIN_INFO_DIR=/data

HEALTHCHECK --interval=15s --timeout=3s --start-period=30s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]

# desktop: build only, produces a binary for the host 

FROM golang:1.26-bookworm AS desktopbuild

RUN apt-get update && apt-get install -y --no-install-recommends \
      libgtk-3-dev libwebkit2gtk-4.1-dev libsoup-3.0-dev pkg-config git \
    && rm -rf /var/lib/apt/lists/*

RUN go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0

WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/ ./third_party/
RUN go mod download
COPY . .
COPY --from=frontend /src/internal/web/dist /src/internal/web/dist

ARG VERSION=docker

ARG DESKTOP_CLIENT_ID=""
ARG DESKTOP_CLIENT_SECRET=""

RUN cd cmd/skein-desktop \
    && /go/bin/wails build -tags webkit2_41,desktop -skipbindings \
        -trimpath \
        -ldflags "-X main.version=${VERSION} -X main.desktopClientID=${DESKTOP_CLIENT_ID} -X main.desktopClientSecret=${DESKTOP_CLIENT_SECRET}" \
        -clean -f \
    && mkdir -p /out \
    && cp build/bin/skein-desktop /out/skein-desktop \
    && go clean -cache -modcache -testcache

FROM scratch AS desktop
COPY --from=desktopbuild /out/skein-desktop /skein-desktop

# desktop-windows: build only, cross-compiled from Linux, no CGO

FROM golang:1.26-alpine AS desktopbuild-windows

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/ ./third_party/
RUN go mod download
COPY . .
COPY --from=frontend /src/internal/web/dist /src/internal/web/dist

ARG VERSION=docker
ARG DESKTOP_CLIENT_ID=""
ARG DESKTOP_CLIENT_SECRET=""

RUN CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
    go build \
      -tags desktop \
      -trimpath \
      -buildvcs=false \
      -ldflags "-s -w -H=windowsgui -X main.version=${VERSION} -X main.desktopClientID=${DESKTOP_CLIENT_ID} -X main.desktopClientSecret=${DESKTOP_CLIENT_SECRET}" \
      -o /out/skein-desktop.exe \
      ./cmd/skein-desktop \
    && go clean -cache -modcache -testcache

FROM scratch AS desktop-windows
COPY --from=desktopbuild-windows /out/skein-desktop.exe /skein-desktop.exe
