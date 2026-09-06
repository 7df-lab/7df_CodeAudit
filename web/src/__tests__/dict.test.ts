// 字典完整性回归（14号 P2/P4：值域=proto 枚举，不自造）
import { AI_VERDICT, DEPRECATED_SCAN_MODES, REPORT_FORMAT, SCAN_MODE, SEVERITY, TASK_STATUS, reportFileExt, zh } from '../dict';

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
  it('ScanMode ADR-186 五模式 + 两弃用项（展示序：A/B/C/D/E 在前，弃用置尾）', () => {
    const keys = Object.keys(SCAN_MODE);
    expect(keys.slice(0, 5)).toEqual(['SCAN_MODE_SAST_ONLY', 'SCAN_MODE_AI_ONLY', 'SCAN_MODE_PARALLEL', 'SCAN_MODE_AI_ENHANCED_SAST', 'SCAN_MODE_COMPARE']);
    expect(keys).toHaveLength(7);
    expect(DEPRECATED_SCAN_MODES.has('SCAN_MODE_TRADITIONAL_FIRST')).toBe(true);
    expect(DEPRECATED_SCAN_MODES.has('SCAN_MODE_SAST_REVIEW')).toBe(true);
  });
  it('zh 未知键回退显示原键（不隐藏数据，P4）', () => {
    expect(zh(TASK_STATUS, 'TASK_STATUS_FUTURE')).toBe('TASK_STATUS_FUTURE');
    expect(zh(SEVERITY, 'SEVERITY_HIGH')).toBe('高危');
  });
  it('ReportFormat 四值映射 + reportFileExt（下载扩展名，未知/0 兜底 json——编排器缺省产 JSON）', () => {
    for (const k of [1, 2, 3, 4]) expect(REPORT_FORMAT[k]).toBeTruthy();
    expect(reportFileExt(3)).toBe('json');
    expect(reportFileExt(2)).toBe('html');
    expect(reportFileExt(1)).toBe('pdf');
    expect(reportFileExt(4)).toBe('csv');
    expect(reportFileExt(0)).toBe('json');
    expect(reportFileExt(undefined)).toBe('json');
  });
});
