import { fileURLToPath, URL } from 'node:url';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  base: '/',
  build: {
    outDir: '../backend/web/dist',
    emptyOutDir: true,
  },
  server: {
    port: 12346,
    proxy: {
      // ws:true so the WebSocket streaming wires (e.g. /api/v3/sauc/bigmodel_async,
      // /api/v3/tts/bidirection) get their Upgrade proxied to the backend in dev,
      // not just plain HTTP. In production the backend serves both on one origin.
      '/api':     { target: 'http://localhost:12345', changeOrigin: true, ws: true },
      '/v1':      { target: 'http://localhost:12345', changeOrigin: true, ws: true },
      '/x':       { target: 'http://localhost:12345', changeOrigin: true, ws: true },
      '/healthz': { target: 'http://localhost:12345' },
      '/openapi.yaml': { target: 'http://localhost:12345' },
      '/openapi.json': { target: 'http://localhost:12345' },
    },
  },
});
