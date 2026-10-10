// 推理 Provider 管理页（ADR-217）：AI 推理 provider 增删改查 + 工作区推理路由查看/切换。
// 链路：/v1/inference/*（admin 面）→ engine → openshell-manager → OpenShell 网关
// （provider 权威存储 gateway.db，凭据加密）。凭据只进不出：GET 永不回流 credentials。
// 网关行为契约（2026-09-17 107 镜像 b448ef29 实测钉死，旧页面注释有多处失实已纠）：
//   ① 类型仅六种：openai/anthropic/nvidia/deepinfra/google-vertex-ai/aws-bedrock
//     （网关 INVALID_ARGUMENT details 原文；zhipu/deepseek 等是厂商不是类型）；
//   ② 创建后类型不可改（网关 "type cannot be changed; delete and recreate"）→ 编辑态锁类型；
//   ③ 空 credentials={} 更新=保留已存凭据（实测同 config 下 credentials:{} 提交后
//     路由验证仍过）——"留空=清除"是旧注释的失实断言，防误清确认弹窗随之取消；
//   ④ 端点键约定：openai 型 OPENAI_BASE_URL/OPENAI_API_KEY；anthropic 型
//     ANTHROPIC_BASE_URL/API_KEY（旧大写 BASE_URL 已被新镜像无视，回落官方端点
//     区域 403；小写 base_url 亦可）。BASE_URL 须 https（ADR-228 边界仍在）。
// 生效语义：provider/路由变更影响下一个任务的 AI 阶段，运行中任务不受影响。
// 非 admin 由 App 路由守卫与本页双重拦截（后端网关 requireAdmin 是最终防线）。
// 推理配置面板（原独立 ProvidersPage 主体，ADR-217 契约不变；2026-10-05 父含子合并）：
// 工作区 = 一等治理对象（team 上车状态机：建区→配推理→就绪），推理配置为其内嵌面。
// 上线状态机同屏闭环——原「到另一页右上角切区」的跨页上下文搬运退役。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Button, Card, Descriptions, Form, Input, Modal, Popconfirm,
  Select, Space, Switch, Table, Tag, Tooltip, Typography, message,
} from 'antd';
import { useState } from 'react';
import {
  createInferenceProvider, deleteInferenceProvider, getInferenceProviders,
  getInferenceRoute, setInferenceRoute, updateInferenceProvider,
} from '../../api/client';
import type { InferenceProvider } from '../../api/types';

// 键值对表单行（credentials/config 两个 map<string,string> 的编辑形态）
interface KVRow {
  key: string;
  value: string;
}

// 网关支持的 provider 类型（2026-09-17 网关 INVALID_ARGUMENT details 原文；
// 曾把 zhipu/deepseek 当类型列出是文档失实——它们是厂商，走 openai/anthropic 兼容协议）
const TYPE_OPTIONS = [
  'openai',
  'anthropic',
  'nvidia',
  'deepinfra',
  'google-vertex-ai',
  'aws-bedrock',
].map((t) => ({ value: t }));

// 提示文案从简（2026-09-17 人类反馈二轮：备注过于啰嗦）——键约定细节由表单
// 预置与模板承担，不再在 extra 里展开；每条 extra 一行短句，不重复出现。
const KEY_CONVENTION_HINT = '键名已按类型预置（网关约定键）；改用其他键将不被识别';
const OPENAI_PRESET = { cred: 'OPENAI_API_KEY', conf: 'OPENAI_BASE_URL' };
const ANTHROPIC_PRESET = { cred: 'API_KEY', conf: 'ANTHROPIC_BASE_URL' };
// anthropic 型用独立约定键，其余已支持型共用 openai 系约定键（未选/未知型回退 openai 系默认）
function presetKeysForType(type: string): { cred: string; conf: string } {
  return type === 'anthropic' ? ANTHROPIC_PRESET : OPENAI_PRESET;
}

