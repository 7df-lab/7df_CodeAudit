import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

// 开发代理到网关（14号 P1：客户端零直连微服务）
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/v1': {
        target: process.env.AUDITMIND_GATEWAY_URL || 'http://localhost:8080',
        changeOrigin: true,
        ws: true, // ADR-172: WebSocket 升级透传（/v1/tasks/{id}/ws）；preview.proxy 缺省继承本配置
      },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: './src/test-setup.ts',
  },
});
