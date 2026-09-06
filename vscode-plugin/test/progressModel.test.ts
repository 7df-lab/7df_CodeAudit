import * as assert from 'assert';
import {
  appendAiChunk,
  applyFrame,
  buildProgressItems,
  createProgressState,
  estimatePercent,
  fmtBytes,
  fmtDuration,
  logIdAfter,
  parseTsMs,
  sandboxPackCheck,
  stageLabel,
  taskStatusLabel,
} from '../src/progressModel';
import type { TaskSnapshot } from '../src/types';

const b64 = (s: string): string => Buffer.from(s, 'utf8').toString('base64');

const frame = (over: Partial<TaskSnapshot> = {}): TaskSnapshot => ({
  task: {
    task_id: 'gw-abc12345',
    project_id: 'p1',
    scan_mode: 'SCAN_MODE_PARALLEL',
    sast_tools: [],
    status: 'TASK_STATUS_RUNNING',
    stages: [],
    error_message: '',
  },
  progress: null,
  logs: null,
  ai: null,
  ...over,
});

const STAGES = [
  { stage_id: 'st-1', type: 'STAGE_TYPE_CODE_ANALYSIS', status: 'STAGE_STATUS_COMPLETED', error_message: '', started_at: '2026-09-03T01:00:00Z', completed_at: '2026-09-03T01:00:02.5Z' },
  { stage_id: 'st-2', type: 'STAGE_TYPE_SAST_SCAN', status: 'STAGE_STATUS_RUNNING', error_message: '', started_at: '2026-09-03T01:00:03Z', completed_at: null },
  { stage_id: 'st-3', type: 'STAGE_TYPE_AI_INFERENCE', status: 'STAGE_STATUS_PENDING', error_message: '', started_at: null, completed_at: null },
];

describe('progressModel：applyFrame 帧归并（ADR-189 四路同构帧）', () => {
  it('状态/百分比/阶段取值：progress 路优先，overall_percent 钳位到 [0,100]', () => {
    const s = createProgressState('t1');
    applyFrame(s, frame({ progress: { task_id: 't1', status: 'TASK_STATUS_RUNNING', overall_percent: 42.6, stages: STAGES }, task: { ...frame().task, stages: [] } }));
    assert.strictEqual(s.percent, 43);
    assert.strictEqual(s.stages.length, 3);
    applyFrame(s, frame({ progress: { task_id: 't1', status: 'TASK_STATUS_RUNNING', overall_percent: 120, stages: [] } }));
    assert.strictEqual(s.percent, 100);
  });

  it('无 progress 帧按阶段完成度估算（完成+跳过算 1，运行中算 0.5）', () => {
    assert.strictEqual(estimatePercent(STAGES), 50); // 1 + 0.5 = 1.5 / 3
    const s = createProgressState('t1');
    applyFrame(s, frame({ task: { ...frame().task, stages: STAGES } }));
    assert.strictEqual(s.percent, 50); // 回退 task.stages 同口径
  });

  it('logs 按 log_id 数值序增量去重（十进制串跨位数字典序失效的回归锁）', () => {
    assert.ok(logIdAfter('10', '9'), '数值上 10 > 9，但字典序 "10" < "9"');
    const s = createProgressState('t1');
    applyFrame(s, frame({ logs: { logs: [{ log_id: '9', task_id: 't1', ts_ms: '1', level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: 'a' }] } }));
    // 轮询重发 9 + 新增 10：只应并入 10
    applyFrame(s, frame({ logs: { logs: [
      { log_id: '9', task_id: 't1', ts_ms: '1', level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: 'a' },
      { log_id: '10', task_id: 't1', ts_ms: '2', level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: 'b' },
    ] } }));
    assert.deepStrictEqual(s.logs.map((l) => l.log_id), ['9', '10']);
    assert.strictEqual(s.lastLogId, '10');
  });

  it('ai chunk 增量追加：next_cursor 未推进的帧丢弃', () => {
    const s = createProgressState('t1');
    applyFrame(s, frame({ ai: { chunk: b64('你好'), next_cursor: '6', complete: false, total_bytes: '6' } }));
    assert.strictEqual(s.aiText, '你好');
    assert.strictEqual(s.aiCursor, 6);
    applyFrame(s, frame({ ai: { chunk: b64('你好'), next_cursor: '6', complete: false, total_bytes: '6' } })); // 重发
    assert.strictEqual(s.aiText, '你好');
    applyFrame(s, frame({ ai: { chunk: b64('，世界'), next_cursor: '15', complete: false, total_bytes: '15' } }));
    assert.strictEqual(s.aiText, '你好，世界');
    assert.strictEqual(s.aiCursor, 15);
    assert.strictEqual(s.aiTotalBytes, 15);
  });

  it('ai 全量重发（重连不带游标）：从 0 重置为最新全文，不重复拼接', () => {
    const s = createProgressState('t1');
    applyFrame(s, frame({ ai: { chunk: b64('abc'), next_cursor: '3', complete: false, total_bytes: '3' } }));
    // 重连后服务端从头推：chunk=全文，next_cursor 相同 → 视为重置而非丢弃
    applyFrame(s, frame({ ai: { chunk: b64('abcdef'), next_cursor: '6', complete: true, total_bytes: '6' } }));
    assert.strictEqual(s.aiText, 'abcdef');
    assert.strictEqual(s.aiCursor, 6);
    assert.strictEqual(s.aiComplete, true);
  });

  it('version 只在内容演进时递增（调用方据此避免无谓重渲染）', () => {
    const s = createProgressState('t1');
    const f = frame({ progress: { task_id: 't1', status: 'TASK_STATUS_RUNNING', overall_percent: 10, stages: [] } });
    applyFrame(s, f);
    const v = s.version;
    applyFrame(s, f); // 完全相同的一帧
    assert.strictEqual(s.version, v);
    applyFrame(s, frame({ progress: { task_id: 't1', status: 'TASK_STATUS_RUNNING', overall_percent: 12, stages: [] } }));
    assert.strictEqual(s.version, v + 1);
  });
});