// 厂商模板（2026-09-17 三轮：类型六选且创建后不可改——约束由模板层吸收，
// 模板即选型入口，覆盖全部六型；类型自动带出，用户只贴 key）。
// base 预填原则：仅预填已实证/官方公开的 OpenAI/Anthropic 兼容端点；
// nvidia/deepinfra/vertex/bedrock 的键约定未实证——只带类型与键位，不编造端点
//（模板标签注明端点手填，端点错误由切路由验证兜底并给可行动报错）。
interface ProviderTemplate {
  key: string;
  label: string;
  type: string;
  base: string; // 预填端点（空=该厂商端点手填）
}
const TEMPLATES: ProviderTemplate[] = [
  {
    key: 'zhipu-glm', label: '智谱 GLM（Anthropic 兼容）', type: 'anthropic',
    base: 'https://open.bigmodel.cn/api/anthropic',
  },
  {
    key: 'anthropic', label: 'Anthropic 官方', type: 'anthropic',
    base: 'https://api.anthropic.com',
  },
  {
    key: 'deepseek', label: 'DeepSeek（OpenAI 兼容）', type: 'openai',
    base: 'https://api.deepseek.com',
  },
  { key: 'openai', label: 'OpenAI', type: 'openai', base: 'https://api.openai.com' },
  { key: 'nvidia', label: 'NVIDIA NIM（端点手填）', type: 'nvidia', base: '' },
  { key: 'deepinfra', label: 'DeepInfra（端点手填）', type: 'deepinfra', base: '' },
  { key: 'gve', label: 'Google Vertex AI（端点手填）', type: 'google-vertex-ai', base: '' },
  { key: 'bedrock', label: 'AWS Bedrock（端点手填）', type: 'aws-bedrock', base: '' },
  { key: 'custom', label: '自定义（手填）', type: '', base: '' },
];

function rowsToMap(rows: KVRow[] | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const r of rows ?? []) {
    const k = r.key?.trim();
    const v = r.value ?? '';
    // 只提交填完整的行：值留空的行整体丢弃——编辑凭据全留空 → credentials:{}
    // = 保留已存凭据（2026-09-17 网关实测；服务端不回显，无法知道是否已存）
    if (k && v !== '') out[k] = v;
  }
  return out;
}

function KVEditor({ name, keyPlaceholder, valueLabel, password }:
{ name: string; keyPlaceholder: string; valueLabel: string; password?: boolean }) {
  return (
    <Form.List name={name}>
      {(fields, { add, remove }) => (
        <>
          {fields.map((f) => (
            // 弹性行（2026-09-17 人类反馈：弹窗内值输入框被挤到 ~110px，凭据看不全）：
            // 键固定 240px，值吃满剩余宽度；Space 不提供 flex 布局，弃用
            <div key={f.key} style={{ display: 'flex', gap: 8, marginBottom: 4, alignItems: 'baseline' }}>
              <Form.Item name={[f.name, 'key']} rules={[{ required: true, message: '键必填' }]} style={{ marginBottom: 0, width: 240 }}>
                <Input placeholder={keyPlaceholder} />
              </Form.Item>
              <Form.Item name={[f.name, 'value']} style={{ marginBottom: 0, flex: 1, minWidth: 0 }}>
                {password
                  ? <Input.Password placeholder={valueLabel} autoComplete="new-password" visibilityToggle />
                  : <Input placeholder={valueLabel} />}
              </Form.Item>
              <Button type="link" danger size="small" onClick={() => remove(f.name)}>移除</Button>
            </div>
          ))}
          <Button type="dashed" size="small" onClick={() => add({ key: '', value: '' })} style={{ width: '100%' }}>
            + 添加一行
          </Button>
        </>
      )}
    </Form.List>
  );
}

