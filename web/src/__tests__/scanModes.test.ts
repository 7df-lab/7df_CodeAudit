// scanModes 单元回归：任务 payload 组装与默认模式解析的
// 单一事实源契约——B5-P1-3 拷贝漂移事故（三处同构逻辑漏模式D）的根治锚。
import { describe, expect, it } from 'vitest';
import { buildScanTaskPayload, DEFAULT_SAST_TOOLS, DEFAULT_SCAN_MODE, GRAPH_ORCH, MODE_SPECS, resolveDefaultScanMode } from '../tasks/scanModes';

describe('buildScanTaskPayload（三处创建入口共用的组装单一事实源）', () => {
  it('needsSastTools 模式：显式工具透传（向导参数步路径）', () => {
    expect(buildScanTaskPayload({ project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: ['opengrep', 'bandit'] }))
      .toMatchObject({ project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: ['opengrep', 'bandit'] });
  });

  it('needsSastTools 模式无显式工具 → 部署默认（项目页自动链/快捷弹窗路径）', () => {
    const body = buildScanTaskPayload({ project_id: 'p1', scan_mode: 'SCAN_MODE_AI_ENHANCED_SAST' });
    // B5-P1-3 回归锁同源：模式D 需要工具——漏带即任务必 FAILED
    expect(body.sast_tools).toEqual(DEFAULT_SAST_TOOLS);
    expect(body.sast_tools).toEqual(['opengrep']);
  });

  it('无工具模式（B 纯AI）→ sast_tools:[]（不得带默认工具）', () => {
    expect(buildScanTaskPayload({ project_id: 'p1', scan_mode: 'SCAN_MODE_AI_ONLY', sast_tools: ['opengrep'] }).sast_tools)
      .toEqual([]);
  });

  it('图编排：config.pack 钉入 + scan_mode 载体 AI_ONLY + 工具面恒空（GraphSpec 短路）', () => {
    const body = buildScanTaskPayload({ project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', pack: GRAPH_ORCH, sast_tools: ['opengrep'] });
    expect(body.scan_mode).toBe('SCAN_MODE_AI_ONLY');
    expect(body.config).toEqual({ pack: 'audit-graph' });
    expect(body.sast_tools).toEqual([]);
  });

  // 审查修复批：深链接受任意非空编排名（编排 CRUD 批）——自定义编排
  // 不是 GraphSpec，此前 !!pack 一刀切静默改写 AI_ONLY/空工具=所见非所发的行为回归锁
  it('自定义编排：仅 config.pack 随行，模式/工具保持用户所选（不静默改 AI_ONLY）', () => {
    const body = buildScanTaskPayload({ project_id: 'p1', scan_mode: 'SCAN_MODE_AI_ENHANCED_SAST', pack: 'audit-sample', sast_tools: ['bandit'] });
    expect(body.scan_mode).toBe('SCAN_MODE_AI_ENHANCED_SAST');
    expect(body.sast_tools).toEqual(['bandit']);
    expect(body.config).toEqual({ pack: 'audit-sample' });
  });

  it('标准编排：不发 pack 键（旧后端默认行为零回归）；审核键随行', () => {
    const body = buildScanTaskPayload({
      project_id: 'p1', scan_mode: 'SCAN_MODE_SAST_REVIEW',
      config: { review_depth: 'REVIEW_DEPTH_STANDARD' },
    });
    expect(body.config).toEqual({ review_depth: 'REVIEW_DEPTH_STANDARD' });
    expect('pack' in body.config).toBe(false);
  });

  it('agent 空=部署默认不发字段（旧后端兼容）；非空透传', () => {
    expect('agent' in buildScanTaskPayload({ project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL' })).toBe(false);
    expect(buildScanTaskPayload({ project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', agent: 'maka' }).agent).toBe('maka');
  });
});

describe('resolveDefaultScanMode（P0-2：项目 default_scan_mode 兑现为任务入口默认模式）', () => {
  it('合法模式跟随项目', () => {
    expect(resolveDefaultScanMode({ default_scan_mode: 'SCAN_MODE_SAST_ONLY' })).toBe('SCAN_MODE_SAST_ONLY');
  });
  it('空值/缺字段 → 回落默认推荐模式C（存量项目 default_scan_mode 可为空串）', () => {
    expect(resolveDefaultScanMode({ default_scan_mode: '' })).toBe(DEFAULT_SCAN_MODE);
    expect(resolveDefaultScanMode(undefined)).toBe(DEFAULT_SCAN_MODE);
  });
  it('UNSPECIFIED/弃用模式不进创建面（不把死枚举送进 payload）', () => {
    expect(resolveDefaultScanMode({ default_scan_mode: 'SCAN_MODE_UNSPECIFIED' })).toBe(DEFAULT_SCAN_MODE);
    expect(resolveDefaultScanMode({ default_scan_mode: 'SCAN_MODE_TRADITIONAL_FIRST' })).toBe(DEFAULT_SCAN_MODE);
    expect(MODE_SPECS.SCAN_MODE_TRADITIONAL_FIRST.deprecated).toBe(true);
  });
});
