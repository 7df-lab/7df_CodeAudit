// 用户管理（14号 Q1/Q2 裁决：V1 只读+更新——proto 无 ListUsers/Register，页面提供
// 种子用户快捷入口 + 按ID查询；用户列表/自助注册待 proto V2.1）
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Descriptions, Input, Select, Space, Tag, Typography, message } from 'antd';
import { useState } from 'react';
import { api } from '../../api/client';
import { useSession } from '../../auth/session';

interface User {
  user_id: string;
  username: string;
  email: string;
  state: string;
  created_at: string | null;
}

const STATE_OPTIONS = [
  { value: 'USER_STATE_ACTIVE', label: '激活' },
  { value: 'USER_STATE_INACTIVE', label: '停用' },
  { value: 'USER_STATE_LOCKED', label: '锁定' },
];

export default function UsersPage() {
  const qc = useQueryClient();
  const { user: me } = useSession();
  const [userId, setUserId] = useState<string>(me?.user_id ?? 'user-001');

  const { data: user, isLoading, isError, refetch } = useQuery({
    queryKey: ['user', userId],
    retry: false,
    queryFn: async () => (await api.get(`/v1/users/${userId}`)).data as User,
    enabled: !!userId,
  });
  const { data: perms } = useQuery({
    queryKey: ['user-perms', userId],
    retry: false,
    queryFn: async () => (await api.get(`/v1/users/${userId}/permissions`)).data as { permissions: string[] },
    enabled: !!userId,
  });

  const update = useMutation({
    mutationFn: async (patch: { state?: string }) => {
      const cur = user!;
      return api.put(`/v1/users/${cur.user_id}`, {
        user: {
          user_id: cur.user_id,
          username: cur.username,
          email: cur.email,
          state: patch.state ?? cur.state,
        },
      });
    },
    onSuccess: () => {
      message.success('用户已更新');
      qc.invalidateQueries({ queryKey: ['user', userId] });
    },
    onError: (e) => message.error(`更新失败：${(e as Error).message}`),
  });

  return (
    // ADR-156: 去定宽 maxWidth 760——与全站页面一致的全宽布局（1440 宽屏下此前右侧 47% 留白）
    <div>
      <Typography.Title level={3}>用户管理</Typography.Title>
      <Typography.Paragraph type="secondary">
        V1 口径（Q1/Q2 裁决）：只读 + 更新；用户列表与自助注册待 proto V2.1。种子用户：user-001 (admin)。
      </Typography.Paragraph>

      <Space style={{ marginBottom: 16 }} wrap>
        <Input
          style={{ width: 260 }}
          placeholder="输入 user_id 查询"
          value={userId}
          onChange={(e) => setUserId(e.target.value)}
        />
        <Button onClick={() => refetch()}>查询</Button>
        {me && (
          <Button type="link" onClick={() => setUserId(me.user_id)}>
            我（{me.user_id}）
          </Button>
        )}
      </Space>

      {isError ? (
        <Typography.Text type="warning">用户不存在或查询失败（proto 无 List RPC，需精确 user_id）</Typography.Text>
      ) : isLoading || !user ? (
        <Typography.Text type="secondary">加载中…</Typography.Text>
      ) : (
        <>
          <Card style={{ marginBottom: 16 }}>
            <Descriptions column={2} size="small">
              <Descriptions.Item label="用户ID">{user.user_id}</Descriptions.Item>
              <Descriptions.Item label="用户名">{user.username}</Descriptions.Item>
              <Descriptions.Item label="邮箱">{user.email || '—'}</Descriptions.Item>
              <Descriptions.Item label="状态">
                <Tag>{user.state?.replace('USER_STATE_', '') || '未知'}</Tag>
              </Descriptions.Item>
              <Descriptions.Item label="创建时间">{user.created_at ?? '—'}</Descriptions.Item>
            </Descriptions>
          </Card>
          <Card title="权限（UserService.GetUserPermissions）" style={{ marginBottom: 16 }}>
            <Space wrap>
              {(perms?.permissions ?? []).map((p) => (
                <Tag key={p} color="blue">{p}</Tag>
              ))}
              {(!perms?.permissions || perms.permissions.length === 0) && (
                <Typography.Text type="secondary">无权限记录</Typography.Text>
              )}
            </Space>
          </Card>
          <Card title="更新用户（UpdateUser）">
            <Space wrap>
              <Select
                style={{ width: 160 }}
                value={user.state || 'USER_STATE_ACTIVE'}
                options={STATE_OPTIONS}
                onChange={(v) => update.mutate({ state: v })}
              />
              <Button loading={update.isPending} onClick={() => update.mutate({})}>
                保存当前资料
              </Button>
            </Space>
            <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>
              状态选择即保存（UpdateUser 全量覆盖，服务端保留空字段）。
            </Typography.Paragraph>
          </Card>
        </>
      )}
    </div>
  );
}
