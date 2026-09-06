// 字典完整性回归（14号 P2/P4：值域=proto 枚举，不自造）
import { AI_VERDICT, SCAN_MODE, SEVERITY, TASK_STATUS, zh } from '../dict';

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
  it('ScanMode 四模式', () => {
    expect(Object.keys(SCAN_MODE)).toHaveLength(4);
  });
  it('zh 未知键回退显示原键（不隐藏数据，P4）', () => {
    expect(zh(TASK_STATUS, 'TASK_STATUS_FUTURE')).toBe('TASK_STATUS_FUTURE');
    expect(zh(SEVERITY, 'SEVERITY_HIGH')).toBe('高危');
  });
});
