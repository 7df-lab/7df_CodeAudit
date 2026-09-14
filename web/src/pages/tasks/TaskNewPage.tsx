// 任务创建向导（14号 §3.3 ①；04 §3 五模式分流）
// Step4 创建 → POST /v1/tasks（网关生成幂等键）；config 不再承载任务级源码键——
// 项目层级决定源代码仓库，向导不提供重新上传/指定仓库/手填路径
// （项目 config.upload_file_id / repo_url 由 task-service 启动时解析，ADR-203 兜底链）
// 2026-09-11 用户报障（建任务引导）：项目列表加载失败 Alert+重试 / 空列表引导去项目页 /
// 项目 Select 可搜索 / ?project_id= 深链预选（项目详情页直达）
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Checkbox, Form, Input, Radio, Select, Space, Steps, Typography, message } from 'antd';
import { autoRunTask } from '../../tasks/stateMachine';
import { useEffect, useState } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';
import { createProject, createTask, getProject, getProjectConfig, getTools, listAllProjects } from '../../api/client';
import { SCAN_MODE, REVIEW_DEPTH, zh } from '../../dict';
import PageHeader from '../../components/PageHeader';
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

export default function TaskNewPage() {
  usePageTitle('新建任务');
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

  // ADR-203: 响应形状经 client.ts 类型化端点锚定（不再页面内 as-cast）。
  // 2026-09-11 用户报障（建任务引导）：isError 显性化 + 重试（此前失败静默空列表，
  // 用户只看到无法选择的下拉）；深链 ?project_id= 供项目详情页直达预选。
  const { data: projects, isError: projectsError, refetch: refetchProjects } = useQuery({
    queryKey: ['projects'],
    queryFn: async () => ({ projects: await listAllProjects() }), // B5-P2-7: 全量翻页
  });
  // 深链预选：仅当 project_id 命中列表项才预选，未命中保持未选（不猜 ID）
  const wantedProjectId = searchParams.get('project_id') ?? '';
  useEffect(() => {
    if (!wantedProjectId || projectId) return;
    if ((projects?.projects ?? []).some((p) => p.project_id === wantedProjectId)) {
      setProjectId(wantedProjectId);
    }
  }, [wantedProjectId, projects, projectId]);
  // ADR-163: 仓库模式——项目配置 repo_url 时启动时后端自动 clone（唯一仓库通道）
  const { data: projInfo } = useQuery({
    queryKey: ['project', projectId],
    queryFn: () => getProject(projectId),
    enabled: !!projectId,
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
    enabled: !!projectId,
  });
  const sourceText = repoURL
    ? `仓库自动拉取（${repoURL}）`
    : projConfig?.config?.upload_file_name
      ? `项目压缩包：${projConfig.config.upload_file_name}`
      : projConfig?.config?.upload_file_id
        ? `项目压缩包（${projConfig.config.upload_file_id}）`
        : '';
  const repoMode = !!repoURL;
  const { data: tools, isLoading: toolsLoading } = useQuery({
    queryKey: ['tools'],
    queryFn: getTools,
    enabled: MODE_SPECS[mode]?.needsSastTools === true, // 14号 §3.3 ①：仅需工具的模式才探测
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
      navigate(`/tasks/${tid}`);
    },
    // ADR-154: 此前创建失败静默（无 onError），用户停在确认页无任何反馈
    onError: (e) => message.error(`任务创建失败：${(e as Error).message}`),
  });

  const spec = MODE_SPECS[mode];
  const executableTools = (tools?.tools ?? []).filter((t) => t.valid);
  const unusableTools = (tools?.tools ?? []).filter((t) => !t.valid);

  const stepContent: Record<number, React.ReactNode> = {
    0: (
      <div>
        {/* 建任务引导（2026-09-11 用户报障）：项目列表加载失败显性化 + 可重试（此前失败
            静默，下拉永远为空无从归因） */}
        {projectsError && (
          <Alert
            type="error"
            showIcon
            style={{ marginBottom: 16 }}
            message="项目列表加载失败"
            description="项目服务暂不可用，无法选择项目。"
            action={<Button size="small" onClick={() => refetchProjects()}>重试</Button>}
          />
        )}
        <Select
          style={{ width: 420 }}
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
        {/* 就地创建：输入名称即建项目并自动选用（第 1 步闭环，无项目时不必离开向导） */}
        <Space.Compact style={{ width: 420, marginTop: 12 }}>
          <Input
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
        </Space.Compact>
      </div>
    ),
    1: (
      <Radio.Group
        value={mode}
        onChange={(e) => setMode(e.target.value)}
        options={Object.entries(SCAN_MODE)
          // ADR-182: 弃用模式不进新建入口；R: UNSPECIFIED 展示键不进新建入口
          // （MODE_SPECS 缺项 → needsSastTools undefined → sast_tools:[] → P-26 同型必 FAILED 复活）
          .filter(([value]) => !MODE_SPECS[value]?.deprecated && value !== 'SCAN_MODE_UNSPECIFIED')
          .map(([value, label]) => ({ value, label: `${label} —— ${MODE_SPECS[value]?.blurb ?? ''}` }))}
      />
    ),
    2: (
      <Form form={form} layout="vertical" style={{ maxWidth: 560 }} onFinish={(v) => { setParams(v); setStep(3); }}>
        {spec?.needsSastTools && (
          <>
            <Form.Item
              name="sast_tools"
              label="SAST 工具（仅列可执行工具）"
              rules={[{ required: true, message: '至少选择一个工具' }]}
            >
              <Select
                mode="multiple"
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
        <Button type="primary" htmlType="submit">
          下一步：确认
        </Button>
      </Form>
    ),
    3: (
      <Card style={{ maxWidth: 560 }}>
        <Typography.Paragraph>
          项目 <b>{projInfo?.name ?? projectId}</b> ｜ 模式 <b>{zh(SCAN_MODE, mode)}</b>
        </Typography.Paragraph>
        {/* ADR-154: 回显第2步参数（此前确认页不可见工具/路径，参数静默丢失无任何提示） */}
        <Typography.Paragraph type="secondary" style={{ marginBottom: 4 }}>
          {spec?.needsSastTools && (
            <>
              SAST 工具：<b>{(params.sast_tools ?? []).join('、') || '—'}</b>
              <br />
            </>
          )}
          源码来源（项目级，不可在此更改）：<b>{sourceText || '未配置——启动将失败，请先在项目页补齐'}</b>
          {spec?.needsReviewConfig && (
            <>
              <br />
              审核深度：<b>{params.review_depth ? zh(REVIEW_DEPTH, params.review_depth) : '—'}</b>
            </>
          )}
        </Typography.Paragraph>
        <Typography.Paragraph style={{ marginTop: 12 }}>
          <Checkbox
            checked={autoStart}
            onChange={(e: { target: { checked: boolean } }) => setAutoStart(e.target.checked)}
          >
            创建后立即启动（不勾则停在待启动，需在详情页手动点启动）
          </Checkbox>
        </Typography.Paragraph>
        <Button
          type="primary"
          loading={create.isPending}
          onClick={() => create.mutate(params)}
        >
          创建任务
        </Button>
      </Card>
    ),
  };

  return (
    <div style={{ maxWidth: 720 }}>
      {/*  PageHeader（Title 默认 margin-top 与 Content padding 叠加的 48px 空带归零） */}
      <PageHeader title="新建扫描任务" />
      <Steps
        items={[{ title: '项目' }, { title: '模式' }, { title: '参数' }, { title: '确认' }]}
        current={step}
        style={{ marginBottom: 24 }}
      />
      {stepContent[step]}
      <div style={{ marginTop: 16 }}>
        {step > 0 && <Button style={{ marginRight: 8 }} onClick={() => setStep(step - 1)}>上一步</Button>}
        {step === 0 && <Button type="primary" disabled={!projectId} onClick={() => setStep(1)}>下一步</Button>}
        {step === 1 && <Button type="primary" disabled={!mode} onClick={() => setStep(2)}>下一步</Button>}
      </div>
    </div>
  );
}
