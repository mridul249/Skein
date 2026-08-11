import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'

// emptyOutDir wipes the tracked .gitkeep that makes internal/web/dist exist on
// a fresh clone; //go:embed all:dist does not compile without it. Emitted as a
// build asset so every path that empties the directory also refills it -
// `make web` restores it too, but CI and a bare `npm run build` bypass that.
function keepEmbedMarker(): Plugin {
  return {
    name: 'skein-keep-embed-marker',
    generateBundle() {
      this.emitFile({ type: 'asset', fileName: '.gitkeep', source: '' })
    },
  }
}

// Strips CR from every module before it is compiled, so the bundle does not
// depend on how the tree was checked out. A CRLF working tree - Git for
// Windows' default, and what .gitattributes fixes only for clones made after
// 65315b3 - puts carriage returns inside multi-line template literals, which
// survive minification as \r escapes and change the bundle hash. That shipped
// in v1.0.0-rc1 and cost an afternoon to find, because `git status` reports
// clean the whole time: eol=lf normalises on read, so the bytes differ only on
// disk. .gitattributes is the correct fix; this is the one that survives a
// contributor whose local git config overrides it, exactly as the Dockerfile
// strips CR from the entrypoint that .gitattributes already covers.
//
// Build only: identical output is a release concern, and leaving the dev
// server untouched keeps its sourcemaps exact. Raw CR reaches source only from
// line endings - an intentional carriage return is written as the escape \r,
// which is two characters and unaffected.
function normalizeLineEndings(): Plugin {
  return {
    name: 'skein-normalize-line-endings',
    apply: 'build',
    enforce: 'pre',
    transform(code, id) {
      // First-party source only. Dependencies are pinned by the lockfile and
      // already identical everywhere, so rewriting them would change the
      // bundle relative to builds made before this plugin existed - for no
      // gain, and it would break comparison against already-published
      // checksums.
      if (id.includes('node_modules')) return null
      if (!code.includes('\r')) return null
      // Line count is unchanged, so existing mappings stay valid.
      return { code: code.replace(/\r\n/g, '\n'), map: null }
    },
  }
}

export default defineConfig({
  plugins: [normalizeLineEndings(), react(), keepEmbedMarker()],
  // Straight into the Go embed directory: `make web` then `go build` is the
  // whole pipeline, and a separate copy step is one more thing to forget.
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    // Every asset is inlined or hashed; nothing is fetched from a CDN,
    // because a self-hosted tool that phones home on page load is a
    // contradiction.
    assetsInlineLimit: 4096,
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: false },
    },
  },
})
