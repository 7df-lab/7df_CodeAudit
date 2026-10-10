// 字典完整性回归（14号 P2/P4：值域=proto 枚举，不自造）
import { REVIEW_DEPTH, DEPRECATED_SCAN_MODES, REPORT_FORMAT, SCAN_MODE, SEVERITY, TASK_STATUS, reportFileExt, zh } from '../dict';
import { MONO_FONT, SEVERITY_COLOR, STAGE_DOT_COLOR, STATUS_COLOR } from '../dict/tokens';
import { VERDICT_STATE_COLOR, VERDICT_STATE_LABEL, hasConclusion, needsReview, stateFromLegacyVerdict, verdictState } from '../dict/verdict';

describe('枚举字典', () => {
  it('判定两轴组合五态全覆盖（2026-10-09 重构：七值压平枚举 → direction×tier）', () => {
    expect(Object.keys(VERDICT_STATE_LABEL)).toEqual(['TRUE_DEFINITE', 'FALSE_DEFINITE', 'TRUE_LIKELY', 'FALSE_LIKELY', 'UNDETERMINED']);
    expect(VERDICT_STATE_LABEL.TRUE_DEFINITE).toBe('确认为真');
    expect(VERDICT_STATE_LABEL.FALSE_DEFINITE).toBe('误报');
    expect(VERDICT_STATE_LABEL.TRUE_LIKELY).toBe('可能为真');
    expect(VERDICT_STATE_LABEL.FALSE_LIKELY).toBe('可能误报');
    expect(VERDICT_STATE_LABEL.UNDETERMINED).toBe('未判定');
    // 色义承旧：真=红系、误报=灰、可能=橙（红绿反转修正的历史不回退）
    expect(VERDICT_STATE_COLOR.TRUE_DEFINITE).not.toBe('green');
    expect(VERDICT_STATE_COLOR.FALSE_DEFINITE).not.toBe('red');
  });
  it('判定谓词：hasConclusion/needsReview 与引擎分类器同义', () => {
    expect(hasConclusion('VERDICT_DIRECTION_TRUE')).toBe(true);
    expect(hasConclusion('VERDICT_DIRECTION_UNSPECIFIED')).toBe(false);
    expect(hasConclusion(undefined)).toBe(false);
    expect(needsReview({ verdict_direction: 'VERDICT_DIRECTION_UNSPECIFIED', verdict_tier: '' })).toBe(true);
    expect(needsReview({ verdict_direction: 'VERDICT_DIRECTION_TRUE', verdict_tier: 'VERDICT_TIER_LIKELY' })).toBe(true);
    expect(needsReview({ verdict_direction: 'VERDICT_DIRECTION_TRUE', verdict_tier: 'VERDICT_TIER_DEFINITE' })).toBe(false);
    expect(needsReview({ verdict_direction: 'VERDICT_DIRECTION_FALSE', verdict_tier: 'VERDICT_TIER_DEFINITE' })).toBe(false);
    // 脏数据防御：方向未判定而档位有值 → 档位归零、未判定态
    expect(verdictState({ verdict_direction: '', verdict_tier: 'VERDICT_TIER_DEFINITE' })).toBe('UNDETERMINED');
  });
  it('legacy 七值映射（存量报告 JSON 兼容）：CONFIRMED 别名归确认为真', () => {
    expect(stateFromLegacyVerdict('AI_VERDICT_TRUE_POSITIVE')).toBe('TRUE_DEFINITE');
    expect(stateFromLegacyVerdict('AI_VERDICT_CONFIRMED')).toBe('TRUE_DEFINITE');
    expect(stateFromLegacyVerdict('AI_VERDICT_LIKELY_TRUE')).toBe('TRUE_LIKELY');
    expect(stateFromLegacyVerdict('AI_VERDICT_FALSE_POSITIVE')).toBe('FALSE_DEFINITE');
    expect(stateFromLegacyVerdict('AI_VERDICT_LIKELY_FALSE')).toBe('FALSE_LIKELY');
    expect(stateFromLegacyVerdict('AI_VERDICT_NEEDS_MANUAL')).toBe('UNDETERMINED');
    expect(stateFromLegacyVerdict('AI_VERDICT_UNCERTAIN')).toBe('UNDETERMINED');
    expect(stateFromLegacyVerdict('')).toBe('UNDETERMINED');
  });
  it('TaskStatus 覆盖 04 §1 全部状态（含 CREATED=8/TIMEOUT=7/DEAD=9）', () => {
    for (const k of ['TASK_STATUS_CREATED', 'TASK_STATUS_PENDING', 'TASK_STATUS_QUEUED', 'TASK_STATUS_RUNNING',
      'TASK_STATUS_COMPLETED', 'TASK_STATUS_FAILED', 'TASK_STATUS_CANCELLED', 'TASK_STATUS_TIMEOUT', 'TASK_STATUS_DEAD']) {
      expect(TASK_STATUS[k]).toBeTruthy();
    }
  });
  it('B5: SCAN_MODE/REVIEW_DEPTH 补 UNSPECIFIED 键（零值显式下发不裸显英文枚举）', () => {
    expect(SCAN_MODE['SCAN_MODE_UNSPECIFIED']).toBe('未指定');
    expect(zh(SCAN_MODE, 'SCAN_MODE_UNSPECIFIED')).toBe('未指定');
    expect(REVIEW_DEPTH['REVIEW_DEPTH_UNSPECIFIED']).toBe('未指定');
  });
  it('ScanMode ADR-186 五模式 + 两弃用项（展示序：A/B/C/D/E 在前，弃用置尾）', () => {
    const keys = Object.keys(SCAN_MODE);
    // B5: 首键为 SCAN_MODE_UNSPECIFIED（零值显式下发），展示序随后 A/B/C/D/E
    expect(keys.slice(1, 6)).toEqual(['SCAN_MODE_SAST_ONLY', 'SCAN_MODE_AI_ONLY', 'SCAN_MODE_PARALLEL', 'SCAN_MODE_AI_ENHANCED_SAST', 'SCAN_MODE_COMPARE']);
    expect(keys).toHaveLength(8);
    expect(DEPRECATED_SCAN_MODES.has('SCAN_MODE_TRADITIONAL_FIRST')).toBe(true);
    expect(DEPRECATED_SCAN_MODES.has('SCAN_MODE_SAST_REVIEW')).toBe(true);
  });
  it('zh 未知键回退显示原键（不隐藏数据，P4）', () => {
    expect(zh(TASK_STATUS, 'TASK_STATUS_FUTURE')).toBe('TASK_STATUS_FUTURE');
    expect(zh(SEVERITY, 'SEVERITY_HIGH')).toBe('高危');
  });
  it('zh 空值走通用回退，无枚举特判（B3-5）', () => {
    expect(zh(TASK_STATUS, undefined)).toBe('未知');
    expect(zh(TASK_STATUS, 'TASK_STATUS_FUTURE')).toBe('TASK_STATUS_FUTURE');
  });
  it('B5-P1-1 ReportFormat 枚举名键控（真实网关形状）+ reportFileExt（未知/UNSPECIFIED 兜底 json）', () => {
    for (const k of ['REPORT_FORMAT_PDF', 'REPORT_FORMAT_HTML', 'REPORT_FORMAT_JSON', 'REPORT_FORMAT_CSV']) {
      expect(REPORT_FORMAT[k]).toBeTruthy();
    }
    expect(reportFileExt('REPORT_FORMAT_JSON')).toBe('json');
    expect(reportFileExt('REPORT_FORMAT_HTML')).toBe('html');
    expect(reportFileExt('REPORT_FORMAT_PDF')).toBe('pdf');
    expect(reportFileExt('REPORT_FORMAT_CSV')).toBe('csv');
    expect(reportFileExt('REPORT_FORMAT_UNSPECIFIED')).toBe('json');
    expect(reportFileExt(undefined)).toBe('json');
  });
});

