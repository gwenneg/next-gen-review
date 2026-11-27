import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// In dev, Vite forwards /api (including the WebSocket) to the Go api, like nginx does in the built image.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': { target: process.env.API_URL ?? 'http://localhost:8080', ws: true },
    },
  },
})
