// 推理 Provider 管理页（ADR-217）：AI 推理 provider 增删改查 + 工作区推理路由查看/切换。
// 链路：/v1/inference/*（admin 面）→ engine → openshell-manager → OpenShell 网关
// （provider 权威存储 gateway.db，凭据加密）。凭据只进不出：编辑时留空提交，
// 已存凭据不可见（服务端读路径本就不回流凭据）。
// 生效语义：provider/路由变更影响下一个任务的 AI 阶段，运行中任务不受影响。
// 非 admin 由 App 路由守卫与本页双重拦截（后端网关 requireAdmin 是最终防线）。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  AutoComplete, Button, Card, Descriptions, Form, Input, Modal, Popconfirm,
  Select, Space, Switch, Table, Tag, Tooltip, Typography, message,
} from 'antd';
import { useState } from 'react';
import {
  createInferenceProvider, deleteInferenceProvider, getInferenceProviders,
  getInferenceRoute, setInferenceRoute, updateInferenceProvider,
} from '../../api/client';
import type { InferenceProvider } from '../../api/types';
import { useSession } from '../../auth/session';

// 键值对表单行（credentials/config 两个 map<string,string> 的编辑形态）
interface KVRow {
  key: string;
  value: string;
}

function rowsToMap(rows: KVRow[] | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const r of rows ?? []) {
    const k = r.key?.trim();
    const v = r.value ?? '';
    // 只提交填完整的行：值留空的行整体丢弃——编辑凭据全留空 → {} = 清空已存凭据
    if (k && v !== '') out[k] = v;
  }
  return out;
}

function KVEditor({ name, valueLabel, password }: { name: string; valueLabel: string; password?: boolean }) {
  return (
    <Form.List name={name}>
      {(fields, { add, remove }) => (
        <>
          {fields.map((f) => (
            <Space key={f.key} style={{ display: 'flex', marginBottom: 4 }} align="baseline">
              <Form.Item name={[f.name, 'key']} rules={[{ required: true, message: '键必填' }]} style={{ marginBottom: 0 }}>
                <Input placeholder="键（如 api_key）" style={{ width: 180 }} />
              </Form.Item>
              <Form.Item name={[f.name, 'value']} style={{ marginBottom: 0 }}>
                {password ? <Input.Password placeholder={valueLabel} autoComplete="new-password" /> : <Input placeholder={valueLabel} />}
              </Form.Item>
              <Button type="link" danger size="small" onClick={() => remove(f.name)}>移除</Button>
            </Space>
          ))}
          <Button type="dashed" size="small" onClick={() => add({ key: '', value: '' })} style={{ width: '100%' }}>
            + 添加一行
          </Button>
        </>
      )}
    </Form.List>
  );
}