// 设计整改 P0：语义配色 token 锁——配色键域与 dict 枚举同名且不缺级。
// 此前 SEVERITY_COLOR 只覆盖 4 级（INFO 无色）、critical=volcano 弱于 high=red（倒置）。
describe('语义配色 token（dict/tokens.ts）', () => {
  it('severity 色阶覆盖全部五级实体（UNSPECIFIED 有意无色），强度序绛红>警红>琥珀>秋黄', () => {
    const levels = ['SEVERITY_CRITICAL', 'SEVERITY_HIGH', 'SEVERITY_MEDIUM', 'SEVERITY_LOW', 'SEVERITY_INFO'];
    for (const k of levels) expect(SEVERITY_COLOR[k]).toBeTruthy();
    expect(Object.keys(SEVERITY_COLOR)).toHaveLength(levels.length);
  });
  it('任务状态覆盖非 default 的全部语义状态（RUNNING/COMPLETED/FAILED/DEAD/TIMEOUT/PAUSED）', () => {
    for (const k of ['TASK_STATUS_RUNNING', 'TASK_STATUS_COMPLETED', 'TASK_STATUS_FAILED',
      'TASK_STATUS_DEAD', 'TASK_STATUS_TIMEOUT', 'TASK_STATUS_PAUSED']) {
      expect(STATUS_COLOR[k]).toBeTruthy();
    }
  });
  it('阶段圆点三态（完成/执行中/失败）', () => {
    expect(STAGE_DOT_COLOR.STAGE_STATUS_COMPLETED).toBeTruthy();
    expect(STAGE_DOT_COLOR.STAGE_STATUS_RUNNING).toBeTruthy();
    expect(STAGE_DOT_COLOR.STAGE_STATUS_FAILED).toBeTruthy();
  });
  it('证据等宽字体栈以 monospace 收尾（系统栈兜底，不引 webfont）', () => {
    expect(MONO_FONT.endsWith('monospace')).toBe(true);
    expect(MONO_FONT).not.toContain('url(');
  });
});