describe('progressModel：appendAiChunk 游标衔接（纯函数边界）', () => {
  it('chunk 覆盖已见前缀时只追加新增尾部（多字节字符不撕裂）', () => {
    // 已见 6 字节（"你好"），重发 chunk = "你好，世界"（15 字节）→ 只追加 "，世界"
    const r = appendAiChunk('你好', 6, '你好，世界', 15);
    assert.ok(r);
    assert.strictEqual(r.text, '你好，世界');
    assert.strictEqual(r.cursor, 15);
  });

  it('跳段（chunkStart > cursor）整体重置', () => {
    const r = appendAiChunk('x', 3, 'yz', 10); // chunkStart=8 > 3
    assert.deepStrictEqual(r, { text: 'yz', cursor: 10 });
  });

  it('cursor=0 首帧直通', () => {
    assert.deepStrictEqual(appendAiChunk('', 0, 'hi', 2), { text: 'hi', cursor: 2 });
  });
});

describe('progressModel：buildProgressItems 进度树', () => {
  const mkState = (): ReturnType<typeof createProgressState> => {
    const s = createProgressState('gw-abcdef12');
    applyFrame(s, frame({
      progress: { task_id: 'gw-abcdef12', status: 'TASK_STATUS_RUNNING', overall_percent: 42, stages: STAGES },
      logs: { logs: [{ log_id: '1', task_id: 'gw-abcdef12', ts_ms: '1756851600000', level: 'TASK_LOG_LEVEL_INFO', source: 'sandbox', message: '沙箱已就绪' }] },
    }));
    return s;
  };

  it('任务头 + 阶段节点（图标/中文标签/时长）+ AI 入口', () => {
    const items = buildProgressItems(mkState());
    assert.strictEqual(items[0].kind, 'task');
    assert.match(items[0].description!, /运行中 · 42% · 轮询/);
    const stages = items.filter((n) => n.kind === 'stage');
    assert.deepStrictEqual(stages.map((n) => n.icon), ['check', 'sync', 'clock']);
    assert.strictEqual(stages[0].label, '代码分析');
    assert.strictEqual(stages[0].description, '完成 · 2.5s');
    const ai = items.find((n) => n.kind === 'ai')!;
    assert.strictEqual(ai.icon, 'robot');
    assert.strictEqual(ai.description, '暂无内容');
  });

  it('AI 有内容时显示字节量与流式/收束状态；失败阶段进 tooltip 与 contextValue', () => {
    const s = mkState();
    applyFrame(s, frame({ ai: { chunk: b64('x'.repeat(2048)), next_cursor: '2048', complete: false, total_bytes: '2048' } }));
    const ai = buildProgressItems(s).find((n) => n.kind === 'ai')!;
    assert.strictEqual(ai.description, '2.0 KB · 流式接收中');
    const failed = [...s.stages, { stage_id: 'st-4', type: 'STAGE_TYPE_SAST_SCAN', status: 'STAGE_STATUS_FAILED', error_message: 'semgrep OOM', started_at: null, completed_at: null }];
    applyFrame(s, frame({ progress: { task_id: s.taskId, status: 'TASK_STATUS_RUNNING', overall_percent: 42, stages: failed } }));
    const st = buildProgressItems(s).find((n) => n.id === 'st-4')!;
    assert.strictEqual(st.contextValue, 'stageFailed');
    assert.match(st.tooltip!, /semgrep OOM/);
  });

  it('异常终态追加失败摘要节点（含最近日志）', () => {
    const s = mkState();
    applyFrame(s, frame({ task: { ...frame().task, status: 'TASK_STATUS_DEAD' } }));
    const f = buildProgressItems(s).find((n) => n.id === 'failure');
    assert.ok(f);
    assert.match(f.description!, /沙箱已就绪/);
  });
});

