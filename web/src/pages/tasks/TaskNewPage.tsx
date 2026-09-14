// 任务创建向导（14号 §3.3 ①；04 §3 五模式分流）——弹窗形态（2026-09-14 视觉重设计二期）
// 全站"创建"类操作统一为 Modal（新建项目/项目编辑/用户/Provider 一致），任务创建原为
// 唯一独立跳页+全宽向导+侧栏——交互逻辑断裂且宽屏留白失衡。现重构为 TaskNewModal
// （TasksPage 按钮直开，与新建项目同构）；默认导出保留 /tasks/new 薄壳宿主页，深链
// ?project_id= 不死链。Step4 创建 → POST /v1/tasks（网关生成幂等键）；config 不再承载
// 任务级源码键——项目层级决定源代码仓库，向导不提供重新上传/
// 指定仓库/手填路径（项目 config.upload_file_id / repo_url 由 task-service 启动时解析，
// ADR-203 兜底链）。2026-09-11 用户报障（建任务引导）：项目列表加载失败 Alert+重试 /
// 空列表引导去项目页 / 项目 Select 可搜索 / ?project_id= 深链预选（项目详情页直达）。
// 2026-09-14 视觉重设计（体系内精致，零新色零 webfont）：
//   ① 模式步"策略卡"——每卡顶部等宽引擎流一行（五模式的本质差异=引擎编排，结构即信息）；
//   ② 机器标识（项目/工具 ID、引擎流）走 MONO_FONT（design-tokens 等宽嗓音规则）；
//   ③ 色/边框全部 theme.useToken() 语义 token。业务逻辑与请求面零变更（测试锚原样保留）。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Checkbox, Form, Input, Modal, Radio, Select, Steps, Tag, Typography, message } from 'antd';
import { theme as antdTheme } from 'antd';
import { autoRunTask } from '../../tasks/stateMachine';
import { useEffect, useState } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';
import { createProject, createTask, getProject, getProjectConfig, getTools, listAllProjects } from '../../api/client';
import { SCAN_MODE, REVIEW_DEPTH, zh } from '../../dict';
import { MONO_FONT } from '../../dict/tokens';
import { usePageTitle } from '../../hooks/usePageTitle';

// ADR-186 五模式矩阵：每模式需要的参数分支（向导分支覆盖的单一来源）
// A=纯SAST多工具并行 / B=纯AI / C=SAST+AI并行融合（默认推荐） / D=AI增强SAST / E=SAST+AI并行对比
export interface ModeSpec {
  needsSastTools: boolean;
  needsReviewConfig: boolean;
  blurb: string;
  deprecated?: boolean; // ADR-182 弃用模式：不进入新建入口，历史任务展示仍可用
}
export const MODE_SPECS: Record<string, ModeSpec> = {
  SCAN_MODE_SAST_ONLY: { needsSastTools: true, needsReviewConfig: false, blurb: '多个 SAST 工具并行审计，结果去重后合并产出' },
  SCAN_MODE_AI_ONLY: { needsSastTools: false, needsReviewConfig: false, blurb: '纯 AI 语义审计（沙箱 DSH；不可用时走降级链并如实标注）' },
  SCAN_MODE_PARALLEL: { needsSastTools: true, needsReviewConfig: false, blurb: '★推荐（默认）：SAST 工具与 AI 并行审计，结果融合去重后输出单一清单' },
  SCAN_MODE_AI_ENHANCED_SAST: { needsSastTools: true, needsReviewConfig: false, blurb: 'SAST 扫描发现先按同文件同段去重，再逐条交 DSH 沙箱验证真伪，SAST+AI 判定汇总后融合出报告' },
  SCAN_MODE_COMPARE: { needsSastTools: true, needsReviewConfig: false, blurb: 'SAST 与 AI 并行各自完成，按 单SAST / 单AI / SAST+AI 三类同维度对比（ADR-186 前称"模式D"）' },
  SCAN_MODE_TRADITIONAL_FIRST: { needsSastTools: true, needsReviewConfig: false, deprecated: true, blurb: '【已弃用】SAST→AI 逐条增强验证（历史兼容）' },
  SCAN_MODE_SAST_REVIEW: { needsSastTools: true, needsReviewConfig: true, deprecated: true, blurb: '【已弃用】SAST 结果交 AI 审核（历史兼容）' },
};
// ADR-182 默认推荐模式C（ADR-186 五模式下维持不变）
export const DEFAULT_SCAN_MODE = 'SCAN_MODE_PARALLEL';

