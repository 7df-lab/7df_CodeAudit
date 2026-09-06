// 通知中心（14号 §3.2）：GET /v1/notifications?user_id=（当前用户）+ 标记已读
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Badge, Button, Card, List, Typography } from 'antd';
import { api } from '../../api/client';
import { useSession } from '../../auth/session';

interface Notification {
  notification_id: string;
  user_id: string;
  title: string;
  body: string;
  read: boolean;
  created_at: string | null;
}

export default function NotificationsPage() {
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
  });

  const unread = (data?.notifications ?? []).filter((n) => !n.read).length;

  return (
    // ADR-156: 去定宽 maxWidth 720——与全站页面一致的全宽布局（1440 宽屏下此前右侧 51% 留白）
    <div>
      <Typography.Title level={3}>
        通知 <Badge count={unread} offset={[6, 0]} />
      </Typography.Title>
      <Card>
        <List
          loading={isLoading}
          dataSource={data?.notifications ?? []}
          locale={{ emptyText: '暂无通知（事件通知依赖 Kafka 异步链路；内存演示模式下为空属预期）' }}
          renderItem={(n) => (
            <List.Item
              actions={
                n.read
                  ? [<Typography.Text key="r" type="secondary">已读</Typography.Text>]
                  : [<Button key="m" size="small" onClick={() => markRead.mutate(n.notification_id)}>标记已读</Button>]
              }
            >
              <List.Item.Meta
                title={n.title || n.notification_id}
                description={n.body}
              />
            </List.Item>
          )}
        />
      </Card>
    </div>
  );
}
