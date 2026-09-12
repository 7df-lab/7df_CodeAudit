// T3 回归：详情页 triage 提交体 + reasoning 展示（P4 原文呈现）
import { QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import FindingDetailPage from '../pages/findings/FindingDetailPage';
import { useFakeGateway } from '../testsupport/fakeGateway';

// ADR-203 fakeGateway：真实 api/client 执行，仅 HTTP 层伪造；未建模路由响亮失败
let detailPayload: unknown;
const gateway = useFakeGateway({
  'GET /v1/findings/:findingId': () => ({ finding: detailPayload }),
  'PUT /v1/findings/:findingId/verdict': () => ({}),
});
function setDefaultDetail() {
  detailPayload = {
    finding_id: 'f-9', task_id: 't1', source_tool: 'ai_agent', source_rule_id: '',
    cwe_id: 'CWE-89', title: 'SQL injection path', description: 'desc',
    severity: 'SEVERITY_CRITICAL', confidence: 0.8,
    ai_verdict: 'AI_VERDICT_NEEDS_MANUAL', ai_confidence: 0,
    ai_reasoning: 'quality-validator: LLM cross-validation unavailable — manual review required',
    ai_fix_suggestion: 'MANUAL_REVIEW_REQUIRED: parameterize query',
    location: { file_path: 'db.py', start_line: 88 },
  };
}
setDefaultDetail();

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/findings/f-9']}>
        <FindingDetailPage findingId="f-9" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('FindingDetailPage（T3 triage 工作台）', () => {
  it('AI reasoning 与“需人工处置”建议原文呈现（P4/P3）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('SQL injection path')).toBeTruthy());
    expect(screen.getByText('quality-validator: LLM cross-validation unavailable — manual review required')).toBeTruthy();
    expect(screen.getByText('该条目为“需人工处置”标记，非自动生成的修复方案')).toBeTruthy();
  });
  it('提交裁决 → PUT 携带所选枚举与 reasoning 输入', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('提交裁决')).toBeTruthy());
    fireEvent.change(screen.getByPlaceholderText('填写裁决理由（可选，将随结论一并保存）'), {
      target: { value: 'verified by human' },
    });
    fireEvent.click(screen.getByText('提交裁决'));
    const put = await waitFor(() => {
      const r = gateway.requests.find((x) => x.method === 'PUT');
      expect(r).toBeTruthy();
      return r!;
    });
    expect(put.url).toBe('/v1/findings/f-9/verdict');
    expect(put.body).toEqual({ verdict: 'AI_VERDICT_TRUE_POSITIVE', reasoning: 'verified by human' });
  });
  // 会话#42 + 2026-09-09 三分法：写入方按 reasoning 前缀推断（AI/系统降级/人工）
  it('写入方推断：有理由且非机器前缀 → 标注人工并展示"裁决理由"', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('写入方：人工')).toBeTruthy());
    expect(screen.getByText('裁决理由')).toBeTruthy();
  });
  it('写入方推断：有结论无理由 → 如实标注"来源未记录"', async () => {
    detailPayload = {
      finding_id: 'f-9', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B608',
      cwe_id: 'CWE-89', title: 't', description: '', severity: 'SEVERITY_HIGH', confidence: 0.8,
      ai_verdict: 'AI_VERDICT_TRUE_POSITIVE', ai_confidence: 0, ai_reasoning: '',
      ai_fix_suggestion: '',
      location: { file_path: 'db.py', start_line: 88 },
    };
    renderPage();
    await waitFor(() => expect(screen.getByText('写入方：未记录')).toBeTruthy());
    expect(screen.getAllByText(/确认为真/).length).toBeGreaterThan(0);
  });
  // 2026-09-09 用户报障：[降级] 前缀的机器写入（规则兜底）此前被误标"写入方：人工"
  it('写入方推断：[降级] 前缀 → 标注"系统（自动降级标记）"并原文展示', async () => {
    detailPayload = {
      finding_id: 'f-9', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B608',
      cwe_id: 'CWE-89', title: 't', description: '', severity: 'SEVERITY_HIGH', confidence: 0.8,
      ai_verdict: 'AI_VERDICT_NEEDS_MANUAL', ai_confidence: 0,
      ai_reasoning: '[降级] 规则引擎兜底产出，未经 AI 语义审查，需人工复核',
      ai_fix_suggestion: '',
      location: { file_path: 'db.py', start_line: 88 },
    };
    renderPage();
    await waitFor(() => expect(screen.getByText('写入方：系统（自动降级标记）')).toBeTruthy());
    expect(screen.getByText('系统自动标记')).toBeTruthy();
    expect(screen.getByText(/\[降级\] 规则引擎兜底产出/)).toBeTruthy();
  });
});

// B4-3（审计修复）：提交裁决成功联动失效 fusion-findings / review-findings 前缀——
// 本组件内嵌于 ReviewView 行展开（I-A0），任务详情 Tabs 的融合/审核视图也读 ai_verdict，
// 不联动则裁决后返回这些视图停留旧结论。探针 queryKey 与真实视图一致，queryFn 计数。
describe('FindingDetailPage 裁决缓存联动（B4-3）', () => {
  it('提交裁决成功 → fusion-findings / review-findings 前缀失效并重拉', async () => {
    setDefaultDetail();
    let fusionFetches = 0;
    let reviewFetches = 0;
    function Probe() {
      useQuery({ queryKey: ['fusion-findings', 't1'], queryFn: async () => { fusionFetches += 1; return null; } });
      useQuery({ queryKey: ['review-findings', 't1'], queryFn: async () => { reviewFetches += 1; return null; } });
      return null;
    }
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={['/findings/f-9']}>
          <Probe />
          <FindingDetailPage findingId="f-9" />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    await waitFor(() => expect(fusionFetches).toBe(1)); // 探针初拉
    await waitFor(() => expect(reviewFetches).toBe(1));
    await waitFor(() => expect(screen.getByText('提交裁决')).toBeTruthy());
    fireEvent.click(screen.getByText('提交裁决'));
    await waitFor(() => {
      const put = gateway.requests.find((x) => x.method === 'PUT');
      expect(put).toBeTruthy();
    });
    await waitFor(() => expect(fusionFetches).toBe(2)); // 前缀失效 → 重拉
    await waitFor(() => expect(reviewFetches).toBe(2));
  });
});
