import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
import fs from 'fs'

// Restores the .gitkeep that lets `//go:embed all:frontend/dist` find a
// non-empty directory on a fresh checkout. Vite's emptyOutDir wipes the
// whole dist on every build, including dotfiles — without this hook, every
// `npm run build` would silently delete the placeholder.
const restoreGitkeep = {
  name: 'restore-gitkeep',
  closeBundle() {
    const target = path.resolve(__dirname, '../cmd/server/frontend/dist/.gitkeep')
    fs.writeFileSync(target, '')
  },
}

export default defineConfig({
  plugins: [react(), restoreGitkeep],
  build: {
    outDir: '../cmd/server/frontend/dist',
    emptyOutDir: true,
    // noVNC 1.7 uses top-level await for WebCodecs H264 support detection.
    // Vite's default target ('modules': chrome87/firefox78/safari14/etc)
    // predates TLA support. Bumping to es2022 keeps the same broad browser
    // coverage while allowing TLA — every browser >= ~April 2021 has it.
    target: 'es2022',
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
        // Don't let Vite follow backend redirects — the browser must follow
        // them so OAuth cookies are set on the correct origin.
        autoRewrite: true,
      },
    },
  },
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
})
