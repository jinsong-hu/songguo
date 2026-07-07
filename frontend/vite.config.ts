import { fileURLToPath, URL } from 'node:url';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

const backendTarget = 'http://127.0.0.1:12345';

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
    host: '127.0.0.1',
    port: 12346,
    strictPort: true,
    proxy: {
      // ws:true so the WebSocket streaming wires (e.g. /api/v3/sauc/bigmodel_async,
      // /api/v3/tts/bidirection) get their Upgrade proxied to the backend in dev,
      // not just plain HTTP. In production the backend serves both on one origin.
      '/api':     { target: backendTarget, changeOrigin: true, ws: true },
      '/v1':      { target: backendTarget, changeOrigin: true, ws: true },
      '/x':       { target: backendTarget, changeOrigin: true, ws: true },
      '/healthz': { target: backendTarget },
      '/openapi.yaml': { target: backendTarget },
      '/openapi.json': { target: backendTarget },
    },
  },
});
