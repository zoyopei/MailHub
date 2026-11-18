import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发时 proxy 到本地 Go 服务；构建产物输出 dist 由 internal/web embed。
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:18099',
      '/healthz': 'http://127.0.0.1:18099',
    },
  },
  build: {
    outDir: 'dist',
    chunkSizeWarningLimit: 1024,
  },
})
