// T4 回归：四视图字段全溯源 + 诚实降级声明（14号 P2/P3/P4）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import FusionView from '../pages/views/FusionView';
import ComparisonView from '../pages/views/ComparisonView';
import ReviewView from '../pages/views/ReviewView';

const getMock = vi.fn(async (url: string) => {
  if (url === '/v1/findings') {
    return { data: { findings: [
      { finding_id: 'f-s1', task_id: 't1', source_tool: 'semgrep', source_rule_id: 'R1', cwe_id: 'CWE-89',
        title: 'sast primary', severity: 'SEVERITY_HIGH', confidence: 0.8, ai_verdict: 'AI_VERDICT_UNSPECIFIED',
        ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '', location: null,
        dedup_group: 'group_1', matched_findings: ['f-a1'], is_unique: false },
      { finding_id: 'f-a1', task_id: 't1', source_tool: 'ai_agent', source_rule_id: '', cwe_id: 'CWE-89',
        title: 'ai dup', severity: 'SEVERITY_HIGH', confidence: 0.9, ai_verdict: 'AI_VERDICT_UNSPECIFIED',
        ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '', location: null,
        dedup_group: 'group_1', matched_findings: ['f-s1'], is_unique: false },
      { finding_id: 'f-u1', task_id: 't1', source_tool: 'ai_agent', source_rule_id: '', cwe_id: 'CWE-798',
        title: 'ai unique', severity: 'SEVERITY_CRITICAL', confidence: 0.9, ai_verdict: 'AI_VERDICT_UNSPECIFIED',
        ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '', location: null,
        dedup_group: '', matched_findings: [], is_unique: true },
    ] } };
  }
  if (url === '/v1/tasks/t1/comparison-report') {
    return { data: { report_id: 'cmp-t1', venn_data_url: '', summary: {
      sast_total: 5, ai_total: 3, both_found: 2, sast_only: 3, ai_only: 1, disagreement: 1,
      metrics: { total_unique: 6, sast_precision: 0.4, sast_recall: 2 / 3, sast_f1: 0.5,
        ai_precision: 2 / 3, ai_recall: 0.4, ai_f1: 0.5 } } } };
  }
  throw new Error('unexpected ' + url);
});

vi.mock('../api/client', () => ({ api: { get: (...args: unknown[]) => getMock(...(args as [string])) } }));

function withProviders(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('FusionView（模式B，proto L84-86 字段消费）', () => {
  it('dedup_group 组视图：primary=SAST、组成员 chips、is_unique 单列', async () => {
    withProviders(<FusionView taskId="t1" />);
    await waitFor(() => expect(screen.getByText(/合并组 group_1/)).toBeTruthy());
    expect(screen.getByText(/primary:/)).toBeTruthy();
    expect(screen.getByText(/ai dup/)).toBeTruthy();
    expect(screen.getAllByText(/未合并发现/).length).toBeGreaterThan(0);
    expect(screen.getByText(/ai unique/)).toBeTruthy();
  });
});

describe('ComparisonView（模式C，后端计算直显 P4）', () => {
  it('四象限与七指标渲染 + ADR-133 口径固定脚注', async () => {
    withProviders(<ComparisonView taskId="t1" />);
    await waitFor(() => expect(screen.getByText('对比视图（模式C 四象限）')).toBeTruthy());
    expect(screen.getByText('结论分歧').nextSibling?.textContent).toBe('1');
    expect(screen.getByText(/指标口径披露/)).toBeTruthy();
    expect(screen.getByText(/非 DiverseVul 全量基准 F1/)).toBeTruthy();
  });
});

describe('ReviewView（模式D，诚实降级）', () => {
  it('显式声明审核报告未持久化（ADR-139），仅展示已落盘结论', async () => {
    withProviders(<ReviewView taskId="t1" />);
    await waitFor(() => expect(screen.getByText(/审核报告（整体评估\+逐条 opinion）当前未持久化/)).toBeTruthy());
    expect(screen.getByText(/sast primary/)).toBeTruthy();
  });
});
