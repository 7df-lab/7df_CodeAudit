// 任务创建向导（14号 §3.3 ①；04 §3 五模式分流）——弹窗形态
// 全站"创建"类操作统一为 Modal（新建项目/项目编辑/用户/Provider 一致），任务创建原为
// 唯一独立跳页+全宽向导+侧栏——交互逻辑断裂且宽屏留白失衡。现重构为 TaskNewModal
// （TasksPage 按钮直开，与新建项目同构）；默认导出保留 /tasks/new 薄壳宿主页，深链
// ?project_id= 不死链。Step4 创建 → POST /v1/tasks（网关生成幂等键）；config 不再承载
// 任务级源码键——2026-09-09 项目层级决定源代码仓库，向导不提供重新上传/
// 指定仓库/手填路径（项目 config.upload_file_id / repo_url 由 task-service 启动时解析，
// ADR-203 兜底链）。2026-09-11 用户报障（建任务引导）：项目列表加载失败 Alert+重试 /
// 空列表引导去项目页 / 项目 Select 可搜索 / ?project_id= 深链预选（项目详情页直达）。
// 2026-09-14 视觉重设计（体系内精致，零新色零 webfont）：
//   ① 模式步"策略卡"——每卡顶部等宽引擎流一行（五模式的本质差异=引擎编排，结构即信息）；
//   ② 机器标识（项目/工具 ID、引擎流）走 MONO_FONT（design-tokens 等宽嗓音规则）；
//   ③ 色/边框全部 theme.useToken() 语义 token。业务逻辑与请求面零变更（测试锚原样保留）。
// 2026-10-03 结构化重设计（用户裁决"编排/模式混排一列不合理"）：
//   ④ 第 1 步更名"执行方案"，拆两组带眉题卡阵——执行编排（2 卡并排）× 扫描模式（5 卡
//      两列网格）；图编排选中 → 模式组收起为接管条（紫左边框=GRAPH_PLAN_BAND 编排
//      语义，流线等宽嗓音贯穿两组），替代"禁用灰卡+漂浮 Alert"。提交 payload 零变更。
// 流程统一批：
//   ⑤ MODE_SPECS/DEFAULT_SCAN_MODE 迁入 src/tasks/scanModes.ts 单一事实源（本文件
//      re-export 保测试锚点）；任务 payload 组装改经 buildScanTaskPayload（此前与
//      项目页自动链/详情页快捷弹窗三份拷贝——B5-P1-3 漂移事故的根治）。
//   ⑥ 模式默认值跟随所选项目 default_scan_mode（P0-2：此前该字段只在项目创建瞬间的
//      自动链被消费一次，向导一律无视；用户手动改过模式则不覆盖）。
//   ⑦ 就地创建项目补齐字段（P1-5：源码二选一+默认分支，复用 ArchiveUploadDragger）——
//      建完即配齐源码，不再叠 ProjectSourceModal 二级弹窗补配（2026-10-03 报障收口的
//      临时形态退役；该弹窗仍服务参数步无来源警告的"立即配置"）。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Checkbox, Form, Input, Modal, Radio, Select, Steps, Tag, Typography, message } from 'antd';
import { theme as antdTheme } from 'antd';
import { autoRunTask } from '../../tasks/stateMachine';
import { useEffect, useState } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';
import { apiErrorMessage, createTask, errStatus, getProject, getProjectConfig, getTools, listAgents, listAllProjects } from '../../api/client';
import { capabilitySummary } from '../../components/AgentSwitchModal';
import { ArchiveUploadDragger, type ArchiveUploadValue } from '../../components/ArchiveUpload';
import { ProjectSourceModal } from '../../components/ProjectSourceModal';
import { SCAN_MODE, REVIEW_DEPTH, zh } from '../../dict';
import { PACK_ORCHESTRATIONS, findOrchestration } from '../../dict/orchestrations';
import { GRAPH_PLAN_BAND, MONO_FONT } from '../../dict/tokens';
import { usePageTitle } from '../../hooks/usePageTitle';
import { buildScanTaskPayload, DEFAULT_SCAN_MODE, GRAPH_ORCH, MODE_SPECS, resolveDefaultScanMode } from '../../tasks/scanModes';
import { createProjectWithSource } from '../../utils/projectCreate';

