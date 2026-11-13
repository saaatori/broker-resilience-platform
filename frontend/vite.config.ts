import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      // 开发期把 /api 转发给本机 Go 后端（工程目录结构 §3.2），浏览器同源免 CORS
      '/api': { target: 'http://localhost:8080', changeOrigin: true },
    },
  },
})
