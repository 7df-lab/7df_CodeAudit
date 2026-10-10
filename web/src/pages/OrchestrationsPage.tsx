// 编排管理页（graph-reseed 后续：打通用户缺少编排入口"→ 编排 CRUD 批
// 2026-09-21 落地增删改查）。数据源=GET /v1/orchestrations（负载 pack 目录，服务端
// configs/packs 内置只读 + CRUD 产物 append 修订）；写面（建/更/停/恢复）=admin。
// 修订语义=append-only：更新=append 新修订（历史任务钉住的修订不受影响）；
// 停用=标记（可恢复）。内置编排只读（产品基线）。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Form, Input, InputNumber, Modal, Popconfirm, Select, Space, Tag, Typography, message } from 'antd';
import { useState } from 'react';
import { Link } from 'react-router-dom';
import { api, apiErrorMessage, createOrchestration, deprecateOrchestration, listOrchestrations, updateOrchestration } from '../api/client';
import type { OrchestrationInfo } from '../api/types';
import { MONO_FONT } from '../dict/tokens';
import PageHeader from '../components/PageHeader';
import { usePageTitle } from '../hooks/usePageTitle';
import { useSession } from '../auth/session';

// parseSpecFields — 从 spec_yaml 提取编辑表单所需字段（简单行解析；
// YAML 由服务端 yaml.Marshal 生成，键位固定）。P1-1/P1-2 修复：编辑预填不再硬编码。
function parseSpecFields(yaml: string): {
  networkProfile: string;
  maxNodes: number;
  maxPlannerReplans: number;
  maxAttemptsPerNode: number;
  seedIntents: string[];
} {
  const result = {
    networkProfile: 'restricted',
    maxNodes: 12,
    maxPlannerReplans: 3,
    maxAttemptsPerNode: 2,
    seedIntents: [] as string[],
  };
  const lines = yaml.split('\n');
  let inSeedIntents = false;
  let inConstraints = false;
  for (const line of lines) {
    const t = line.trim();
    if (t === 'seed_intents:') { inSeedIntents = true; inConstraints = false; continue; }
    if (t === 'constraints:') { inConstraints = true; inSeedIntents = false; continue; }
    if (t.startsWith('- ') && inSeedIntents) {
      result.seedIntents.push(t.slice(2).replace(/^["']|["']$/g, ''));
      continue;
    }
    if (inConstraints) {
      const mn = t.match(/^max_nodes:\s*(\d+)/);
      if (mn) { result.maxNodes = parseInt(mn[1]); inConstraints = false; continue; }
      const mr = t.match(/^max_planner_replans:\s*(\d+)/);
      if (mr) { result.maxPlannerReplans = parseInt(mr[1]); continue; }
      const ma = t.match(/^max_attempts_per_node:\s*(\d+)/);
      if (ma) { result.maxAttemptsPerNode = parseInt(ma[1]); continue; }
    }
    const np = t.match(/^profile:\s*(.+)/);
    if (np) result.networkProfile = np[1].trim().replace(/["']/g, '');
  }
  return result;
}

// 结构化表单 → pack YAML（全量；服务端 Validate 校验，错误带字段路径回显）。
// 值统一 JSON 双引号转义（YAML 兼容）。
function buildPackYAML(v: {
  name: string;
  description: string;
  networkProfile: string;
  maxNodes: number;
  maxPlannerReplans: number;
  maxAttemptsPerNode: number;
  seedIntents: string[];
}): string {
  const q = (s: string) => JSON.stringify(s);
  const intents = (v.seedIntents.length ? v.seedIntents : ['待补充审计面'])
    .map((s) => `      - ${q(s)}`)
    .join('\n');
  return [
    'apiVersion: codeaudit.pack/v0',
    'kind: WorkloadPack',
    `name: ${q(v.name)}`,
    `description: ${q(v.description)}`,
    'spec:',
    '  image: dsh-pentest-sse:latest',
    '  dialect: dsh-sse',
    '  network:',
    `    profile: ${q(v.networkProfile)}`,
    '  execution: graph',
    '  seeds:',
    '    brief_renderer: baton-brief',
    '    render_target: task-card-prefix',
    '    template_ref: assignmentTemplate@frozen',
    '  results:',
    '    schema_ref: findings/submit_findings@v1',
    '    parser: codeaudit-submit',
    '  timeouts:',
    '    per_turn: default',
    '    task_total: audit-default',
    '  graph:',
    '    seed_intents:',
    intents,
    '    constraints:',
    `      max_nodes: ${v.maxNodes || 12}`,
    '      allowed_types: [planner, executor, verifier]',
    `      max_planner_replans: ${v.maxPlannerReplans || 3}`,
    `      max_attempts_per_node: ${v.maxAttemptsPerNode || 2}`,
    '    timeouts:',
    '      planner: 180',
    '      executor: 900',
    '      verifier: 300',
  ].join('\n') + '\n';
}

interface PackFormValues {
  name: string;
  description: string;
  networkProfile: string;
  maxNodes: number;
  maxPlannerReplans: number;
  maxAttemptsPerNode: number;
  seedIntents?: string;
}

export default function OrchestrationsPage() {
  usePageTitle('编排');
  const qc = useQueryClient();
  const [form] = Form.useForm();
  const { user } = useSession();
  const isAdmin = user?.role === 'ROLE_ADMIN';
  const [showDeprecated, setShowDeprecated] = useState(false);
  const [modalOpen, setModalOpen] = useState(false);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<OrchestrationInfo | null>(null);

  const listQ = useQuery({
    queryKey: ['orchestrations', showDeprecated],
    queryFn: async () =>
      (
        await api.get('/v1/orchestrations', {
          params: showDeprecated ? { include_deprecated: true } : {},
        })
      ).data as { orchestrations: OrchestrationInfo[] },
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: ['orchestrations'] });

  const deprecate = useMutation({
    mutationFn: async (v: { name: string; unde: boolean }) => deprecateOrchestration(v.name, v.unde),
    onSuccess: (_d, v) => {
      message.success(v.unde ? '编排已恢复' : '编排已停用');
      invalidate();
    },
    onError: (e) => message.error(`操作失败：${apiErrorMessage(e)}`),
  });

  const upsert = useMutation({
    mutationFn: async (v: { name: string; specYAML: string; update: boolean }) =>
      v.update ? updateOrchestration(v.name, v.specYAML) : createOrchestration(v.name, v.specYAML),
    onSuccess: (_d, v) => {
      message.success(v.update ? '编排已更新（append 新修订）' : '编排已创建');
      setModalOpen(false);
      setEditing(null);
      invalidate();
    },
    onError: (e) => message.error(`保存失败：${apiErrorMessage(e)}`),
  });

  const openCreate = () => {
    setEditing(null);
    setCreating(true);
    form.resetFields();
    form.setFieldsValue({ networkProfile: 'restricted', maxNodes: 12, maxPlannerReplans: 3, maxAttemptsPerNode: 2 });
    setModalOpen(true);
  };

  const openEdit = (o: OrchestrationInfo) => {
    setEditing(o);
    setCreating(false);
    form.resetFields();
    // 从 spec_yaml 反解结构化字段（YAML 由服务端 yaml.Marshal 生成，格式可预测）
    const parsed = parseSpecFields(o.spec_yaml);
    form.setFieldsValue({
      name: o.name,
      description: o.description,
      networkProfile: parsed.networkProfile,
      maxNodes: parsed.maxNodes,
      maxPlannerReplans: parsed.maxPlannerReplans,
      maxAttemptsPerNode: parsed.maxAttemptsPerNode,
      seedIntents: parsed.seedIntents.join('\n'),
    });
    setModalOpen(true);
  };

  const submitForm = (v: PackFormValues) => {
    upsert.mutate({
      name: (v.name ?? '').trim(),
      specYAML: buildPackYAML({
        name: (v.name ?? '').trim(),
        description: (v.description ?? '').trim(),
        networkProfile: v.networkProfile,
        maxNodes: Number(v.maxNodes) || 12,
        maxPlannerReplans: Number(v.maxPlannerReplans) || 3,
        maxAttemptsPerNode: Number(v.maxAttemptsPerNode) || 2,
        seedIntents: (v.seedIntents ?? '')
          .split('\n')
          .map((s) => s.trim())
          .filter(Boolean),
      }),
      update: !!editing,
    });
  };

  return (
    <div>
      <PageHeader
        title="编排"
        extra={
          isAdmin ? (
            <Button type="primary" onClick={openCreate}>
              新建编排
            </Button>
          ) : undefined
        }
      />
      <Typography.Paragraph type="secondary" style={{ maxWidth: 720 }}>
        编排 = 执行一条审计任务的负载与流程定义。选定的编排决定执行流的形态（图编排按
        「审计面规划 → 逐面深度审计 → 复核去重」推进，重派节点自动携带前任失败要点）；
        在新建任务时可随时切换回标准编排。
      </Typography.Paragraph>

      <Space style={{ marginBottom: 12 }}>
        <Button size="small" onClick={() => setShowDeprecated((v) => !v)}>
          {showDeprecated ? '隐藏已停用' : '显示已停用'}
        </Button>
      </Space>

      {listQ.isLoading ? (
        <Typography.Text type="secondary">加载中…</Typography.Text>
      ) : listQ.isError ? (
        <div>
          <Typography.Text type="danger">编排列表加载失败。</Typography.Text>
          <Button size="small" style={{ marginLeft: 8 }} onClick={() => listQ.refetch()}>重试</Button>
        </div>
      ) : (listQ.data?.orchestrations ?? []).length === 0 ? (
        <Typography.Text type="secondary">
          暂无编排。
          {isAdmin && ' 点右上角「新建编排」创建。'}
        </Typography.Text>
      ) : (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(360px, 1fr))', gap: 16, alignItems: 'stretch' }}>
          {(listQ.data?.orchestrations ?? []).map((o) => (
            <Card
              key={o.name}
              style={{ display: 'flex', flexDirection: 'column' }}
              styles={{ body: { flex: 1, display: 'flex', flexDirection: 'column', gap: 10 } }}
              title={
                <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                  <span style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>{o.name}</span>
                  {o.readonly && <Tag style={{ marginInlineEnd: 0 }}>内置</Tag>}
                  {o.deprecated && <Tag color="default">已停用</Tag>}
                </span>
              }
              extra={
                <Tag color={o.execution === 'graph' ? 'purple' : 'default'}>
                  {o.execution === 'graph' ? '图编排' : '单会话'}
                </Tag>
              }
            >
              <span style={{ fontFamily: MONO_FONT, fontSize: 12.5, color: o.execution === 'graph' ? '#6b4fd8' : '#1f6feb' }}>
                {o.execution === 'graph' ? '审计面规划 → 逐面深度审计 → 复核去重' : '单会话 → 全量审计 → 单一清单'}
              </span>
              {o.description && (
                <Typography.Paragraph style={{ marginBottom: 0, fontSize: 13 }}>{o.description}</Typography.Paragraph>
              )}
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                修订 v{o.latest_revision} · {o.deprecated ? '已停用' : '可选'}
              </Typography.Text>
              <div style={{ marginTop: 'auto', display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
                {!o.deprecated && <Link to={`/tasks/new?orchestration=${encodeURIComponent(o.name)}`}>用此编排建任务</Link>}
                {isAdmin && !o.deprecated && !o.readonly && (
                  <>
                    <Button size="small" onClick={() => openEdit(o)}>
                      编辑（append 修订）
                    </Button>
                    <Popconfirm title={`停用编排「${o.name}」？已建任务不受影响`} onConfirm={() => deprecate.mutate({ name: o.name, unde: false })}>
                      <Button size="small" danger>
                        停用
                      </Button>
                    </Popconfirm>
                  </>
                )}
                {isAdmin && o.deprecated && (
                  <Button size="small" onClick={() => deprecate.mutate({ name: o.name, unde: true })}>
                    恢复
                  </Button>
                )}
              </div>
            </Card>
          ))}
        </div>
      )}

      <Modal
        title={creating ? '新建编排' : `编辑编排（append 新修订）：${editing?.name ?? ''}`}
        open={modalOpen}
        onCancel={() => {
          setModalOpen(false);
          setEditing(null);
        }}
        footer={null}
        width={640}
        forceRender
      >
        <Form form={form} layout="vertical" onFinish={submitForm}>
          <Form.Item name="name" label="名称（DNS-1123）" rules={[{ required: true, pattern: /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/, message: '小写字母/数字/连字符' }, { max: 63, message: '最长 63 字符（对齐 engine nameRe）' }]}>
            <Input disabled={!!editing} placeholder="如 audit-graph" />
          </Form.Item>
          <Form.Item name="description" label="描述" rules={[{ required: true, message: '请填写描述' }]}>
            <Input placeholder="一句话说明该编排的用途" />
          </Form.Item>
          <Form.Item name="networkProfile" label="网络档" initialValue="restricted">
            <Select
              options={[
                { value: 'restricted', label: 'restricted（默认全拒，仅推理出口）' },
                { value: 'targeted', label: 'targeted（基础+授权目标）' },
              ]}
            />
          </Form.Item>
          <Form.Item name="seedIntents" label="审计面拆解（每行一面）" rules={[{ required: true, message: '至少填写一个审计面' }]}>
            <Input.TextArea rows={4} placeholder={'每行一个审计面，如：\n危险模式全库审计：SQL 注入/命令注入/硬编码凭据\n认证与会话链路审计'} />
          </Form.Item>
          <Space wrap size={12}>
            <Form.Item name="maxNodes" label="节点上限" initialValue={12}>
              <InputNumber min={1} style={{ width: 110 }} />
            </Form.Item>
            <Form.Item name="maxPlannerReplans" label="重规划上限" initialValue={3}>
              <InputNumber min={0} style={{ width: 110 }} />
            </Form.Item>
            <Form.Item name="maxAttemptsPerNode" label="每节点尝试上限" initialValue={2}>
              <InputNumber min={1} style={{ width: 110 }} />
            </Form.Item>
          </Space>
          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
            <Button onClick={() => { setModalOpen(false); setEditing(null); }}>取消</Button>
            <Button type="primary" loading={upsert.isPending} onClick={() => form.submit()}>
              {creating ? '创建编排' : '保存（append 新修订）'}
            </Button>
          </div>
        </Form>
      </Modal>
    </div>
  );
}
