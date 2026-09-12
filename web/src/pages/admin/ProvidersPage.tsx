// 推理 Provider 管理页（ADR-217）：AI 推理 provider 增删改查 + 工作区推理路由查看/切换。
// 链路：/v1/inference/*（admin 面）→ engine → openshell-manager → OpenShell 网关
// （provider 权威存储 gateway.db，凭据加密）。凭据只进不出：GET 永不回流 credentials，
// PUT 空 credentials={} 会清空已存凭据——编辑留空提交必须经 Modal 显式确认（防误清）。
// 键名约定：网关按大写约定键解析端点（openai 系 OPENAI_BASE_URL/OPENAI_API_KEY；
// anthropic BASE_URL/API_KEY），小写键静默存储但不被识别（2026-09-11 用户报障）。
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

// 键名约定（2026-09-11 用户报障取证）：OpenShell 网关按 provider config/credentials 的
// 大写约定键解析端点——openai/openai-compatible/deepseek/zhipu 型 → OPENAI_BASE_URL/OPENAI_API_KEY；
// anthropic 型 → BASE_URL/API_KEY。小写键（base_url/api_key）会被静默存储但不被识别，
// 切路由验证时失败。此前预置小写键即根因。
const KEY_CONVENTION_HINT =
  '网关按约定大写键解析端点（openai 型：OPENAI_BASE_URL/OPENAI_API_KEY；anthropic 型：BASE_URL/API_KEY）；其他自定义键将被忽略';
const OPENAI_PRESET = { cred: 'OPENAI_API_KEY', conf: 'OPENAI_BASE_URL' };
const ANTHROPIC_PRESET = { cred: 'API_KEY', conf: 'BASE_URL' };
// anthropic 型用独立约定键，其余已支持型共用 openai 系约定键（未选/未知型回退 openai 系缺省）
function presetKeysForType(type: string): { cred: string; conf: string } {
  return type === 'anthropic' ? ANTHROPIC_PRESET : OPENAI_PRESET;
}

function rowsToMap(rows: KVRow[] | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const r of rows ?? []) {
    const k = r.key?.trim();
    const v = r.value ?? '';
    // 只提交填完整的行：值留空的行整体丢弃——编辑凭据全留空 → {} = 清空已存凭据
    //（防误清：该语义仅允许经 Modal 显式确认后到达，见 onFinish）
    if (k && v !== '') out[k] = v;
  }
  return out;
}

// 凭据行是否有任一非空值（防误清判定：全空 = 意图清除）
function rowsHaveValue(rows: KVRow[] | undefined): boolean {
  return (rows ?? []).some((r) => (r.value ?? '') !== '');
}

