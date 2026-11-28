import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

// In dev, Vite forwards /api (including the WebSocket) to the Go api, like nginx does in the built image.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': { target: process.env.API_URL ?? 'http://localhost:8080', ws: true },
    },
  },
  test: { environment: 'jsdom', setupFiles: ['./src/test/setup.ts'] },
})
