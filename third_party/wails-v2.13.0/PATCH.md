# Skein's fork of Wails v2.13.0

Vendored from `github.com/wailsapp/wails/v2@v2.13.0` and patched locally,
pinned via a `replace` directive in the repo root `go.mod`. Not upstreamed;
re-apply this diff (or re-derive it) on any Wails version bump.

## Why

`wails.Run` always navigates the window to the synthetic `wails://wails/`
scheme. Every request the frontend makes — API calls, uploads, downloads —
goes through `pkg/assetserver/assetserver_webview.go`'s
`processWebViewRequestInternal`, which builds each `*http.Request` with
`http.NewRequest(method, uri, body)`. That call attaches
`context.Background()` and nothing downstream ever replaces it with a
cancellable context tied to the actual `WebKitURISchemeRequest`. Consequence,
found and reproduced in Skein's desktop build (2026-08-01):

- `r.Context()` inside every server handler never cancels through this
  bridge — not on `xhr.abort()`, not on window close, not on tab navigation.
  A cancelled upload keeps writing shards and holding quota reservations
  until the process itself dies.
- WebKitGTK's `WebKitURISchemeRequest` body is a `GInputStream`, read via
  `g_input_stream_read_all` (`pkg/assetserver/webview/webkit2_40+.go`).
  Nothing in Wails' Linux frontend connects `WebKitWebView::download-started`
  or any download-policy signal, so a `<a download>` click has no download
  manager to land in — confirmed by grepping the entire
  `internal/frontend/desktop/linux` package for `download`: zero hits.

Neither of these can be fixed from Skein's own code. They are structural to
routing every request through the custom scheme instead of a real HTTP
connection.

## What changed

Three files, minimal diff:

- `internal/app/app_production.go` — `CreateApp` takes a new `startURL
  string` parameter. When non-empty, it is parsed and stored under the
  `"starturl"` context key that `internal/frontend/desktop/linux/frontend.go`
  (and the darwin/windows equivalents) **already read** — this key exists in
  stock Wails and already does the right thing when set; there was simply no
  public path to set it. When `starturl` is present, `NewFrontend` skips
  registering the custom scheme handler and its asset server entirely
  (`frontend.go:196-222`) and the window navigates straight to the real URL.
  Only the `production` build tag is patched — Skein always builds with
  `-tags production,webkit2_41`, and the `dev`/`debug`/`bindings` variants of
  `CreateApp` are untouched, so this fork does not affect anyone using those.
- `pkg/application/application.go` — `Application.startURL` field,
  `NewWithOptionsAndStartURL` constructor, `Run()` passes it through to
  `CreateApp`.
- `wails.go` — `RunWithStartURL(opts *options.App, startURL string) error`,
  alongside the untouched `Run`.

Everything else — the whole webview/webkit2 bridge, the C bindings, every
other Wails feature Skein doesn't use — is byte-identical to upstream. This
patch only adds a second entry point; it does not remove or alter `Run`.

## Consequence for Skein

`cmd/skein-desktop` calls `wails.RunWithStartURL` with `"http://" +
a.Addr()` — the same real, in-process `net/http.Server` already listening on
`127.0.0.1:<random port>` for `cmd/skein`'s own web build. The window becomes
a real HTTP client of a real server: `r.Context()` cancels correctly on
abort, and downloads go through WebKit's normal HTTP loader, which does have
working download handling (the gap was specific to the custom-scheme path).

The reverse-proxy `AssetServer.Handler` this replaced is no longer used and
was removed from `cmd/skein-desktop/main.go` in the same change.