function KVEditor({ name, keyPlaceholder, valueLabel, password }:
{ name: string; keyPlaceholder: string; valueLabel: string; password?: boolean }) {
  return (
    <Form.List name={name}>
      {(fields, { add, remove }) => (
        <>
          {fields.map((f) => (
            <Space key={f.key} style={{ display: 'flex', marginBottom: 4 }} align="baseline">
              <Form.Item name={[f.name, 'key']} rules={[{ required: true, message: '键必填' }]} style={{ marginBottom: 0 }}>
                <Input placeholder={keyPlaceholder} style={{ width: 180 }} />
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
  // 防误清确认（受控 Modal，随页面 React 树渲染）
  const [clearConfirmOpen, setClearConfirmOpen] = useState(false);
  const [pendingValues, setPendingValues] = useState<{
    name: string; type: string; credentials: KVRow[]; config: KVRow[];
  } | null>(null);
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
    // 凭据语义动态文案（2026-09-11 用户报障）：服务端永不回显 credentials（GET 响应无该键），
    // 用户必须被告知凭据发生了什么——创建=写入；编辑非空=覆盖生效；编辑全空（经确认）=清除
    onSuccess: (r, v) => {
      const toast = r.created
        ? `Provider「${r.name}」已创建；凭据已写入（服务端不再回显）`
        : rowsHaveValue(v.credentials)
          ? `Provider「${r.name}」已更新；新凭据已生效`
          : `Provider「${r.name}」已更新；已存凭据已清除`;
      message.success(toast);
      setFormOpen(false);
      form.resetFields();
      qc.invalidateQueries({ queryKey: ['inference-providers'] });
    },
    onError: (e) => message.error(`保存失败：${(e as Error).message}`),
  });

  // 提交闸门（防误清，2026-09-11 用户报障）：PUT 空 credentials={} 会清空已存凭据，
  // 而 rowsToMap 丢空值行 → 编辑不填凭据提交 = 静默清空。现：
  //   新建：凭据至少一行非空才允许提交（无凭据 provider 无法通过任何验证）；
  //   编辑：凭据行全空 → 受控确认 Modal 显式确认后才按原逻辑提交（credentials:{}），取消返回表单。
  const onFormFinish = (v: { name: string; type: string; credentials: KVRow[]; config: KVRow[] }) => {
    if (!editing && !rowsHaveValue(v.credentials)) {
      message.error('凭据不能为空——新建无凭据的 provider 无法通过任何验证');
      return;
    }
    if (editing && !rowsHaveValue(v.credentials)) {
      setPendingValues(v);
      setClearConfirmOpen(true);
      return;
    }
    save.mutate(v);
  };

  // type 变化联动（2026-09-11 用户报障）：行仍为"未填值"（值为空且键=约定预置键）时，
  // 按 type 重置预置键——anthropic → API_KEY/BASE_URL，其余 → OPENAI_API_KEY/OPENAI_BASE_URL。
  // 已填值或用户自定义键不碰。
  const onTypeChange = (type: string) => {
    const preset = presetKeysForType(type);
    const resetIfPreset = (listName: 'credentials' | 'config', fromKeys: string[], toKey: string) => {
      const rows = (form.getFieldValue(listName) ?? []) as KVRow[];
      if (rows.length === 1 && rows[0] && rows[0].value === '' && fromKeys.includes(rows[0].key)) {
        form.setFieldValue([listName, 0, 'key'], toKey);
      }
    };
    resetIfPreset('credentials', [OPENAI_PRESET.cred, ANTHROPIC_PRESET.cred], preset.cred);
    resetIfPreset('config', [OPENAI_PRESET.conf, ANTHROPIC_PRESET.conf], preset.conf);
  };

  const openCreate = () => {
    setEditing(null);
    form.resetFields();
    // type 未选：预置 openai 系缺省键（选 anthropic 且行未填值时由 onTypeChange 联动重置）
    form.setFieldsValue({
      name: '', type: '',
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
      // 凭据不可见（服务端不回流）：预置空行待填（键按该 provider 类型的约定键预置）
      credentials: [{ key: presetKeysForType(p.type).cred, value: '' }],
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
        <Form form={form} layout="vertical" onFinish={onFormFinish}>
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
              options={[
                { value: 'openai' },
                { value: 'openai-compatible' },
                { value: 'deepseek' },
                { value: 'zhipu' },
                // anthropic 型端点键与 openai 系不同（BASE_URL/API_KEY）——下拉即警示，防错键；
                // BASE_URL 须 https（ADR-228：明文 http 会被网关验证层回落官方端点，区域 403）
                { value: 'anthropic', label: 'anthropic（注意：端点键为 BASE_URL / API_KEY，与 openai 系不同；BASE_URL 须 https）' },
              ]}
              onChange={onTypeChange}
              placeholder="如 anthropic"
            />
          </Form.Item>
          <Form.Item
            label="凭据（credentials）"
            required
            extra={editing
              ? `服务端不回显已存凭据；留空提交将清除已存凭据。${KEY_CONVENTION_HINT}`
              : `仅写入网关加密存储，保存后任何页面不再显示。${KEY_CONVENTION_HINT}`}
          >
            <KVEditor name="credentials" keyPlaceholder="键（如 OPENAI_API_KEY）" valueLabel="凭据值（如 sk-…）" password />
          </Form.Item>
          <Form.Item label="配置（config）" extra={KEY_CONVENTION_HINT}>
            <KVEditor name="config" keyPlaceholder="键（如 OPENAI_BASE_URL）" valueLabel="配置值" />
          </Form.Item>
        </Form>
      </Modal>

      {/* 防误清确认（2026-09-11 用户报障）：编辑凭据全空提交时显式确认清除语义。
          受控 Modal（页内渲染）而非 Modal.confirm 命令式门户——取消/重开状态确定。 */}
      <Modal
        title="确认清除已存凭据？"
        open={clearConfirmOpen}
        onCancel={() => { setClearConfirmOpen(false); setPendingValues(null); }}
        onOk={() => {
          if (pendingValues) save.mutate(pendingValues);
          setClearConfirmOpen(false);
        }}
        okText="确认清除"
        okButtonProps={{ danger: true }}
        cancelText="返回表单"
        confirmLoading={save.isPending}
      >
        凭据行留空将清除已存凭据（网关不回显凭据，无法恢复）。确认清除？
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