describe('progressModel：格式化辅助', () => {
  it('parseTsMs 容忍 9 位小数秒（protojson Timestamp）', () => {
    assert.strictEqual(parseTsMs('2026-09-03T01:00:00.123456789Z'), parseTsMs('2026-09-03T01:00:00.123Z'));
    assert.strictEqual(parseTsMs(null), null);
    assert.strictEqual(parseTsMs('not-a-time'), null);
  });

  it('fmtDuration / fmtBytes / 标签映射', () => {
    assert.strictEqual(fmtDuration(2_500), '2.5s');
    assert.strictEqual(fmtDuration(95_000), '1m 35s');
    assert.strictEqual(fmtDuration(3_600_000), '1h0m');
    assert.strictEqual(fmtBytes(512), '512 B');
    assert.strictEqual(fmtBytes(2048), '2.0 KB');
    assert.strictEqual(stageLabel('STAGE_TYPE_AI_INFERENCE', 'x'), 'AI 推理');
    assert.strictEqual(stageLabel('STAGE_TYPE_WHATEVER', 'st-9'), 'st-9');
    assert.strictEqual(taskStatusLabel('TASK_STATUS_CANCELLED'), '已取消');
  });
});

describe('progressModel：沙箱收包校验（sandboxPackCheck）', () => {
  const packLog = (n: number) => [{ message: `项目打包完成 /sandbox/project（${n} 字节）→ 上传沙箱` }];

  it('日志尚无"打包完成"行 → null（不判定）', () => {
    assert.strictEqual(sandboxPackCheck([], 901191), null);
    assert.strictEqual(sandboxPackCheck([{ message: '沙箱就绪（wait-ready 通过）' }], 901191), null);
  });

  it('解析沙箱打包字节数；近空包（32B 空 tar.gz 实测签名）判废', () => {
    assert.deepStrictEqual(sandboxPackCheck(packLog(32), 901191), { received: 32, tooSmall: true });
    assert.deepStrictEqual(sandboxPackCheck(packLog(0), 901191), { received: 0, tooSmall: true });
  });

  it('健康项目包放行：重打包 tar.gz 与上传 zip 不等属正常，只看下限', () => {
    // 901KB zip 解包重打包 ~1.2MB（≠ 上传大小）→ 放行
    assert.deepStrictEqual(sandboxPackCheck(packLog(1_234_567), 901_191), { received: 1_234_567, tooSmall: false });
    // 小工程（251B zip）解包重打包 700B ≥ max(64, 251/100) → 放行
    assert.deepStrictEqual(sandboxPackCheck(packLog(700), 251), { received: 700, tooSmall: false });
  });

  it('取最近一条打包完成行；message 缺失/无字节 → received=0 判废', () => {
    const logs = [...packLog(1_000_000), { message: '项目打包完成 /x（32 字节）→ 上传沙箱' }];
    assert.deepStrictEqual(sandboxPackCheck(logs, 901_191), { received: 32, tooSmall: true });
    assert.deepStrictEqual(sandboxPackCheck([{ message: '项目打包完成（异常）' }], 901_191), { received: 0, tooSmall: true });
  });
});