// 单一事实源已迁 src/tasks/scanModes.ts——re-export 保
// 既有 import 路径与测试锚点（TaskNewPage.test 的 MODE_SPECS 用例）。
export { MODE_SPECS, DEFAULT_SCAN_MODE } from '../../tasks/scanModes';
export type { ModeSpec } from '../../tasks/scanModes';

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
// 三宿主：TasksPage 按钮直开 / /tasks/new 薄壳页（深链
// ?project_id= 预选）/ 项目详情页悬浮窗（presetProjectId 预选——跳转 /tasks/new 会
// 断回程：薄壳关闭落任务列表而非来源详情页，故详情页原地挂载）。
export function TaskNewModal({ open, onClose, presetProjectId = '' }: { open: boolean; onClose: () => void; presetProjectId?: string }) {
  const { token } = antdTheme.useToken();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [searchParams] = useSearchParams();
  const [step, setStep] = useState(0);
  const [projectId, setProjectId] = useState<string>('');
  // 就地创建项目（第 1 步无项目时的第二条出路）——P1-5补齐
  // 字段：源码二选一 + 默认分支，与项目页创建弹窗同构（ArchiveUploadDragger/二选一
  // 拦截），建完即配齐源码；sourceModalFor 退役就地创建触发（该弹窗仍服务参数步
  // 无来源警告的"立即配置"）
  const [newProjForm] = Form.useForm();
  const [newProjUpload, setNewProjUpload] = useState<ArchiveUploadValue>({ fileId: null, fileName: '' });
  const [sourceModalFor, setSourceModalFor] = useState<string>('');
  const [mode, setMode] = useState<string>(DEFAULT_SCAN_MODE); // ADR-182: 默认推荐模式C
  // P0-2：模式默认值跟随所选项目 default_scan_mode——
  // 用户在执行方案步手动改过（modeTouched）则不再跟随换项目而变
  const [modeTouched, setModeTouched] = useState(false);
  // 平台切片 7：任务级 Agent 选择（空=部署默认；TP20-T2 起清单端点化 /v1/agents——
  // 后端 CreateScanTask 格式校验+runtime LoadDescriptor 真校验兜底）
  const [agent, setAgent] = useState<string>('');
  // TP20-T2 清单端点化（计划 §3.1）：数据源=GET /v1/agents（创建页与切换 Modal 同源；
  // 硬编码清单退役）。失败=锁定默认项（空值=部署默认，仍可提交）——不回落硬编码。
  const { data: agentsData, isError: agentsError, error: agentsErrObj } = useQuery({
    queryKey: ['agents'],
    queryFn: listAgents,
    staleTime: 60_000,
  });
  const agentOptions = (agentsData?.agents ?? []).map((a) => ({
    value: a.is_default ? '' : a.id, // 空=部署默认（不发字段，旧后端兼容）
    name: a.id,
    tag: a.is_default ? '默认' : '',
    dialect: a.dialect,
    caps: capabilitySummary(a).join(' · '),
  }));
  // 编排形态。''=标准编排
  // （默认不发 pack 键——旧后端/默认行为零回归）；'audit-graph'=图编排（config.pack
  // 钉入，GraphSpec 短路 scan_mode）。目录见 src/dict/orchestrations.ts。
  const [orch, setOrch] = useState<string>('');
  //  2026-09-01（B3-5 文案纠偏）：审批流已废除——创建→启动直达，无"提交→批准"环节；
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
    setSourceModalFor('');
    setMode(DEFAULT_SCAN_MODE);
    setModeTouched(false);
    setOrch('');
    setAutoStart(true);
    setParams({});
    form.resetFields();
    newProjForm.resetFields();
    setNewProjUpload({ fileId: null, fileName: '' });
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
  // 预选：URL ?project_id=（薄壳深链）或宿主 presetProjectId（详情页悬浮窗）二源取一；
  // 仅当命中列表项才预选，未命中保持未选（不猜 ID）
  const wantedProjectId = searchParams.get('project_id') ?? presetProjectId ?? '';
  useEffect(() => {
    if (!open || !wantedProjectId || projectId) return;
    if ((projects?.projects ?? []).some((p) => p.project_id === wantedProjectId)) {
      setProjectId(wantedProjectId);
    }
  }, [open, wantedProjectId, projects, projectId]);
  // P0-2 模式跟随：选中项目（含深链预选/就地创建后选用）且用户未在执行方案步手改过
  // 模式时，默认模式=项目 default_scan_mode（空值/弃用/UNSPECIFIED 由
  // resolveDefaultScanMode 回落 DEFAULT_SCAN_MODE——不把死枚举送进创建面）
  useEffect(() => {
    if (!projectId || modeTouched) return;
    const want = resolveDefaultScanMode(projects?.projects.find((p) => p.project_id === projectId));
    setMode((prev) => (prev === want ? prev : want));
  }, [projectId, projects, modeTouched]);
  // 编排深链预选（编排管理页 CTA 落点）：仅命中目录且开放才预选
  const wantedOrch = searchParams.get('orchestration') ?? '';
  useEffect(() => {
    if (!open || !wantedOrch || orch) return;
    // 编排 CRUD 批：接受任意非空编排名（CRUD 产物不在静态目录）
    setOrch(wantedOrch);
  }, [open, wantedOrch, orch]);

  // ADR-163: 仓库模式——项目配置 repo_url 时启动时后端自动 clone（唯一仓库通道）
  const { data: projInfo, isSuccess: projInfoOk } = useQuery({
    queryKey: ['project', projectId],
    queryFn: () => getProject(projectId),
    enabled: open && !!projectId,
  });

  // 就地创建项目（第 1 步无项目时的第二条出路，与"前往项目页创建"并存）——P1-5：
  // 表单补齐源码二选一+默认分支（与项目页创建弹窗同构），建完即配齐源码并自动选用；
  // 创建链经 createProjectWithSource 共享（审查修复批：与 ProjectCreateModal 同源，
  // 二选一拦截/config 落盘/部分成功标记不再双份）；configFailed=部分成功（如实
  // warning，源码可在参数步警告"立即配置"补配）
  const createProj = useMutation({
    mutationFn: async (v: { name: string; repo_url?: string; default_branch: string }) =>
      createProjectWithSource({ ...v, default_scan_mode: DEFAULT_SCAN_MODE }, newProjUpload),
    onSuccess: ({ project, configFailed }) => {
      if (configFailed) {
        message.warning('项目已创建，但压缩包关联失败——可稍后在参数步"立即配置"补配');
      } else {
        message.success(`项目「${project.name}」已创建并选用`);
      }
      setProjectId(project.project_id);
      newProjForm.resetFields();
      setNewProjUpload({ fileId: null, fileName: '' });
      qc.invalidateQueries({ queryKey: ['projects'] });
    },
    onError: (e) => message.error(`项目创建失败：${apiErrorMessage(e)}`),
  });

  const repoURL = projInfo?.repo_url ?? '';
  // 项目级源码来源展示
  const {
    data: projConfig,
    isSuccess: projCfgOk,
    isError: projCfgErr,
    error: projCfgError,
  } = useQuery({
    queryKey: ['project-config', projectId],
    queryFn: () => getProjectConfig(projectId),
    enabled: open && !!projectId,
  });
  // 展示序=解析链序（task-service：任务级 upload_file_id（API 兼容保留，向导不写）→
  // 项目上传件 → repo_url，ADR-200/203）。此前仓库在先——与并存存量项目（创建时
  // 仓库+压缩包同填）实际扫描用的源码不一致
  const sourceText = projConfig?.config?.upload_file_name
    ? `项目压缩包：${projConfig.config.upload_file_name}`
    : projConfig?.config?.upload_file_id
      // 无名上传（API/e2e 上传路径不写名、原名服务端无档）——统一短文案；裸
      // file_id 对用户无信息量
      ? `项目压缩包（名称未记录）`
      : repoURL
        ? `仓库自动拉取（${repoURL}）`
        : '';
  // "未配置"判定门（与详情页同口径，评审 P2）：两查询都已决（项目 GET 成功 + config
  // 成功或 404=从未配置）且确无来源才亮警告——加载期/服务故障（5xx）不闪"任务启动将
  // 失败"假警告+假"立即配置"按钮
  const projCfgSettled = projCfgOk || (projCfgErr && errStatus(projCfgError) === 404);
  const noSource = projInfoOk && projCfgSettled && !sourceText;
  // isError/refetch：交互审计 Top4-②——工具服务失败时下拉永远为空、必填报
  // "至少选择一个工具"，无从归因无重试（第 0 步项目列表同型已修，此处补齐）
  const { data: tools, isLoading: toolsLoading, isError: toolsError, refetch: refetchTools } = useQuery({
    queryKey: ['tools'],
    queryFn: getTools,
    enabled: open && orch !== GRAPH_ORCH && MODE_SPECS[mode]?.needsSastTools === true, // 14号 §3.3 ①：仅需工具的模式才探测（图编排接管时不探测）
  });

  const create = useMutation({
    mutationFn: async (values: { sast_tools?: string[]; review_depth?: string; review_opts?: string[] }) => {
      const config: Record<string, string> = {};
      // 2026-09-09  不再写任务级源码键（upload_file_id/project_path）——
      // 源码来源由项目解析；config 只承载审核类键
      if (values.review_depth) config.review_depth = values.review_depth;
      if (values.review_opts?.length) {
        config.assess_severity = String(values.review_opts.includes('assess_severity'));
        config.verify_location = String(values.review_opts.includes('verify_location'));
        config.generate_suggestions = String(values.review_opts.includes('generate_suggestions'));
      }
      // payload 统一经 buildScanTaskPayload 组装（图编排 pack 钉入/AI_ONLY 载体、
      // sast_tools 口径、agent 空不发——与项目页自动链同一事实源；详情页快捷弹窗
      // 已退役，创建任务单一 UI=本向导，用户裁决 2026-10-07）
      return createTask(buildScanTaskPayload({
        project_id: projectId,
        scan_mode: mode,
        sast_tools: values.sast_tools,
        config,
        agent: agent || undefined,
        ...(orch ? { pack: orch } : {}),
      }));
    },
    onSuccess: (resp) => {
      const tid = resp.task_id;
      // 任务域全集缓存（总览/任务页统计带/报告中心索引，I-99）：创建即失效，
      // 统计带回任务页不滞后（staleTime 60s 内的旧计数会冒充总数——本批口径的对称面）
      qc.invalidateQueries({ queryKey: ['tasks-all'] });
      qc.invalidateQueries({ queryKey: ['tasks-page'] });
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
    onError: (e) => message.error(`任务创建失败：${apiErrorMessage(e)}`),
  });

  // 图编排接管时参数步按"无 SAST 工具"分支（scan_mode 载体=AI_ONLY 已无工具面；
  // 图 executor 的审计材料=上传件与 intent，与 SAST 工具无关）
  const graphOrch = orch === GRAPH_ORCH;
  const spec = graphOrch
    ? { needsSastTools: false as boolean | undefined, needsReviewConfig: false as boolean | undefined }
    : MODE_SPECS[mode];
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
            display: 'flex', alignItems: 'flex-start', margin: 0, padding: '12px 16px', width: '100%', height: '100%',
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
                // 空列表引导：此前空态无任何出路提示
                <Typography.Text type="secondary">
                  暂无项目——下方就地创建，或<Link to="/projects/new">前往新建项目页</Link>
                </Typography.Text>
              )}
            options={(projects?.projects ?? []).map((p) => ({ value: p.project_id, label: `${p.name} (${p.project_id})` }))}
          />
        </div>
        {/* 就地创建（P1-5，流程统一批 2026-10-07）：表单与项目页创建弹窗同构——名称+
            源码二选一+默认分支，建完即配齐源码并自动选用（第 1 步闭环，无项目时不必
            离开向导；此前单输入框建"裸项目"+叠源码弹窗补配的两段式退役） */}
        <div>
          <Typography.Text type="secondary" style={{ display: 'block', marginBottom: 8 }}>还没有项目？就地创建一个（建完即配齐源码）</Typography.Text>
          <Form form={newProjForm} layout="vertical" onFinish={(v) => createProj.mutate(v as { name: string; repo_url?: string; default_branch: string })}>
            <Form.Item name="name" label="名称" rules={[{ required: true }]} style={{ marginBottom: 12 }}>
              <Input placeholder="新项目名称" />
            </Form.Item>
            <Form.Item
              name="repo_url"
              label="仓库地址（与上传压缩包二选一）"
              extra={newProjUpload.fileId ? '已上传压缩包——此栏可留空，项目以压缩包为源码' : '不填则必须上传代码压缩包'}
              style={{ marginBottom: 12 }}
            >
              <Input placeholder="如 https://git.example.com/team/repo.git" />
            </Form.Item>
            <Form.Item label="代码压缩包（.zip/.tar.gz，≤100MB）" style={{ marginBottom: 12 }}>
              <ArchiveUploadDragger value={newProjUpload} onChange={setNewProjUpload} />
            </Form.Item>
            <Form.Item name="default_branch" label="默认分支" initialValue="main" rules={[{ required: true }]} style={{ marginBottom: 12 }}>
              <Input />
            </Form.Item>
            <Button type="primary" loading={createProj.isPending} onClick={() => newProjForm.submit()}>
              创建并选用
            </Button>
          </Form>
        </div>
      </div>
    ),
    1: (
      // 2026-10-03 结构化重设计：两个正交决策分组呈现——执行编排（怎么跑）× 扫描模式
      // （审什么流水线）。图编排选中时模式组整体收起为接管条（结构即信息），取代
      // 旧"5 卡禁用灰置 + 漂浮 Alert"的负信息布局。
      <div style={{ display: 'flex', flexDirection: 'column', gap: 20 }}>
        <section style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          <div style={{ display: 'flex', alignItems: 'baseline', gap: 8 }}>
            <Typography.Text strong style={{ fontSize: 13 }}>执行编排</Typography.Text>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>任务由什么结构执行</Typography.Text>
          </div>
          <Radio.Group
            value={orch}
            onChange={(e) => setOrch(e.target.value)}
            style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: 12 }}
          >
            {/* 编排形态（graph-reseed 后续）：负载与流程定义。引擎流一行沿用策略卡
                签名语言——图编排的三段流与五模式卡视觉同构，结构即信息。流线紫色=
                编排语义（GRAPH_PLAN_BAND，与图视图计划批次带同源）。 */}
            {[
              {
                key: '',
                name: '标准编排',
                tag: '默认',
                flow: '单会话 → 全量执行 → 单一产出',
                desc: '单 DSH 会话按所选扫描模式一次执行到底（既有默认行为）',
              },
              {
                key: 'audit-graph',
                name: '图编排 · 代码审计',
                tag: '图编排',
                flow: '审计面规划 → 逐面深度审计 → 复核去重',
                desc: '按审计面拆解执行；支持服务端注册的自定义编排（经编排管理页创建）。',
              },
            ].map((o) => {
              const selected = orch === o.key;
              return (
                <Radio
                  key={o.key || 'std'}
                  value={o.key}
                  style={{
                    display: 'flex', alignItems: 'flex-start', margin: 0, padding: '12px 16px', width: '100%', height: '100%',
                    border: `1px solid ${selected ? token.colorPrimary : token.colorBorderSecondary}`,
                    borderRadius: token.borderRadiusLG,
                    background: selected ? token.colorPrimaryBg : token.colorBgContainer,
                    transition: 'border-color 0.2s, background 0.2s',
                  }}
                >
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 4, minWidth: 0 }}>
                    <span style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
                      <b>{o.name}</b>
                      <Tag style={{ marginInlineEnd: 0 }} color={o.key ? 'purple' : 'default'}>{o.tag}</Tag>
                    </span>
                    <span style={{ fontFamily: MONO_FONT, fontSize: 12.5, color: o.key ? GRAPH_PLAN_BAND.text : token.colorPrimary, letterSpacing: 0.2 }}>
                      {o.flow}
                    </span>
                    <Typography.Text type="secondary" style={{ fontSize: 13 }}>{o.desc}</Typography.Text>
                  </div>
                </Radio>
              );
            })}
          </Radio.Group>
        </section>
        <section style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          {graphOrch ? (
            // 接管条：图编排选中后模式组不渲染——一句说明 + 编排流线 + 自定义指引，
            // 替代旧禁用卡阵（占屏不说事）与漂浮 Alert。
            <div
              style={{
                border: `1px solid ${token.colorBorderSecondary}`,
                borderLeft: `3px solid ${GRAPH_PLAN_BAND.border}`,
                borderRadius: token.borderRadiusLG,
                background: token.colorFillQuaternary,
                padding: '12px 16px',
                display: 'flex', flexDirection: 'column', gap: 4,
              }}
            >
              <span style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
                <b>扫描模式由图编排接管</b>
                <Tag style={{ marginInlineEnd: 0 }} color="purple">图编排</Tag>
              </span>
              <span style={{ fontFamily: MONO_FONT, fontSize: 12.5, color: GRAPH_PLAN_BAND.text, letterSpacing: 0.2 }}>
                {findOrchestration('audit-graph')?.flow ?? '审计面规划 → 逐面深度审计 → 复核去重'}
              </span>
              <Typography.Text type="secondary" style={{ fontSize: 13 }}>
                执行形态由编排定义，无需另选扫描模式；自定义编排经编排管理页创建。
              </Typography.Text>
            </div>
          ) : (
            <>
              <div style={{ display: 'flex', alignItems: 'baseline', gap: 8 }}>
                <Typography.Text strong style={{ fontSize: 13 }}>扫描模式</Typography.Text>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>审计流水线如何组合</Typography.Text>
              </div>
              <Radio.Group
                value={mode}
                onChange={(e) => { setMode(e.target.value); setModeTouched(true); }}
                style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(300px, 1fr))', gap: 12 }}
              >
                {modeCards}
              </Radio.Group>
            </>
          )}
        </section>
      </div>
    ),
    2: (
      <Form form={form} layout="vertical" onFinish={(v) => { setParams(v); setStep(3); }}>
        {spec?.needsSastTools && (
          <>
            {toolsError && (
              <Alert
                type="error"
                showIcon
                style={{ marginBottom: 16 }}
                message="工具列表加载失败"
                description="工具服务暂不可用，无法选择 SAST 工具。"
                action={<Button size="small" onClick={() => refetchTools()}>重试</Button>}
              />
            )}
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
        <Form.Item label="Agent（AI 阶段执行引擎）">
          {/* 切片 7 复批（frontend-design）：选择卡组——通道形态视觉编码（行为差异
              即信息）：DSH=流式+工具提交（结构化发现实时回流）；Maka=整回合+文本
              结论（无逐字流，结论经报告与发现列表呈现）。 */}
          {agentsError && (
            <Alert
              type="warning"
              showIcon
              style={{ marginBottom: 8 }}
              message="Agent 清单加载失败——已锁定部署默认（仍可提交）"
              description={String((agentsErrObj as Error)?.message ?? '')}
            />
          )}
          <Radio.Group
            value={agent}
            onChange={(e) => setAgent(e.target.value)}
            disabled={agentsError || agentOptions.length === 0}
            style={{ display: 'grid', gap: 8 }}
          >
            {(agentOptions.length
              ? agentOptions
              : [{ value: '', name: '部署默认', tag: '默认', dialect: '', caps: '' }] // 失败/加载中=锁定默认项
            ).map((a) => (
              <Radio key={a.value || 'default'} value={a.value} style={{ marginInlineEnd: 0, width: '100%' }}>
                <div style={{ display: 'inline-flex', gap: 10, alignItems: 'baseline', padding: '2px 0' }}>
                  <span style={{ fontWeight: 600 }}>{a.name}</span>
                  {a.tag && <Tag style={{ marginInlineEnd: 0 }}>{a.tag}</Tag>}
                  {a.dialect && <span style={{ fontSize: 12, opacity: 0.55 }}>{a.dialect}</span>}
                </div>
                {a.caps && <div style={{ fontSize: 12, opacity: 0.65, paddingLeft: 24 }}>{a.caps}</div>}
              </Radio>
            ))}
          </Radio.Group>
        </Form.Item>
        {/* 2026-09-09 人类指令: 项目层级决定源代码仓库——任务向导不提供重新上传/指定
            仓库/手填路径；源码来源=项目 config/repo_url，只读呈现 */}
        {sourceText ? (
          <Alert
            type="info"
            showIcon
            style={{ marginBottom: 16 }}
            message={`源码来源（项目级）：${sourceText}`}
          />
        ) : noSource ? (
          <Alert
            type="warning"
            showIcon
            style={{ marginBottom: 16 }}
            message="该项目未配置源码来源（无仓库地址也无上传压缩包）——任务启动将失败，请先在项目详情页补配"
            // 直达补配（评审对齐：详情页建任务弹窗同款闭环，不必离开向导找入口）
            action={<Button size="small" onClick={() => setSourceModalFor(projectId)}>立即配置</Button>}
          />
        ) : null /* 两查询未决/故障（评审 P2）：不闪假警告，加载完再如实呈现 */}
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
          <Typography.Text type="secondary">
            {graphOrch ? '图编排 · 代码审计（接管执行流）' : zh(SCAN_MODE, mode)}
          </Typography.Text>
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
            <span style={{ fontFamily: MONO_FONT, fontSize: 13 }}>
              {graphOrch
                ? findOrchestration('audit-graph')?.flow ?? '—'
                : ENGINE_FLOW[mode] ?? '—'}
            </span>
          </div>
          {graphOrch && (
            <div style={{ display: 'flex', gap: 12, padding: '6px 0' }}>
              <Typography.Text type="secondary" style={{ flex: '0 0 210px' }}>负载编排</Typography.Text>
              <span style={{ fontFamily: MONO_FONT, fontSize: 13 }}>config.pack = audit-graph@最新</span>
            </div>
          )}
          <div style={{ display: 'flex', gap: 12, padding: '6px 0' }}>
            <Typography.Text type="secondary" style={{ flex: '0 0 210px' }}>Agent（AI 阶段）</Typography.Text>
            <span style={{ fontFamily: MONO_FONT, fontSize: 13 }}>{agent || '部署默认'}</span>
          </div>
          <div style={{ display: 'flex', gap: 12, padding: '6px 0' }}>
            <Typography.Text type="secondary" style={{ flex: '0 0 210px' }}>源码来源（项目级，不可在此更改）</Typography.Text>
            {sourceText
              ? <span>{sourceText}</span>
              : noSource
                ? <Typography.Text type="warning">未配置——启动将失败，请先在项目详情页补配</Typography.Text>
                : <Typography.Text type="secondary">读取中…</Typography.Text>}
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
        items={[{ title: '项目' }, { title: '执行方案' }, { title: '参数' }, { title: '确认' }]}
        current={step}
        style={{ marginBottom: 24 }}
      />
      {stepContent[step]}
      {/* 参数步无来源警告的"立即配置"补配弹窗（P1-5 后就地创建已带源码，本弹窗不再
          由就地创建链触发——服务存量无来源项目的参数步警告直达） */}
      <ProjectSourceModal
        open={!!sourceModalFor}
        projectId={sourceModalFor}
        onClose={() => setSourceModalFor('')}
      />
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
