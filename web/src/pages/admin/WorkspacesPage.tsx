import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Badge, Button, Card, Drawer, Empty, Form, Input, Modal, Popconfirm,
  Space, Table, Tag, Tooltip, Typography, message,
} from 'antd';
import { useState } from 'react';
import { usePageTitle } from '../../hooks/usePageTitle';
import { useSession } from '../../auth/session';
import {
  createWorkspace, deleteWorkspace, getInferenceProviders, getInferenceRoute,
  getWorkspaces, type WorkspaceInfo,
} from '../../api/client';
import InferenceConfigPanel from './InferenceConfigPanel';

// 工作区管理——信息结构是本页的签名：
// 工作区不是普通 CRUD 行，是「team 上车状态机」（建区 → 配推理 → 就绪）；
// 推理配置是工作区的内嵌面（原独立「推理 Provider」页收敛于此，行内抽屉承载）——
// 上车状态机同屏闭环，跨页「到另一页右上角切区」的上下文搬运退役。
// 每行把就绪度显性化：Provider 数 + 当前路由模型（未配置=可行动的警示），
// default 的不可删语义显性化；删除遇资源守卫时 409 详情原文透出（唯一可操作信息）。

interface Row extends WorkspaceInfo {
  providerCount?: number;
  routeModel?: string;
}

