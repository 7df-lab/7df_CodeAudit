// 字典完整性回归（14号 P2/P4：值域=proto 枚举，不自造）
import { AI_VERDICT, REVIEW_DEPTH, DEPRECATED_SCAN_MODES, REPORT_FORMAT, SCAN_MODE, SEVERITY, TASK_STATUS, reportFileExt, zh } from '../dict';
import { MONO_FONT, SEVERITY_COLOR, STAGE_DOT_COLOR, STATUS_COLOR, VERDICT_COLOR } from '../dict/tokens';

describe('枚举字典', () => {
  it('AIVerdict 覆盖 proto 六值 + UNSPECIFIED', () => {
    for (const k of ['AI_VERDICT_UNSPECIFIED', 'AI_VERDICT_TRUE_POSITIVE', 'AI_VERDICT_LIKELY_TRUE',
      'AI_VERDICT_FALSE_POSITIVE', 'AI_VERDICT_LIKELY_FALSE', 'AI_VERDICT_NEEDS_MANUAL', 'AI_VERDICT_UNCERTAIN']) {
      expect(AI_VERDICT[k]).toBeTruthy();
    }
    expect(Object.keys(AI_VERDICT)).toHaveLength(7);
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
    // 首键为 SCAN_MODE_UNSPECIFIED（零值显式下发），展示序随后 A/B/C/D/E
    expect(keys.slice(1, 6)).toEqual(['SCAN_MODE_SAST_ONLY', 'SCAN_MODE_AI_ONLY', 'SCAN_MODE_PARALLEL', 'SCAN_MODE_AI_ENHANCED_SAST', 'SCAN_MODE_COMPARE']);
    expect(keys).toHaveLength(8);
    expect(DEPRECATED_SCAN_MODES.has('SCAN_MODE_TRADITIONAL_FIRST')).toBe(true);
    expect(DEPRECATED_SCAN_MODES.has('SCAN_MODE_SAST_REVIEW')).toBe(true);
  });
  it('zh 未知键回退显示原键（不隐藏数据，P4）', () => {
    expect(zh(TASK_STATUS, 'TASK_STATUS_FUTURE')).toBe('TASK_STATUS_FUTURE');
    expect(zh(SEVERITY, 'SEVERITY_HIGH')).toBe('高危');
  });
  it('zh 空值走通用回退，无枚举特判；"未判定"由调用点显式归一后走查表路径', () => {
    expect(zh(AI_VERDICT, undefined)).toBe('未知');
    expect(zh(AI_VERDICT, '')).toBe('未知');
    expect(zh(AI_VERDICT, 'AI_VERDICT_UNSPECIFIED')).toBe('未判定'); // 正常键查表，非回退特判
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

// （2026-09-13）：语义配色 token 锁——配色键域与 dict 枚举同名且不缺级。
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
  it('verdict 覆盖除 UNSPECIFIED 外全部六值；真漏洞=红系、误报=灰（不再红绿倒置）', () => {
    for (const k of ['AI_VERDICT_TRUE_POSITIVE', 'AI_VERDICT_LIKELY_TRUE', 'AI_VERDICT_NEEDS_MANUAL',
      'AI_VERDICT_FALSE_POSITIVE', 'AI_VERDICT_LIKELY_FALSE', 'AI_VERDICT_UNCERTAIN']) {
      expect(VERDICT_COLOR[k]).toBeTruthy();
    }
    expect(VERDICT_COLOR.AI_VERDICT_TRUE_POSITIVE).not.toBe('green');
    expect(VERDICT_COLOR.AI_VERDICT_FALSE_POSITIVE).not.toBe('red');
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
