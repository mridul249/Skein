import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
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
