// 用户管理页（14号 §3.3 P1 / ADR-205 V2.1）：Q1/Q2 裁决的 V2 形态——
// 列表（GET /v1/users，游标"加载更多"）+ 搜索/状态过滤 + 管理员建号（POST /v1/users）
// + 启用/停用（PUT /v1/users/{id}，复用既有更新通道）+ 重置密码（POST password:reset）。
// 非 admin 由 App 路由守卫与本页双重拦截（后端网关 requireAdmin 是最终防线）。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Descriptions, Form, Input, Modal, Select, Space, Table, Tag, Typography, message } from 'antd';
import { useState } from 'react';
import { api } from '../../api/client';
import { useSession } from '../../auth/session';
import { ROLE, USER_STATE } from '../../dict';

interface User {
  user_id: string;
  username: string;
  email: string;
  state: string;
  role?: string;
  must_change_password?: boolean;
  created_at: string | null;
}

interface ListResp {
  users: User[];
  pagination?: { next_cursor?: string; has_next?: boolean; total?: number };
}

export default function UsersPage() {
  const qc = useQueryClient();
  const { user: me } = useSession();
  const isAdmin = me?.role === 'ROLE_ADMIN';
  const [search, setSearch] = useState('');
  const [usernameContains, setUsernameContains] = useState('');
  const [stateFilter, setStateFilter] = useState<string | undefined>();
  const [cursor, setCursor] = useState<string>('');

  const listKey = ['users', usernameContains, stateFilter ?? '', cursor] as const;
  const { data, isLoading, isError, error, refetch } = useQuery({
    queryKey: listKey,
    enabled: isAdmin,
    retry: false,
    queryFn: async () => {
      const resp = await api.get<ListResp>('/v1/users', {
        params: {
          pagination: cursor ? { cursor } : undefined,
          username_contains: usernameContains || undefined,
          state: stateFilter || undefined,
        },
      });
      return resp.data;
    },
  });

  // 建号
  const [createOpen, setCreateOpen] = useState(false);
  const [createForm] = Form.useForm();
  const create = useMutation({
    mutationFn: async (v: { username: string; email: string; password: string; role?: string }) => {
      const resp = await api.post('/v1/users', {
        username: v.username,
        email: v.email,
        password: v.password,
        role: v.role || undefined,
      });
      return resp.data;
    },
    onSuccess: () => {
      message.success('用户已创建（首登须改密；临时密码如需下发请用「重置密码」）');
      setCreateOpen(false);
      createForm.resetFields();
      qc.invalidateQueries({ queryKey: ['users'] });
    },
    onError: (e) => message.error(`创建失败：${(e as Error).message}`),
  });

  // 启用/停用（PUT 全量 user，Q2a 既有契约）
  const toggleState = useMutation({
    mutationFn: async (u: User) => {
      const next = u.state === 'USER_STATE_ACTIVE' ? 'USER_STATE_INACTIVE' : 'USER_STATE_ACTIVE';
      await api.put(`/v1/users/${u.user_id}`, {
        user: {
          user_id: u.user_id,
          username: u.username,
          email: u.email,
          state: next,
        },
      });
      return next;
    },
    onSuccess: () => {
      message.success('状态已更新');
      refetch();
    },
    onError: (e) => message.error(`更新失败：${(e as Error).message}`),
  });

  // 重置密码：一次性临时密码仅在响应中返回一次（ADR-205）
  const [tempPw, setTempPw] = useState<{ username: string; password: string } | null>(null);
  const resetPw = useMutation({
    mutationFn: async (u: User) => {
      const resp = await api.post(`/v1/users/${u.user_id}/password:reset`, {});
      return resp.data as { temporary_password: string };
    },
    onSuccess: (r, u) => {
      setTempPw({ username: u.username, password: r.temporary_password });
      refetch();
    },
    onError: (e) => message.error(`重置失败：${(e as Error).message}`),
  });

  if (!isAdmin) {
    return (
      <Card>
        <Typography.Text type="danger">403：仅管理员可访问用户管理（ROLE_ADMIN，ADR-205）。</Typography.Text>
      </Card>
    );
  }

  const rows = data?.users ?? [];
  const columns = [
    { title: '用户名', dataIndex: 'username', key: 'username' },
    { title: '邮箱', dataIndex: 'email', key: 'email' },
    {
      title: '角色',
      dataIndex: 'role',
      key: 'role',
      render: (r?: string) => (
        <Tag color={r === 'ROLE_ADMIN' ? 'gold' : r === 'ROLE_DEVELOPER' ? 'blue' : 'default'}>{ROLE[r ?? ''] ?? r ?? '—'}</Tag>
      ),
    },
    {
      title: '状态',
      dataIndex: 'state',
      key: 'state',
      render: (s: string) => (
        <Tag color={s === 'USER_STATE_ACTIVE' ? 'green' : s === 'USER_STATE_LOCKED' ? 'red' : 'default'}>{USER_STATE[s] ?? s}</Tag>
      ),
    },
    {
      title: '待改密',
      dataIndex: 'must_change_password',
      key: 'mcp',
      render: (v?: boolean) => (v ? <Tag color="orange">首登须改密</Tag> : '—'),
    },
    {
      title: '操作',
      key: 'actions',
      render: (_: unknown, u: User) => (
        <Space>
          <Button
            size="small"
            onClick={() => toggleState.mutate(u)}
            disabled={u.user_id === me?.user_id}
            title={u.user_id === me?.user_id ? '不能停用自己' : undefined}
          >
            {u.state === 'USER_STATE_ACTIVE' ? '停用' : '启用'}
          </Button>
          <Button size="small" onClick={() => resetPw.mutate(u)}>
            重置密码
          </Button>
        </Space>
      ),
    },
  ];

  return (
    <Card
      title="用户管理"
      extra={
        <Space>
          <Input.Search
            placeholder="按用户名搜索"
            allowClear
            style={{ width: 200 }}
            onSearch={(v) => {
              setCursor('');
              setUsernameContains(v);
            }}
            onChange={(e) => setSearch(e.target.value)}
            value={search}
          />
          <Select
            allowClear
            placeholder="全部状态"
            style={{ width: 120 }}
            options={Object.entries(USER_STATE).map(([value, label]) => ({ value, label }))}
            onChange={(v) => {
              setCursor('');
              setStateFilter(v);
            }}
          />
          <Button type="primary" onClick={() => setCreateOpen(true)}>
            新建用户
          </Button>
        </Space>
      }
    >
      {isError && <Typography.Text type="danger">加载失败：{(error as Error).message}</Typography.Text>}
      <Table
        rowKey="user_id"
        size="small"
        loading={isLoading}
        dataSource={rows}
        columns={columns}
        pagination={false}
        footer={() =>
          data?.pagination?.has_next ? (
            <Button size="small" onClick={() => setCursor(data.pagination!.next_cursor ?? '')}>
              加载更多（已列 {rows.length}/{data.pagination?.total ?? '?'}）
            </Button>
          ) : (
            <Typography.Text type="secondary">共 {data?.pagination?.total ?? rows.length} 个用户</Typography.Text>
          )
        }
      />

      <Modal
        title="新建用户"
        open={createOpen}
        onCancel={() => setCreateOpen(false)}
        onOk={() => createForm.submit()}
        confirmLoading={create.isPending}
        destroyOnClose
      >
        <Form form={createForm} layout="vertical" onFinish={(v) => create.mutate(v)}>
          <Form.Item
            name="username"
            label="用户名"
            rules={[
              { required: true, message: '请输入用户名' },
              { pattern: /^[a-zA-Z0-9_-]{3,32}$/, message: '3-32 位字母/数字/下划线/短横线' },
            ]}
          >
            <Input />
          </Form.Item>
          <Form.Item
            name="email"
            label="邮箱"
            rules={[{ required: true, message: '请输入邮箱' }, { type: 'email', message: '邮箱格式不正确' }]}
          >
            <Input />
          </Form.Item>
          <Form.Item
            name="password"
            label="初始密码"
            extra="一次性临时密码语义：用户首登须改密"
            rules={[
              { required: true, message: '请输入初始密码' },
              { min: 8, message: '至少 8 位' },
            ]}
          >
            <Input.Password autoComplete="new-password" />
          </Form.Item>
          <Form.Item name="role" label="角色" initialValue="ROLE_DEVELOPER">
            <Select
              options={Object.entries(ROLE)
                .filter(([value]) => value !== 'ROLE_UNSPECIFIED')
                .map(([value, label]) => ({ value, label }))}
            />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        open={!!tempPw}
        title={`重置成功：${tempPw?.username ?? ''} 的一次性临时密码`}
        onCancel={() => setTempPw(null)}
        onOk={() => setTempPw(null)}
        okText="我已保存"
      >
        <Descriptions column={1} size="small" bordered>
          <Descriptions.Item label="临时密码">
            <Typography.Text copyable code style={{ fontSize: 16 }}>
              {tempPw?.password}
            </Typography.Text>
          </Descriptions.Item>
        </Descriptions>
        <Typography.Paragraph type="warning" style={{ marginTop: 12, marginBottom: 0 }}>
          仅此一次显示，关闭后无法再次查看。用户下次登录须使用该密码并强制设置新密码。
        </Typography.Paragraph>
      </Modal>
    </Card>
  );
}
