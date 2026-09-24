import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  base: './',
  build: {
    outDir: 'dist',
    sourcemap: false,
    rollupOptions: {
      output: {
        // Perf: split the stable vendor runtime (react/react-dom/router)
        // from app code so a deploy only invalidates the small app chunk —
        // the vendor chunk is served Cache-Control: immutable (Dockerfile).
        manualChunks: {
          vendor: ['react', 'react-dom', 'react-router-dom'],
        },
      },
    },
  },
  server: { port: 5201 },
})
