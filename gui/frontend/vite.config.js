import { defineConfig } from 'vite'

export default defineConfig({
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    rollupOptions: {
      input: 'index.html',
    },
  },
  server: {
    port: 5173,
    // In dev mode, proxy all API/SSE calls to the FluxGet backend
    proxy: {
      '/events':  { target: 'http://localhost:1700', changeOrigin: true },
      '/list':    { target: 'http://localhost:1700', changeOrigin: true },
      '/history': { target: 'http://localhost:1700', changeOrigin: true },
      '/pause':   { target: 'http://localhost:1700', changeOrigin: true },
      '/resume':  { target: 'http://localhost:1700', changeOrigin: true },
      '/delete':  { target: 'http://localhost:1700', changeOrigin: true },
      '/download':{ target: 'http://localhost:1700', changeOrigin: true },
      '/stream':  { target: 'http://localhost:1700', changeOrigin: true },
      '/health':   { target: 'http://localhost:1700', changeOrigin: true },
      '/settings': { target: 'http://localhost:1700', changeOrigin: true },
    },
  },
})
