import { defineConfig } from 'vitest/config';

// The dev server proxies API and media requests to the local Go backend.
// changeOrigin stays off so the backend sees the original Host.
export default defineConfig({
  server: {
    proxy: {
      '/api': { target: 'http://127.0.0.1:8080', changeOrigin: false },
      '/m': { target: 'http://127.0.0.1:8080', changeOrigin: false },
      '/healthz': { target: 'http://127.0.0.1:8080', changeOrigin: false },
    },
  },
  build: {
    target: 'es2022',
  },
  test: {
    environment: 'jsdom',
  },
});