export default function ProvidersPage() {
  const qc = useQueryClient();
  const { user: me } = useSession();
  const isAdmin = me?.role === 'ROLE_ADMIN';

  const providersQ = useQuery({
    queryKey: ['inference-providers'],
    enabled: isAdmin,
    retry: false,
    queryFn: getInferenceProviders,
  });
  const routeQ = useQuery({
    queryKey: ['inference-route'],
    enabled: isAdmin,
    retry: false,
    queryFn: getInferenceRoute,
  });

  const providers = providersQ.data?.providers ?? [];
  const route = routeQ.data;

  // ---- 新建/编辑（共用表单；upsert 语义，created 由服务端判定）----
  const [editing, setEditing] = useState<InferenceProvider | null>(null); // null=新建
  const [formOpen, setFormOpen] = useState(false);
  const [form] = Form.useForm();
  const save = useMutation({
    mutationFn: async (v: {
      name: string; type: string; credentials: KVRow[]; config: KVRow[];
    }) => {
      const payload = {
        type: v.type,
        credentials: rowsToMap(v.credentials),
        config: rowsToMap(v.config),
      };
      return editing
        ? updateInferenceProvider(editing.name, payload)
        : createInferenceProvider(v.name, payload);
    },
    onSuccess: (r) => {
      message.success(r.created ? `Provider「${r.name}」已创建` : `Provider「${r.name}」已更新`);
      setFormOpen(false);
      form.resetFields();
      qc.invalidateQueries({ queryKey: ['inference-providers'] });
    },
    onError: (e) => message.error(`保存失败：${(e as Error).message}`),
  });

  const openCreate = () => {
    setEditing(null);
    form.resetFields();
    form.setFieldsValue({
      name: '', type: '', credentials: [{ key: 'api_key', value: '' }], config: [{ key: 'base_url', value: '' }],
    });
    setFormOpen(true);
  };
  const openEdit = (p: InferenceProvider) => {
    setEditing(p);
    form.resetFields();
    form.setFieldsValue({
      name: p.name,
      type: p.type,
      // 凭据不可见（服务端不回流）：预置空行待填
      credentials: [{ key: 'api_key', value: '' }],
      config: Object.entries(p.config).map(([key, value]) => ({ key, value })),
    });
    setFormOpen(true);
  };

  // ---- 删除（在用 provider 先切换路由）----
  const remove = useMutation({
    mutationFn: (name: string) => deleteInferenceProvider(name),
    onSuccess: (r) => {
      message.success(r.deleted ? 'Provider 已删除' : 'Provider 不存在（可能已被删除）');
      qc.invalidateQueries({ queryKey: ['inference-providers'] });
    },
    onError: (e) => message.error(`删除失败：${(e as Error).message}`),
  });

  // ---- 切换路由（自带连通性验证回执）----
  const [routeOpen, setRouteOpen] = useState(false);
  const [routeForm] = Form.useForm();
  const setRoute = useMutation({
    mutationFn: setInferenceRoute,
    onSuccess: (r) => {
      const endpoints = r.validated_endpoints.map((e) => e.url).join('、');
      message.success(
        r.validation_performed
          ? `路由已切换（${r.provider} / ${r.model}），连通性验证通过：${endpoints || '（无端点回执）'}`
          : `路由已切换（${r.provider} / ${r.model}，未验证连通性）`,
      );
      setRouteOpen(false);
      qc.invalidateQueries({ queryKey: ['inference-route'] });
      qc.invalidateQueries({ queryKey: ['inference-providers'] });
    },
    onError: (e) => message.error(`切换失败（路由未变更）：${(e as Error).message}`),
  });

  if (!isAdmin) {
    return (
      <Card>
        <Typography.Text type="danger">403：仅管理员可访问推理 Provider 管理（ROLE_ADMIN，ADR-217）。</Typography.Text>
      </Card>
    );
  }

  const columns = [
    { title: '名称', dataIndex: 'name', key: 'name' },
    {
      title: '类型',
      dataIndex: 'type',
      key: 'type',
      render: (t: string) => <Tag color="geekblue">{t || '—'}</Tag>,
    },
    {
      title: '配置',
      dataIndex: 'config',
      key: 'config',
      render: (c: Record<string, string>) =>
        Object.keys(c ?? {}).length === 0
          ? <Typography.Text type="secondary">—</Typography.Text>
          : <Typography.Text code>{Object.entries(c).map(([k, v]) => `${k}=${v}`).join('；')}</Typography.Text>,
    },
    {
      title: '当前路由',
      key: 'in-use',
      render: (_: unknown, p: InferenceProvider) =>
        route?.provider === p.name ? <Tag color="green">当前使用</Tag> : null,
    },
    {
      title: '操作',
      key: 'actions',
      render: (_: unknown, p: InferenceProvider) => {
        const inUse = route?.provider === p.name;
        const del = (
          <Button size="small" danger disabled={inUse}>
            删除
          </Button>
        );
        return (
          <Space>
            <Button size="small" onClick={() => openEdit(p)}>编辑</Button>
            {inUse ? (
              <Tooltip title="该 Provider 正被当前推理路由使用，请先切换路由">{del}</Tooltip>
            ) : (
              <Popconfirm title={`确认删除 Provider「${p.name}」？`} onConfirm={() => remove.mutate(p.name)}>
                {del}
              </Popconfirm>
            )}
          </Space>
        );
      },
    },
  ];

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={16}>
      <Card title="当前推理路由" extra={<Button onClick={() => { routeForm.resetFields(); setRouteOpen(true); }}>切换路由</Button>}>
        <Descriptions column={3} size="small" bordered>
          <Descriptions.Item label="Provider">{route?.provider || <Typography.Text type="secondary">未设置</Typography.Text>}</Descriptions.Item>
          <Descriptions.Item label="模型">{route?.model || <Typography.Text type="secondary">未设置</Typography.Text>}</Descriptions.Item>
          <Descriptions.Item label="版本">{route?.version ?? '—'}</Descriptions.Item>
        </Descriptions>
        <Typography.Paragraph type="secondary" style={{ marginBottom: 0, marginTop: 8 }}>
          路由决定下一个任务 AI 阶段的推理出口（沙箱内由网关注入凭据，运行中任务不受切换影响）。
        </Typography.Paragraph>
      </Card>

      <Card
        title="Provider 列表"
        extra={<Button type="primary" onClick={openCreate}>新建 Provider</Button>}
      >
        <Table
          rowKey="name"
          size="small"
          loading={providersQ.isLoading}
          dataSource={providers}
          columns={columns}
          pagination={false}
          locale={{
            emptyText: providersQ.isError
              ? '加载失败（推理服务不可用或权限不足）'
              : '暂无 Provider，点击右上角新建',
          }}
        />
      </Card>

      <Modal
        title={editing ? `编辑 Provider：${editing.name}` : '新建 Provider'}
        open={formOpen}
        onCancel={() => setFormOpen(false)}
        onOk={() => form.submit()}
        confirmLoading={save.isPending}
        destroyOnClose
      >
        <Form form={form} layout="vertical" onFinish={(v) => save.mutate(v)}>
          <Form.Item
            name="name"
            label="名称"
            rules={[{ required: true, message: '请输入名称' }, { pattern: /^[a-zA-Z0-9_-]{1,64}$/, message: '1-64 位字母/数字/下划线/短横线' }]}
            extra={editing ? '名称不可修改（删除后重建可改名）' : undefined}
          >
            <Input disabled={!!editing} placeholder="如 zhipu-main" />
          </Form.Item>
          <Form.Item
            name="type"
            label="类型"
            rules={[{ required: true, message: '请输入类型' }]}
            extra="Provider 类型串（以 OpenShell 网关支持的类型为准）"
          >
            <AutoComplete
              options={['anthropic', 'openai', 'openai-compatible', 'deepseek', 'zhipu'].map((t) => ({ value: t }))}
              placeholder="如 anthropic"
            />
          </Form.Item>
          <Form.Item
            label="凭据（credentials）"
            required
            extra={editing
              ? '已存凭据不可见；本次填写将整体覆盖，留空即清空已存凭据'
              : '仅写入网关加密存储，保存后任何页面不再显示'}
          >
            <KVEditor name="credentials" valueLabel="凭据值（如 sk-…）" password />
          </Form.Item>
          <Form.Item label="配置（config）" extra="如 base_url 等，取决于 Provider 类型">
            <KVEditor name="config" valueLabel="配置值" />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title="切换推理路由"
        open={routeOpen}
        onCancel={() => setRouteOpen(false)}
        onOk={() => routeForm.submit()}
        confirmLoading={setRoute.isPending}
        destroyOnClose
      >
        <Form
          form={routeForm}
          layout="vertical"
          onFinish={(v) => setRoute.mutate({ provider: v.provider, model: v.model, no_verify: !v.verify })}
        >
          <Form.Item name="provider" label="Provider" rules={[{ required: true, message: '请选择 Provider' }]}>
            <Select
              options={providers.map((p) => ({ value: p.name, label: `${p.name}（${p.type}）` }))}
              placeholder="选择 Provider"
            />
          </Form.Item>
          <Form.Item
            name="model"
            label="模型"
            rules={[{ required: true, message: '请输入模型 ID' }]}
            extra="模型目录在推理服务侧，此处自由填写；保存时的连通性验证会校验组合是否可用"
          >
            <Input placeholder="如 glm-5.3-flash / deepseek-v4-flash" />
          </Form.Item>
          <Form.Item name="verify" label="保存时验证连通性" valuePropName="checked" initialValue={true}>
            <Switch />
          </Form.Item>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 0, fontSize: 12 }}>
            验证由网关实测推理端点；失败则路由不生效（诚实失败，原路由保持）。
          </Typography.Paragraph>
        </Form>
      </Modal>
    </Space>
  );
}