// 父含子合并批（2026-10-05 用户裁决）：原独立「推理 Provider」页主体收敛为本面板，
// 由工作区页按行内嵌（抽屉）——上下文（workspace）由宿主给定，不再自持切区选择器。
// workspace 空串=default 全局默认（E-34..E-39 的 ?workspace= 语义不变）。
export default function InferenceConfigPanel({ workspace }: { workspace: string }) {
  const qc = useQueryClient();
  const ws = workspace;
  const providersQ = useQuery({
    queryKey: ['inference-providers', ws],
    retry: false,
    queryFn: () => getInferenceProviders(ws),
  });
  const routeQ = useQuery({
    queryKey: ['inference-route', ws],
    retry: false,
    queryFn: () => getInferenceRoute(ws),
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
        ? updateInferenceProvider(editing.name, payload, ws)
        : createInferenceProvider(v.name, payload, ws);
    },
    // 凭据语义动态文案：服务端永不回显 credentials（GET 响应无该键），用户必须
    // 被告知凭据发生了什么——创建=写入；编辑非空=覆盖生效；编辑全空=保留已存
    //（2026-09-17 网关实测：credentials:{} 更新不清除，旧"留空=清除"为失实断言）
    onSuccess: (r, v) => {
      const filled = (v.credentials ?? []).some((row) => (row.value ?? '') !== '');
      const toast = r.created
        ? `Provider「${r.name}」已创建；凭据已写入（服务端不再回显）`
        : filled
          ? `Provider「${r.name}」已更新；新凭据已生效`
          : `Provider「${r.name}」已更新；已存凭据保留未变（留空=保留）`;
      message.success(toast);
      setFormOpen(false);
      form.resetFields();
      qc.invalidateQueries({ queryKey: ['inference-providers'] });
      qc.invalidateQueries({ queryKey: ['workspaces-detail'] }); // 就绪度列联动
    },
    onError: (e) => message.error(`保存失败：${(e as Error).message}`),
  });

  // 提交闸门：仅新建要求凭据非空（无凭据 provider 无法通过任何验证）；
  // 编辑留空=保留已存凭据（网关实测语义），无需确认弹窗。
  const onFormFinish = (v: { name: string; type: string; credentials: KVRow[]; config: KVRow[] }) => {
    if (!editing && !(v.credentials ?? []).some((row) => (row.value ?? '') !== '')) {
      message.error('凭据不能为空——新建无凭据的 provider 无法通过任何验证');
      return;
    }
    save.mutate(v);
  };

  // type 变化联动：行仍为"未填值"（值为空且键=约定预置键）时，按 type 重置预置键。
  // 已填值或用户自定义键不碰（编辑态类型锁定，联动只在新建发生）。
  const onTypeChange = (type: string) => {
    const preset = presetKeysForType(type);
    const resetIfPreset = (listName: 'credentials' | 'config', fromKeys: string[], toKey: string) => {
      const rows = (form.getFieldValue(listName) ?? []) as KVRow[];
      if (rows.length === 1 && rows[0] && rows[0].value === '' && fromKeys.includes(rows[0].key)) {
        form.setFieldValue([listName, 0, 'key'], toKey);
      }
    };
    resetIfPreset('credentials', [OPENAI_PRESET.cred, ANTHROPIC_PRESET.cred], preset.cred);
    resetIfPreset('config',
      [OPENAI_PRESET.conf, ANTHROPIC_PRESET.conf, 'BASE_URL'], preset.conf);
  };

  // 快捷模板（新建态）：一键铺类型+端点+键名，用户只贴 key
  const applyTemplate = (key: string) => {
    const tpl = TEMPLATES.find((t) => t.key === key);
    if (!tpl || !tpl.type) return; // 自定义：不预填
    const preset = presetKeysForType(tpl.type);
    form.setFieldsValue({
      type: tpl.type,
      credentials: [{ key: preset.cred, value: '' }],
      config: [{ key: preset.conf, value: tpl.base }],
    });
  };

  const openCreate = () => {
    setEditing(null);
    form.resetFields();
    // type 默认 openai（预置 openai 系键；切类型且行未填值时由 onTypeChange 联动重置）
    form.setFieldsValue({
      name: '', type: 'openai',
      credentials: [{ key: OPENAI_PRESET.cred, value: '' }], config: [{ key: OPENAI_PRESET.conf, value: '' }],
    });
    setFormOpen(true);
  };
  const openEdit = (p: InferenceProvider) => {
    setEditing(p);
    form.resetFields();
    form.setFieldsValue({
      name: p.name,
      type: p.type,
      // 凭据不可见（服务端不回流）：预置空行待填，留空提交=保留已存（键按类型约定预置）
      credentials: [{ key: presetKeysForType(p.type).cred, value: '' }],
      config: Object.entries(p.config).map(([key, value]) => ({ key, value })),
    });
    setFormOpen(true);
  };

  // ---- 删除（在用 provider 先切换路由）----
  const remove = useMutation({
    mutationFn: (name: string) => deleteInferenceProvider(name, ws),
    onSuccess: (r) => {
      message.success(r.deleted ? 'Provider 已删除' : 'Provider 不存在（可能已被删除）');
      qc.invalidateQueries({ queryKey: ['inference-providers'] });
      qc.invalidateQueries({ queryKey: ['workspaces-detail'] }); // 就绪度列联动
    },
    onError: (e) => message.error(`删除失败：${(e as Error).message}`),
  });

  // ---- 切换路由（自带连通性验证回执）----
  const [routeOpen, setRouteOpen] = useState(false);
  const [routeForm] = Form.useForm();
  const setRoute = useMutation({
    mutationFn: (p: { provider: string; model: string; no_verify?: boolean }) => setInferenceRoute(p, ws),
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
      qc.invalidateQueries({ queryKey: ['workspaces-detail'] }); // 就绪度列联动
    },
    onError: (e) => message.error(`切换失败（路由未变更）：${(e as Error).message}`),
  });

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
          : <Typography.Text code style={{ wordBreak: 'break-all' }}>{Object.entries(c).map(([k, v]) => `${k}=${v}`).join('；')}</Typography.Text>,
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
    <Space direction="vertical" style={{ width: '100%' }} size={12}>
      <Card
        title="当前推理路由"
        extra={(
          <Button onClick={() => { routeForm.resetFields(); setRouteOpen(true); }}>切换路由</Button>
        )}
      >
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
        width={760}
      >
        <Form form={form} layout="vertical" onFinish={onFormFinish}>
          {!editing && (
            <Form.Item label="厂商模板" extra="选厂商自动带出类型/端点/键名，只需粘贴 API Key">
              <Select
                options={TEMPLATES.map((t) => ({ value: t.key, label: t.label }))}
                placeholder="选择厂商（可选）"
                onChange={applyTemplate}
                allowClear
              />
            </Form.Item>
          )}
          <Form.Item
            name="name"
            label="名称"
            rules={[{ required: true, message: '请输入名称' }, { pattern: /^[a-zA-Z0-9_-]{1,64}$/, message: '1-64 位字母/数字/下划线/短横线' }]}
          >
            <Input disabled={!!editing} placeholder="如 zhipu-main" />
          </Form.Item>
          <Form.Item
            name="type"
            label="类型"
            rules={[{ required: true, message: '请选择类型' }]}
            extra={editing ? '不可修改（删除重建可换）' : '创建后不可修改'}
          >
            <Select
              options={TYPE_OPTIONS}
              onChange={onTypeChange}
              disabled={!!editing}
              placeholder="选择类型"
            />
          </Form.Item>
          <Form.Item
            label="凭据（credentials）"
            required={!editing}
            extra={editing ? '留空=保留已存，填写=覆盖；删键须删除重建 Provider' : '仅写入加密存储，不回显'}
          >
            <KVEditor name="credentials" keyPlaceholder="键（如 OPENAI_API_KEY）" valueLabel="API Key（留空=保留已存）" password />
          </Form.Item>
          <Form.Item label="配置（config）" extra={KEY_CONVENTION_HINT}>
            <KVEditor name="config" keyPlaceholder="键（如 OPENAI_BASE_URL）" valueLabel="配置值" />
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
            extra="保存时验证连通性，失败则路由不生效（原路由保持）"
          >
            <Input placeholder="如 glm-4.5 / deepseek-chat" />
          </Form.Item>
          <Form.Item name="verify" label="保存时验证连通性" valuePropName="checked" initialValue={true}>
            <Switch />
          </Form.Item>
        </Form>
      </Modal>
    </Space>
  );
}