export default function WorkspacesPage() {
  usePageTitle('工作区');
  const qc = useQueryClient();
  const { user: me } = useSession();
  const isAdmin = me?.role === 'ROLE_ADMIN';

  // 行内推理配置抽屉（父含子）：configWs=null 关闭；default 行 → 面板 workspace=''（全局默认）
  const [configWs, setConfigWs] = useState<string | null>(null);

  const wsQ = useQuery({
    queryKey: ['workspaces'],
    enabled: isAdmin,
    retry: false,
    queryFn: getWorkspaces,
  });
  const list = wsQ.data?.workspaces ?? [];

  // 就绪度：逐区拉 provider 数与当前路由（与 Provider 页同查询键空间，切区不缓存错位）
  const detailQ = useQuery({
    queryKey: ['workspaces-detail', list.map((w) => w.name).join(',')],
    enabled: isAdmin && list.length > 0,
    retry: false,
    queryFn: async () => {
      const out: Record<string, { providerCount?: number; routeModel?: string }> = {};
      await Promise.all(list.map(async (w) => {
        try {
          const [provs, route] = await Promise.all([
            getInferenceProviders(w.name === 'default' ? undefined : w.name),
            getInferenceRoute(w.name === 'default' ? undefined : w.name).catch(() => null),
          ]);
          out[w.name] = { providerCount: provs.providers.length, routeModel: route?.model };
        } catch {
          out[w.name] = {}; // 读不到推理配置不算页面失败：就绪度列显示未配置
        }
      }));
      return out;
    },
  });
  const rows: Row[] = list.map((w) => ({ ...w, ...(detailQ.data?.[w.name] ?? {}) }));

  // ---- 新建 ----
  const [formOpen, setFormOpen] = useState(false);
  const [form] = Form.useForm();
  const create = useMutation({
    mutationFn: (v: { name: string }) => createWorkspace({ name: v.name }),
    onSuccess: (r) => {
      message.success(`工作区 ${r.name} 已创建——在本行打开「推理配置」即可为其注册 Provider 与路由`);
      setFormOpen(false);
      form.resetFields();
      qc.invalidateQueries({ queryKey: ['workspaces'] });
    },
    onError: (e) => message.error(`创建失败：${(e as Error).message}`),
  });

  // ---- 删除（资源守卫 409 详情原文透出）----
  const remove = useMutation({
    mutationFn: (name: string) => deleteWorkspace(name),
    onSuccess: (r) => {
      message.success(r.deleted ? `工作区 ${r.name} 已删除` : `工作区 ${r.name} 不存在（可能已被删除）`);
      qc.invalidateQueries({ queryKey: ['workspaces'] });
    },
    onError: (e) => {
      // 服务端 409 原文即下一步操作指引（含哪些资源挡住删除）——不吞不改
      message.error({
        content: `删除被拒绝：${(e as Error).message}`,
        duration: 8,
      });
      qc.invalidateQueries({ queryKey: ['workspaces'] });
    },
  });

  if (!isAdmin) {
    return (
      <Card>
        <Typography.Text type="danger">403：仅管理员可访问工作区管理（ROLE_ADMIN）。</Typography.Text>
      </Card>
    );
  }

  const columns = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      render: (n: string) => n === 'default'
        ? <Space>{n}<Tag>全局默认</Tag></Space>
        : n,
    },
    {
      title: '推理配置',
      key: 'readiness',
      render: (_: unknown, r: Row) => {
        const parts = [
          r.providerCount !== undefined && `${r.providerCount} 个 Provider`,
          r.routeModel ? `路由 ${r.routeModel}` : null,
        ].filter(Boolean);
        const badge = !r.routeModel
          ? <Badge status="warning" text={parts.length ? parts.join(' · ') : '未配置（无路由）'} />
          : <Badge status="success" text={parts.join(' · ')} />;
        // 点击即进入该区推理配置（父含子主通道）；未配置态 Tooltip 给出行动指引
        return (
          <Tooltip title={r.routeModel ? '点击配置该工作区的推理' : r.name === 'default' ? '未配置时任务将无法调用模型——点击立即配置' : '未配置——点击为该团队配置 Provider 与路由'}>
            <Button type="link" size="small" style={{ padding: 0 }} onClick={() => setConfigWs(r.name)}>{badge}</Button>
          </Tooltip>
        );
      },
    },
    {
      title: '标签',
      dataIndex: 'labels',
      key: 'labels',
      render: (l: Record<string, string>) =>
        Object.keys(l ?? {}).length === 0
          ? <Typography.Text type="secondary">—</Typography.Text>
          : <Space size={4} wrap>{Object.entries(l).map(([k, v]) => <Tag key={k}>{k}={v}</Tag>)}</Space>,
    },
    {
      title: '操作',
      key: 'actions',
      render: (_: unknown, r: WorkspaceInfo) => (
        <Space>
          <Button size="small" onClick={() => setConfigWs(r.name)}>推理配置</Button>
          {r.name === 'default' ? (
            <Tooltip title="全局默认工作区，不可删除"><Button size="small" danger disabled>删除</Button></Tooltip>
          ) : (
            <Popconfirm
              title={`删除工作区「${r.name}」？`}
              description="区内含沙箱或推理配置时将被拒绝；删除后该团队任务将无法运行。"
              onConfirm={() => remove.mutate(r.name)}
            >
              <Button size="small" danger>删除</Button>
            </Popconfirm>
          )}
        </Space>
      ),
    },
  ];

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={16}>
      <Card
        title="团队工作区"
        extra={<Button type="primary" onClick={() => setFormOpen(true)}>新建工作区</Button>}
      >
        <Typography.Paragraph type="secondary" style={{ marginBottom: 12 }}>
          工作区按团队隔离推理配置：每个工作区独立注册 Provider、指定路由；
          项目的 team 字段决定任务运行在哪个工作区。
        </Typography.Paragraph>
        {list.length === 0 && !wsQ.isLoading ? (
          <Empty description="还没有工作区——建一个，在本行打开「推理配置」为它配模型" />
        ) : (
          <Table
            rowKey="name"
            size="middle"
            loading={wsQ.isLoading || detailQ.isLoading}
            columns={columns}
            dataSource={rows}
            pagination={false}
          />
        )}
      </Card>

      <Modal
        title="新建工作区"
        open={formOpen}
        onCancel={() => setFormOpen(false)}
        onOk={() => form.submit()}
        confirmLoading={create.isPending}
        destroyOnClose
      >
        <Typography.Paragraph type="secondary" style={{ fontSize: 12 }}>
          建区后在本行打开「推理配置」：注册 Provider 并切换路由，团队即可用。
        </Typography.Paragraph>
        <Form form={form} layout="vertical" onFinish={(v) => create.mutate(v)}>
          <Form.Item
            name="name"
            label="名称"
            rules={[
              { required: true, message: '请输入名称' },
              { pattern: /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/, message: '小写字母/数字/短横线（DNS-1123，与 team 名一致）' },
            ]}
            extra="与团队名恒等映射（ADR-247）；创建后即可在本行配置推理"
          >
            <Input placeholder="如 team-alpha" />
          </Form.Item>
        </Form>
      </Modal>

      {/* 父含子：行内推理配置抽屉（原独立 Provider 页主体，E-34..E-39 契约不变） */}
      <Drawer
        title={`推理配置 · ${configWs === 'default' ? 'default（全局默认）' : configWs ?? ''}`}
        open={configWs !== null}
        onClose={() => setConfigWs(null)}
        width={780}
        destroyOnClose
      >
        {configWs !== null && <InferenceConfigPanel workspace={configWs === 'default' ? '' : configWs} />}
      </Drawer>
    </Space>
  );
}
