// T3 回归：详情页 triage 提交体 + reasoning 展示（P4 原文呈现）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
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
    expect(screen.getByText('该建议为“需人工处置”声明，非自动生成方案')).toBeTruthy();
  });
  it('提交裁决 → PUT 携带所选枚举与 reasoning 输入', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('提交裁决')).toBeTruthy());
    fireEvent.change(screen.getByPlaceholderText('裁决理由（写入 finding.reasoning，proto L1240）'), {
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
  // 会话#42：写入方按实际数据推断标注（reasoning 仅人工链路写入 → 有理由=人工）
  it('写入方推断：有理由 → 标注人工并展示"人工裁决理由（原文）"', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('写入方：人工')).toBeTruthy());
    expect(screen.getByText('人工裁决理由（原文）')).toBeTruthy();
  });
  it('写入方推断：有结论无理由 → 如实标注 V1 契约无法区分', async () => {
    detailPayload = {
      finding_id: 'f-9', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B608',
      cwe_id: 'CWE-89', title: 't', description: '', severity: 'SEVERITY_HIGH', confidence: 0.8,
      ai_verdict: 'AI_VERDICT_TRUE_POSITIVE', ai_confidence: 0, ai_reasoning: '',
      ai_fix_suggestion: '',
      location: { file_path: 'db.py', start_line: 88 },
    };
    renderPage();
    await waitFor(() => expect(screen.getByText(/V1 契约无法区分/)).toBeTruthy());
    expect(screen.getAllByText(/确认为真/).length).toBeGreaterThan(0);
  });
});
