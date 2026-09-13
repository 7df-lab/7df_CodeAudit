import { QueryClientProvider } from '@tanstack/react-query';
import { App as AntApp, ConfigProvider } from 'antd';
import zhCN from 'antd/locale/zh_CN';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import App from './App';
import { ErrorBoundary } from './ErrorBoundary';
import { queryClient } from './api/queryClient';
import { SessionProvider } from './auth/session';
import { consoleTheme } from './theme';

// 应用启动即拉取当前用户（access 缺失时 401 → 刷新/跳登录，14号 §2.1）
// B5-P2-6: QueryClient 抽为 src/api/queryClient.ts 模块单例——登出时 auth/session
// 清同一实例的缓存（跨账号数据不残留）。
// （2026-09-13）: dev:mock 视觉走查入口——VITE_MOCK_GATEWAY=1（npm run
// dev:mock）时先挂演示网关（adapter 层，真实 client 拦截器链全量执行）再渲染，
// 无后端可打开全站核对设计。
void (async () => {
  if (import.meta.env.VITE_MOCK_GATEWAY) {
    const { installDemoGateway } = await import('./testsupport/demoGateway');
    installDemoGateway();
  }
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <ConfigProvider locale={zhCN} theme={consoleTheme}>
        <AntApp>
          <QueryClientProvider client={queryClient}>
            <BrowserRouter>
              <SessionProvider>
                <ErrorBoundary>
                  <App />
                </ErrorBoundary>
              </SessionProvider>
            </BrowserRouter>
          </QueryClientProvider>
        </AntApp>
      </ConfigProvider>
    </StrictMode>,
  );
})();
