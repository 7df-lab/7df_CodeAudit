// 全站统一页首（2026-09-13）。
// 解决三件事：① antd Title 默认 margin-top 24 与 Content padding 24 叠加出 ~48px 空带
// （2026-09-12 人类反馈过，此前仅 TaskDetail 手工归零，其余页仍在）；② 页首工具栏间距
// 8/12/16 三档跳跃 → 统一 16；③ "标题+状态位 | 动作位"的 flex 结构各页手写。
// level：独立页 3（24px）；嵌套视图（表格展开行/Tab 内）用 4。
import { Typography } from 'antd';
import type { ReactNode } from 'react';

export default function PageHeader(
  { title, extra, level = 3 }: { title: ReactNode; extra?: ReactNode; level?: 3 | 4 | 5 },
) {
  return (
    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: 12, marginBottom: 16 }}>
      <Typography.Title level={level} style={{ margin: 0 }}>{title}</Typography.Title>
      {extra && <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>{extra}</div>}
    </div>
  );
}
