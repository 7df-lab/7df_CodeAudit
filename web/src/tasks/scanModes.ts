// 扫描模式规格与任务 payload 组装的单一事实源。
// 来历：MODE_SPECS 原定义在 TaskNewPage.tsx 且被 ProjectsPage/ProjectDetailPage 跨页
// import；任务 payload 组装（sast_tools 口径）更是同构逻辑三份拷贝——B5-P1-3 已实锤
// 拷贝漂移事故（旧 NEEDS_TOOLS 集合漏模式D → 自动建任务必 FAILED）。本模块收敛：
//   ① MODE_SPECS/DEFAULT_SCAN_MODE（自 TaskNewPage 迁入，页面 re-export 保锚点兼容）
//   ② DEFAULT_SAST_TOOLS——项目自动链/快捷弹窗不经工具选择时的部署默认口径
//   ③ buildScanTaskPayload——三处创建入口（向导/项目页自动链/详情页快捷弹窗）共用
//   ④ resolveDefaultScanMode——项目 default_scan_mode 是任务创建入口的默认模式来源
//      （空值/弃用/UNSPECIFIED 回落 DEFAULT_SCAN_MODE；此前该字段仅在项目创建瞬间的
//      自动链被消费一次，向导与快捷弹窗一律无视——"默认"承诺未兑现）
import type { CreateTaskPayload } from '../api/client';

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

// 部署默认 SAST 工具：不经工具选择面的创建入口（项目页自动链/详情页快捷弹窗）用它。
// 显式给了工具（向导参数步）则原样透传；该口径此前散在三处字面量。
export const DEFAULT_SAST_TOOLS = ['opengrep'];

// 图编排名：仅 audit-graph 走 GraphSpec 短路（AI_ONLY 载体+
// 无工具面）——自定义编排（编排 CRUD 深链）仍发用户所选模式/工具，config.pack 照随。
// 判定口径与向导 graphOrch 同源，字面量收敛于此。
export const GRAPH_ORCH = 'audit-graph';

export interface ScanTaskBuildInput {
  project_id: string;
  /** 标准编排下用户所选扫描模式；图编排（pack 非空）时仅作日志语义，实际载体见 pack */
  scan_mode: string;
  /** 显式选择的工具（向导参数步，needsSastTools 模式下表单必填）；默认回落 DEFAULT_SAST_TOOLS */
  sast_tools?: string[];
  /** 审核类等任务 config 键；源码键禁入（E-19：源码来源由项目层级解析） */
  config?: Record<string, string>;
  /** 空=部署默认（不发字段，旧后端兼容） */
  agent?: string;
  /** 图编排名（如 'audit-graph'）：config.pack 钉入 + scan_mode 承载合法枚举 AI_ONLY */
  pack?: string;
}

// 任务创建 payload 统一组装——三处入口共用。图编排（pack=GRAPH_ORCH）：GraphSpec
// 短路模式 switch，AI_ONLY 为合法枚举载体、无工具面；**自定义编排**（pack 为其他名，
// 编排 CRUD 深链可达）：仅 config.pack 随行，模式/工具保持用户所选（与向导 graphOrch
// 判定同口径——审查修复批：此前 !!pack 一刀切把自定义编排也静默改写为 AI_ONLY）。
export function buildScanTaskPayload(input: ScanTaskBuildInput): CreateTaskPayload {
  const graph = input.pack === GRAPH_ORCH;
  const needsTools = !graph && MODE_SPECS[input.scan_mode]?.needsSastTools === true;
  return {
    project_id: input.project_id,
    scan_mode: graph ? 'SCAN_MODE_AI_ONLY' : input.scan_mode,
    sast_tools: needsTools
      ? (input.sast_tools?.length ? input.sast_tools : [...DEFAULT_SAST_TOOLS])
      : [],
    config: input.pack ? { ...input.config, pack: input.pack } : { ...input.config },
    ...(input.agent ? { agent: input.agent } : {}),
  };
}

// 任务创建入口的默认模式：跟随项目 default_scan_mode；空值/UNSPECIFIED/弃用模式
// （存量数据可能带）一律回落 DEFAULT_SCAN_MODE——不把死枚举送进创建面。
export function resolveDefaultScanMode(project?: { default_scan_mode?: string }): string {
  const m = project?.default_scan_mode ?? '';
  return m && m !== 'SCAN_MODE_UNSPECIFIED' && !MODE_SPECS[m]?.deprecated ? m : DEFAULT_SCAN_MODE;
}
