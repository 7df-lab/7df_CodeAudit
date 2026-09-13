// 全站统一 加载/空/错 状态（2026-09-13）：此前"加载中…"纯文本 ×6、
// Table loading、Spin ×2、"会话恢复中…"四种形态并存；6 处 emptyText 文案风格不一；
// FusionView/ReviewView 查询失败无错误分支（永远停在加载文案）。
import { Alert, Button, Empty, Skeleton, Spin, Typography } from 'antd';
import type { ReactNode } from 'react';

// 页面级加载占位（替换纯文本"加载中…"）：居中 Spin + 可选说明；固定上下留白让
// 加载完成瞬间的布局跳变小于文字替换
export function PageLoading({ tip }: { tip?: string }) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 12, padding: '72px 0' }}>
      <Spin size="large" />
      {tip && <Typography.Text type="secondary">{tip}</Typography.Text>}
    </div>
  );
}

// 列表首屏骨架（首查无数据时替换 Table；有数据后的翻页 loading 仍走 Table 自带 loading）
export function ListSkeleton() {
  return <Skeleton active paragraph={{ rows: 6 }} />;
}

// 统一空态：Simple 插画 + 一句话说明 + 行动指引（description 讲清楚发生了什么，
// action 给出下一步入口——空屏是行动邀请，不是死胡同）
export function EmptyState({ description, action }: { description: ReactNode; action?: ReactNode }) {
  return (
    <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={description}>
      {action}
    </Empty>
  );
}

// 统一查询失败（页面级）：错误原因可见 + 重试出口
export function QueryError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  return (
    <Alert
      type="error"
      showIcon
      message="加载失败"
      description={`原因：${(error as Error | undefined)?.message ?? '未知'}（服务可能暂不可用）`}
      action={<Button size="small" onClick={onRetry}>重试</Button>}
    />
  );
}