// 引擎流一行（视觉层呈现，不进 MODE_SPECS 契约）——五种入口模式的本质差异即引擎编排方式。
// 等宽呈现：流程是机器编排的嗓音（design-tokens"凡机器产生一律等宽"的同类延展）。
const ENGINE_FLOW: Record<string, string> = {
  SCAN_MODE_SAST_ONLY: 'SAST → 合并清单',
  SCAN_MODE_AI_ONLY: 'AI 沙箱 → AI 清单',
  SCAN_MODE_PARALLEL: 'SAST ∥ AI → 融合去重',
  SCAN_MODE_AI_ENHANCED_SAST: 'SAST → AI 逐条验证 → 汇总',
  SCAN_MODE_COMPARE: 'SAST ∥ AI → 三类对比',
};
// PARALLEL 的 blurb 首部推荐前缀提为 Tag 后，描述行去掉星标段
const RECO_PREFIX = '★推荐（默认）：';

// 创建向导弹窗（与新建项目 Modal 同构：footer 集中按钮，关闭即回收全部状态）。
export function TaskNewModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { token } = antdTheme.useToken();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [searchParams] = useSearchParams();
  const [step, setStep] = useState(0);
  const [projectId, setProjectId] = useState<string>('');
  const [newProjName, setNewProjName] = useState<string>('');
  const [mode, setMode] = useState<string>(DEFAULT_SCAN_MODE); // ADR-182: 默认推荐模式C
  // （文案纠偏）：审批流已废除——创建→启动直达，无"提交→批准"环节；
  // 勾掉自动启动则停在已创建，需在任务页手动点启动
  const [autoStart, setAutoStart] = useState<boolean>(true);
  // 2026-09-09: 任务级源码覆盖（uploadFileId/project_path）随"项目层级决定源码"指令退役，
  // ADR-154 的参数暂存只剩工具与审核配置
  const [params, setParams] = useState<{
    sast_tools?: string[];
    review_depth?: string;
    review_opts?: string[];
  }>({});
  const [form] = Form.useForm();

  // 关闭即回收（对齐 ProjectsPage resetModalState 先例）——重开不残留上次的向导状态
  const resetAndClose = () => {
    setStep(0);
    setProjectId('');
    setNewProjName('');
    setMode(DEFAULT_SCAN_MODE);
    setAutoStart(true);
    setParams({});
    form.resetFields();
    onClose();
  };

  // ADR-203: 响应形状经 client.ts 类型化端点锚定（不再页面内 as-cast）。
  // 2026-09-11 用户报障（建任务引导）：isError 显性化 + 重试（此前失败静默空列表，
  // 用户只看到无法选择的下拉）；深链 ?project_id= 供项目详情页直达预选。
  const { data: projects, isError: projectsError, refetch: refetchProjects } = useQuery({
    queryKey: ['projects'],
    queryFn: async () => ({ projects: await listAllProjects() }), // B5-P2-7: 全量翻页
    enabled: open, // 弹窗关着不打项目列表（TasksPage 首屏零额外请求）
  });
  // 深链预选：仅当 project_id 命中列表项才预选，未命中保持未选（不猜 ID）
  const wantedProjectId = searchParams.get('project_id') ?? '';
  useEffect(() => {
    if (!open || !wantedProjectId || projectId) return;
    if ((projects?.projects ?? []).some((p) => p.project_id === wantedProjectId)) {
      setProjectId(wantedProjectId);
    }
  }, [open, wantedProjectId, projects, projectId]);
  // ADR-163: 仓库模式——项目配置 repo_url 时启动时后端自动 clone（唯一仓库通道）
  const { data: projInfo } = useQuery({
    queryKey: ['project', projectId],
    queryFn: () => getProject(projectId),
    enabled: open && !!projectId,
  });

  // 就地创建项目（第 1 步无项目时的第二条出路，与"前往项目页创建"并存）：
  // 代码来源不在向导指定——项目页/项目详情后续配置（上传件或仓库地址均可），
  // 参数步对无来源项目已有如实警告兜底。
  const createProj = useMutation({
    mutationFn: (name: string) => createProject({
      name,
      default_branch: 'main',
      default_scan_mode: DEFAULT_SCAN_MODE,
    }),
    onSuccess: (p) => {
      message.success(`项目「${p.name}」已创建并选用`);
      setProjectId(p.project_id);
      setNewProjName('');
      qc.invalidateQueries({ queryKey: ['projects'] });
    },
    onError: (e) => message.error(`项目创建失败：${(e as Error).message}`),
  });

  const repoURL = projInfo?.repo_url ?? '';
  // 项目级源码来源展示（源码由项目层级决定，向导只读呈现）
  const { data: projConfig } = useQuery({
    queryKey: ['project-config', projectId],
    queryFn: () => getProjectConfig(projectId),
    enabled: open && !!projectId,
  });
  const sourceText = repoURL
    ? `仓库自动拉取（${repoURL}）`
    : projConfig?.config?.upload_file_name
      ? `项目压缩包：${projConfig.config.upload_file_name}`
      : projConfig?.config?.upload_file_id
        ? `项目压缩包（${projConfig.config.upload_file_id}）`
        : '';
  const { data: tools, isLoading: toolsLoading } = useQuery({
    queryKey: ['tools'],
    queryFn: getTools,
    enabled: open && MODE_SPECS[mode]?.needsSastTools === true, // 14号 §3.3 ①：仅需工具的模式才探测
  });

  const create = useMutation({
    mutationFn: async (values: { sast_tools?: string[]; review_depth?: string; review_opts?: string[] }) => {
      const config: Record<string, string> = {};
      // 不再写任务级源码键（upload_file_id/project_path）——
      // 源码来源由项目解析；config 只承载审核类键
      if (values.review_depth) config.review_depth = values.review_depth;
      if (values.review_opts?.length) {
        config.assess_severity = String(values.review_opts.includes('assess_severity'));
        config.verify_location = String(values.review_opts.includes('verify_location'));
        config.generate_suggestions = String(values.review_opts.includes('generate_suggestions'));
      }
      return createTask({
        project_id: projectId,
        scan_mode: mode,
        sast_tools: MODE_SPECS[mode]?.needsSastTools ? values.sast_tools ?? [] : [],
        config,
      });
    },
    onSuccess: (resp) => {
      const tid = resp.task_id;
      if (autoStart) {
        message.success('任务已创建，正在自动启动…'); // 审批流废除（2026-09-01）：创建→启动直达
        autoRunTask(tid).then(() => {
          message.success('扫描任务已自动启动');
        }).catch((e) => {
          message.warning(`自动启动失败（${(e as Error).message}），可在任务页手动续走`);
        });
      } else {
        message.success('任务已创建');
      }
      resetAndClose();
      navigate(`/tasks/${tid}`);
    },
    // ADR-154: 此前创建失败静默（无 onError），用户停在确认页无任何反馈
    onError: (e) => message.error(`任务创建失败：${(e as Error).message}`),
  });

  const spec = MODE_SPECS[mode];
  const executableTools = (tools?.tools ?? []).filter((t) => t.valid);
  const unusableTools = (tools?.tools ?? []).filter((t) => !t.valid);

  // 模式策略卡（签名元素①）：标题行保留 dict label 文本（回归锚），引擎流一行等宽，
  // 描述行承接 blurb；默认推荐模式挂 Tag 替代 blurb 内文本星标。
  const modeCards = Object.entries(SCAN_MODE)
    // ADR-182: 弃用模式不进新建入口；R: UNSPECIFIED 展示键不进新建入口
    // （MODE_SPECS 缺项 → needsSastTools undefined → sast_tools:[] → P-26 同型必 FAILED 复活）
    .filter(([value]) => !MODE_SPECS[value]?.deprecated && value !== 'SCAN_MODE_UNSPECIFIED')
    .map(([value, label]) => {
      const selected = value === mode;
      const blurb = MODE_SPECS[value]?.blurb ?? '';
      const desc = blurb.startsWith(RECO_PREFIX) ? blurb.slice(RECO_PREFIX.length) : blurb;
      // dict label 的"（推荐）"后缀由下方 Tag 承担，标题去重
      const title = value === DEFAULT_SCAN_MODE ? label.replace('（推荐）', '') : label;
      return (
        <Radio
          key={value}
          value={value}
          style={{
            display: 'flex', alignItems: 'flex-start', margin: 0, padding: '12px 16px', width: '100%',
            border: `1px solid ${selected ? token.colorPrimary : token.colorBorderSecondary}`,
            borderRadius: token.borderRadiusLG,
            background: selected ? token.colorPrimaryBg : token.colorBgContainer,
            transition: 'border-color 0.2s, background 0.2s',
          }}
        >
          <div style={{ display: 'flex', flexDirection: 'column', gap: 4, minWidth: 0 }}>
            <span style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
              <b>{title}</b>
              {value === DEFAULT_SCAN_MODE && <Tag color="processing" style={{ marginInlineEnd: 0 }}>推荐 · 默认</Tag>}
            </span>
            <span style={{ fontFamily: MONO_FONT, fontSize: 12.5, color: token.colorPrimary, letterSpacing: 0.2 }}>
              {ENGINE_FLOW[value] ?? '—'}
            </span>
            <Typography.Text type="secondary" style={{ fontSize: 13 }}>{desc}</Typography.Text>
          </div>
        </Radio>
      );
    });

  const stepContent: Record<number, React.ReactNode> = {
    0: (
      <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
        {/* 建任务引导（2026-09-11 用户报障）：项目列表加载失败显性化 + 可重试（此前失败
            静默，下拉永远为空无从归因） */}
        {projectsError && (
          <Alert
            type="error"
            showIcon
            message="项目列表加载失败"
            description="项目服务暂不可用，无法选择项目。"
            action={<Button size="small" onClick={() => refetchProjects()}>重试</Button>}
          />
        )}
        <div>
          <Typography.Text type="secondary" style={{ display: 'block', marginBottom: 8 }}>选择要审计的已有项目</Typography.Text>
          <Select
            style={{ width: '100%' }}
            placeholder="选择项目"
            showSearch
            optionFilterProp="label"
            value={projectId || undefined}
            onChange={(v) => setProjectId(v)}
            notFoundContent={projectsError
              ? <Typography.Text type="secondary">加载失败，请点上方重试</Typography.Text>
              : (
                // 空列表引导（2026-09-11 用户报障）：此前空态无任何出路提示
                <Typography.Text type="secondary">
                  暂无项目——下方输入名称就地创建，或<Link to="/projects">前往项目页创建</Link>
                </Typography.Text>
              )}
            options={(projects?.projects ?? []).map((p) => ({ value: p.project_id, label: `${p.name} (${p.project_id})` }))}
          />
        </div>
        {/* 就地创建：输入名称即建项目并自动选用（第 1 步闭环，无项目时不必离开向导） */}
        <div>
          <Typography.Text type="secondary" style={{ display: 'block', marginBottom: 8 }}>还没有项目？就地创建一个</Typography.Text>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
            <Input
              style={{ flex: 1, minWidth: 240 }}
              placeholder="或输入新项目名称，就地创建"
              value={newProjName}
              onChange={(e) => setNewProjName(e.target.value)}
              onPressEnter={() => { const n = newProjName.trim(); if (n) createProj.mutate(n); }}
            />
            <Button
              loading={createProj.isPending}
              disabled={!newProjName.trim()}
              onClick={() => createProj.mutate(newProjName.trim())}
            >
              创建并选用
            </Button>
          </div>
          <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 8 }}>
            项目创建后，代码来源（仓库地址或上传压缩包）在项目页配置——向导按项目层级读取。
          </Typography.Text>
        </div>
      </div>
    ),
    1: (
      <Radio.Group value={mode} onChange={(e) => setMode(e.target.value)} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        {modeCards}
      </Radio.Group>
    ),
    2: (
      <Form form={form} layout="vertical" onFinish={(v) => { setParams(v); setStep(3); }}>
        {spec?.needsSastTools && (
          <>
            <Form.Item
              name="sast_tools"
              label="SAST 工具（仅列可执行工具）"
              rules={[{ required: true, message: '至少选择一个工具' }]}
            >
              <Select
                mode="multiple"
                style={{ width: '100%' }}
                loading={toolsLoading}
                options={executableTools.map((t) => ({ value: t.tool_id, label: `${t.tool_id}（${t.output_format}）` }))}
              />
            </Form.Item>
            {unusableTools.length > 0 && (
              <Alert
                type="warning"
                showIcon
                style={{ marginBottom: 16 }}
                message={`以下工具解析器就绪但执行未接入，不可选：${unusableTools.map((t) => t.tool_id).join('、')}`}
              />
            )}
          </>
        )}
        {/* 项目层级决定源代码仓库——任务向导不提供重新上传/指定
            仓库/手填路径；源码来源=项目 config/repo_url，只读呈现 */}
        {sourceText ? (
          <Alert
            type="info"
            showIcon
            style={{ marginBottom: 16 }}
            message={`源码来源（项目级）：${sourceText}`}
          />
        ) : (
          <Alert
            type="warning"
            showIcon
            style={{ marginBottom: 16 }}
            message="该项目未配置源码来源（无仓库地址也无上传压缩包）——任务启动将失败，请先在项目页补齐"
          />
        )}
        {spec?.needsReviewConfig && (
          <>
            <Form.Item name="review_depth" label="审核深度（ReviewConfig.depth）" initialValue="REVIEW_DEPTH_STANDARD">
              {/* R: 零值不进新建入口 */}
              <Select options={Object.entries(REVIEW_DEPTH).filter(([value]) => value !== 'REVIEW_DEPTH_UNSPECIFIED').map(([value, label]) => ({ value, label }))} />
            </Form.Item>
            <Form.Item name="review_opts" label="审核选项">
              <Checkbox.Group
                options={[
                  { value: 'assess_severity', label: '评估严重级别' },
                  { value: 'verify_location', label: '校验定位' },
                  { value: 'generate_suggestions', label: '生成修复建议' },
                ]}
              />
            </Form.Item>
          </>
        )}
      </Form>
    ),
    3: (
      // 确认步=任务简报的定稿核对（ADR-154：回显第2步参数，参数静默丢失的历史缺陷不再）
      <div style={{ display: 'flex', flexDirection: 'column', gap: 20 }}>
        <div style={{ borderBottom: `1px solid ${token.colorBorderSecondary}`, paddingBottom: 12 }}>
          <Typography.Title level={5} style={{ margin: 0 }}>
            {projInfo?.name ?? projectId} <span style={{ fontFamily: MONO_FONT, fontSize: 13, fontWeight: 400, color: token.colorTextSecondary }}>{projectId}</span>
          </Typography.Title>
          <Typography.Text type="secondary">{zh(SCAN_MODE, mode)}</Typography.Text>
        </div>
        <div>
          {spec?.needsSastTools && (
            <div style={{ display: 'flex', gap: 12, padding: '6px 0' }}>
              <Typography.Text type="secondary" style={{ flex: '0 0 210px' }}>SAST 工具</Typography.Text>
              <span style={{ fontFamily: MONO_FONT, fontSize: 13 }}>{(params.sast_tools ?? []).join('、') || '—'}</span>
            </div>
          )}
          <div style={{ display: 'flex', gap: 12, padding: '6px 0' }}>
            <Typography.Text type="secondary" style={{ flex: '0 0 210px' }}>引擎编排</Typography.Text>
            <span style={{ fontFamily: MONO_FONT, fontSize: 13 }}>{ENGINE_FLOW[mode] ?? '—'}</span>
          </div>
          <div style={{ display: 'flex', gap: 12, padding: '6px 0' }}>
            <Typography.Text type="secondary" style={{ flex: '0 0 210px' }}>源码来源（项目级，不可在此更改）</Typography.Text>
            {sourceText
              ? <span>{sourceText}</span>
              : <Typography.Text type="warning">未配置——启动将失败，请先在项目页补齐</Typography.Text>}
          </div>
          {spec?.needsReviewConfig && (
            <div style={{ display: 'flex', gap: 12, padding: '6px 0' }}>
              <Typography.Text type="secondary" style={{ flex: '0 0 210px' }}>审核深度</Typography.Text>
              <span>{params.review_depth ? zh(REVIEW_DEPTH, params.review_depth) : '—'}</span>
            </div>
          )}
        </div>
        <div>
          <Checkbox
            checked={autoStart}
            onChange={(e: { target: { checked: boolean } }) => setAutoStart(e.target.checked)}
          >
            创建后立即启动（不勾则停在待启动，需在详情页手动点启动）
          </Checkbox>
        </div>
      </div>
    ),
  };

  // footer 集中按钮（对齐 ProjectsPage 的 onOk=form.submit() 模式——参数步提交由 footer 触发）
  const footer = (
    <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
      {step > 0 && <Button onClick={() => setStep(step - 1)}>上一步</Button>}
      <Button onClick={resetAndClose}>取消</Button>
      {step === 0 && <Button type="primary" disabled={!projectId} onClick={() => setStep(1)}>下一步</Button>}
      {step === 1 && <Button type="primary" disabled={!mode} onClick={() => setStep(2)}>下一步</Button>}
      {step === 2 && <Button type="primary" onClick={() => form.submit()}>下一步：确认</Button>}
      {step === 3 && (
        <Button type="primary" loading={create.isPending} onClick={() => create.mutate(params)}>
          创建任务
        </Button>
      )}
    </div>
  );

  return (
    <Modal
      title="新建扫描任务"
      open={open}
      onCancel={resetAndClose}
      footer={footer}
      width={760}
    >
      <Steps
        items={[{ title: '项目' }, { title: '模式' }, { title: '参数' }, { title: '确认' }]}
        current={step}
        style={{ marginBottom: 24 }}
      />
      {stepContent[step]}
    </Modal>
  );
}

// 薄壳宿主页：/tasks/new 直达与 ?project_id= 深链的落点——弹窗即页面内容，
// 关闭回任务列表（TasksPage 内也直挂同组件，点按钮即开，与新建项目同构）。
export default function TaskNewPage() {
  usePageTitle('新建任务');
  const navigate = useNavigate();
  return <TaskNewModal open onClose={() => navigate('/tasks')} />;
}
