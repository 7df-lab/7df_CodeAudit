// 编排目录。
// 数据源注记：负载 pack 由服务端 configs/packs 配置文件钉定（非动态 CRUD）——
// 本目录是 web 侧产品化呈现；漂移兜底=创建时后端按注册表校验 fail-fast（400 带
// pack 名）。审计图模式依据=platform-slice5-6-graph-engine.md（B3 三权）+
// graph-reseed-task-handoff.md（重派种子服务审计重派节点）。

export interface PackOrchestration {
  key: string; // config.pack 值（服务端 pack 名）
  name: string;
  /** 引擎流一行（策略卡签名语言同构——等宽，结构即信息） */
  flow: string;
  form: 'graph' | 'session';
  tag?: string;
  description: string;
  /** 审计面/攻击面种子（图编排的初始拆解） */
  seedIntents: string[];
  constraints: {
    maxNodes: number;
    allowedTypes: string[];
    maxPlannerReplans: number;
    maxAttemptsPerNode: number;
  };
  timeouts: { planner: number; executor: number; verifier: number };
  /** 建任务额外要求（如授权目标——当前未开放则 enabled=false） */
  enabled: boolean;
  disabledReason?: string;
}

export const PACK_ORCHESTRATIONS: PackOrchestration[] = [
  {
    key: 'audit-graph',
    name: '图编排 · 代码审计',
    flow: '审计面规划 → 逐面深度审计 → 复核去重',
    form: 'graph',
    tag: '图编排',
    description:
      '三面独立审计图：planner 拆解审计面，executor 逐面深度审计（发现走标准提交通道落库），verifier 对照源文件去重复核。重派节点自动携带前任失败要点（种子契约）。',
    seedIntents: [
      '危险模式全库审计（SQL 注入/命令注入/硬编码凭据）',
      '输入处理与外部交互面（反序列化/SSRF/路径拼接）',
      '认证与会话链路（口令处理/令牌生成/越权检查点）',
    ],
    constraints: { maxNodes: 12, allowedTypes: ['planner', 'executor', 'verifier'], maxPlannerReplans: 3, maxAttemptsPerNode: 2 },
    timeouts: { planner: 180, executor: 900, verifier: 300 },
    enabled: true,
  },
  {
    key: 'audit-sample',
    name: '标准编排 · 单会话审计',
    flow: '单会话 → 全量审计 → 单一清单',
    form: 'session',
    description:
      '单 DSH 会话完成全量审计（与部署默认行为等价的双轨锚）。适合小仓库或希望一口气看完整审计过程的场景。',
    seedIntents: [],
    constraints: { maxNodes: 0, allowedTypes: [], maxPlannerReplans: 0, maxAttemptsPerNode: 0 },
    timeouts: { planner: 0, executor: 0, verifier: 0 },
    enabled: true,
  },
  {
    key: 'pentest',
    name: '图编排 · 渗透测试',
    flow: '攻击面规划 → 逐项探测 → 复核',
    form: 'graph',
    tag: '暂未开放',
    description:
      '渗透测试负载（planner/executor/verifier 图编排；靶标经授权目标注入）。依赖授权暴露面供给链，暂未开放建任务。',
    seedIntents: [
      '端口与服务指纹识别',
      'Web 入口与版本信息收集',
      '凭据与默认口令试探（只限授权范围）',
    ],
    constraints: { maxNodes: 24, allowedTypes: ['planner', 'executor', 'verifier'], maxPlannerReplans: 5, maxAttemptsPerNode: 2 },
    timeouts: { planner: 180, executor: 900, verifier: 300 },
    enabled: false,
    disabledReason: '授权暴露面供给链建设中——靶标名需可被沙箱解析/路由',
  },
];

export function findOrchestration(key: string): PackOrchestration | undefined {
  return PACK_ORCHESTRATIONS.find((o) => o.key === key);
}
