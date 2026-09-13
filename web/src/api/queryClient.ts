// QueryClient 模块单例（B5-P2-6）：main.tsx 装配与 auth/session 登出清理共用同一实例。
// 此前 qc 在 main.tsx 模块内创建且不导出——登出只清 token 不清缓存，软登出（SPA 内跳
// /login）后TanStack 缓存留存至 gcTime：共享机器上另一账号登录会先见到上一账号的列表
// 数据（401 硬跳转路径因整页刷新自然清空，两条登出路径行为不一致）。
import { QueryClient } from '@tanstack/react-query';

export const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 1, refetchOnWindowFocus: false } },
});
