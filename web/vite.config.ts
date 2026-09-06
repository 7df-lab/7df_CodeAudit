import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

// 开发代理到网关（14号 P1：客户端零直连微服务）
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/v1': {
        target: process.env.CODEAUDIT_GATEWAY_URL || 'http://localhost:8080',
        changeOrigin: true,
        ws: true, // ADR-172: WebSocket 升级透传（/v1/tasks/{id}/ws）；preview.proxy 缺省继承本配置
      },
    },
  },
  build: {
    // antd 生态单块 ≈1MB（antd+icons+rc-*+dayjs，版本随 antd 锁定同步升版），
    // 属组件库固有体积，再拆不改善缓存粒度——阈值为有依据接受，非静默压制
    chunkSizeWarningLimit: 1000,
    rollupOptions: {
      output: {
        // 按变更频率分包：应用代码每版都变，三方库几乎不变——
        // nginx 对 /assets/ 30d immutable 缓存下，发版后回访只需重下应用块。
        // 函数式按模块 id 归块（对象式对传递依赖的落块不可控）；
        // react 全家桶与 antd 分开，dayjs 随 antd（其日期组件强依赖），
        // 其余三方（axios/tanstack）入 vendor
        manualChunks(id: string) {
          if (!id.includes('node_modules')) return undefined;
          if (/[\\/]node_modules[\\/](react|react-dom|scheduler|react-router|react-router-dom|@remix-run)[\\/]/.test(id)) return 'react';
          if (/[\\/]node_modules[\\/](antd|@ant-design|@rc-component|rc-[^\\/]*|dayjs)[\\/]/.test(id)) return 'antd';
          return 'vendor';
        },
      },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: './src/test-setup.ts',
  },
});
