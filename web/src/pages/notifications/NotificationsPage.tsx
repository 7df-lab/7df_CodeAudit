// 通知中心（14号 §3.2）：GET /v1/notifications?user_id=（当前用户）+ 标记已读
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Badge, Button, Card, List, Typography, message } from 'antd';
import dayjs from 'dayjs';
import { useState } from 'react';
import { api, errStatus } from '../../api/client';
import PageHeader from '../../components/PageHeader';
import { useSession } from '../../auth/session';
import { usePageTitle } from '../../hooks/usePageTitle';

interface Notification {
  notification_id: string;
  user_id: string;
  title: string;
  body: string;
  read: boolean;
  created_at: string | null;
}

export default function NotificationsPage() {
  usePageTitle('通知');
  const { user } = useSession();
  const qc = useQueryClient();
  const { data, isLoading } = useQuery({
    queryKey: ['notifications', user?.user_id],
    queryFn: async () =>
      (await api.get('/v1/notifications', { params: { user_id: user?.user_id ?? '' } })).data as {
        notifications: Notification[];
      },
    enabled: !!user,
  });

  const markRead = useMutation({
    mutationFn: async (id: string) => api.post(`/v1/notifications/${id}/read`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['notifications', user?.user_id] });
      // ADR-156: 同步失效导航角标缓存，已读后"（N 未读）"即时消失
      qc.invalidateQueries({ queryKey: ['notify-unread'] });
    },
    // B3-3（审计修复）：失败静默 → 页面现有 message.error 通道，携带状态码
    onError: (e) => {
      const status = errStatus(e);
      message.error(`标记已读失败${status ? `（HTTP ${status}）` : ''}：${(e as Error).message}`);
    },
  });

  // E-31a（engine ADR-222）: 一键全部已读——网关组合式端点，单请求替代 N 次逐条标记
  const markAllRead = useMutation({
    mutationFn: async () => api.post('/v1/notifications/read-all'),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['notifications', user?.user_id] });
      qc.invalidateQueries({ queryKey: ['notify-unread'] });
    },
    onError: (e) => {
      const status = errStatus(e);
      message.error(`全部已读失败${status ? `（HTTP ${status}）` : ''}：${(e as Error).message}`);
    },
  });

  const unread = (data?.notifications ?? []).filter((n) => !n.read).length;
  // 2026-09-09 GUI 评审: 通知会随任务数线性累积（回归跑批一次产生数十条），
  // 全量渲染出超长页面——客户端分页 + 条目时间显示。
  // 2026-10-08 报障"20条/页不能调整"：simple 分页无条数选择器且 PAGE_SIZE 写死——
  // 改标准分页器放开条数选择。仍为客户端分页：E-30 服务端返回全量（proto 的
  // pagination 字段在位但网关 transcode/storage 均未消费，服务端分页属跨仓改造），
  // 全量在手、total 真实，条数切换是诚实重切片而非假装。
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const all = data?.notifications ?? [];

  return (
    // ADR-156: 去定宽 maxWidth 720——与全站页面一致的全宽布局（1440 宽屏下此前右侧 51% 留白）
    <div>
      <PageHeader
        title={<>通知 <Badge count={unread} offset={[6, 0]} /></>}
        extra={(
          <Button
            size="small"
            disabled={unread === 0}
            loading={markAllRead.isPending}
            onClick={() => markAllRead.mutate()}
          >
            全部已读
          </Button>
        )}
      />
      <Card>
        {/* 设计整改 P2: 分页改 List 自带（此前手工拼接 Pagination 组件，样式与全站分裂） */}
        <List
          loading={isLoading}
          dataSource={all}
          pagination={{
            current: page,
            pageSize,
            total: all.length,
            // onChange 同时承接翻页与条数切换（antd 条数变化经 onShowSizeChange+onChange
            // 双发，页号可能被服务端语义收拢——以 onChange 到达的为准）
            onChange: (p, ps) => { setPage(p); setPageSize(ps); },
            showSizeChanger: true,
            pageSizeOptions: [10, 20, 50, 100],
            hideOnSinglePage: true,
          }}
          locale={{ emptyText: '暂无通知（扫描任务创建/完成时会产生通知）' }}
          renderItem={(n) => (
            <List.Item
              actions={[
                n.created_at && (
                  <Typography.Text key="t" type="secondary" style={{ fontSize: 12 }}>
                    {dayjs(n.created_at).format('YYYY-MM-DD HH:mm')}
                  </Typography.Text>
                ),
                n.read
                  ? <Typography.Text key="r" type="secondary">已读</Typography.Text>
                  : <Button key="m" size="small" onClick={() => markRead.mutate(n.notification_id)}>标记已读</Button>,
              ]}
            >
              <List.Item.Meta
                title={<span style={{ fontWeight: n.read ? 400 : 700 }}>{n.title || n.notification_id}</span>}
                description={n.body}
              />
            </List.Item>
          )}
        />
      </Card>
    </div>
  );
}
