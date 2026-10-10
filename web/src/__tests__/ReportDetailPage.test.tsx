// 报告阅读视图：解析器形状防御直测 +
// 页面渲染（审计句/光谱带/阅读卡/代码证据）+ 光谱过滤 + 原始 JSON + HTML/空档降级 +
// 列表页"在线查看"分流导航。契约锚：E-29①（getReportContent）。
// 网关台纪律：useFakeGateway 必须模块顶层注册（beforeEach 挂载序），可变载荷经
// 闭包变量按用例置值——不在测试体内二次注册钩子。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import dayjs from 'dayjs';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import ReportDetailPage from '../pages/reports/ReportDetailPage';
import ReportsPage from '../pages/reports/ReportsPage';
import { parseReportContent, severityBucket } from '../pages/reports/reportContent';
import { useFakeGateway, type RouteValue } from '../testsupport/fakeGateway';

// source_raw=明文 JSON 字符串（与发现域 base64 口径不同）——报告正文整体 JSON.stringify
// 后即为单层转义，夹具直接持字符串本体，勿再套一层 stringify（首轮就是双层编码假红）
const RAW_CTX = JSON.stringify({ code: 'x = 1', line: 5, context: { start_line: 4, end_line: 6, lines: ['def a():', '    x = 1', ''] } });
const FINDING = (over: Record<string, unknown>) => ({
  finding_id: 'f-1', title: '标题', severity: 'SEVERITY_HIGH', verdict: 'AI_VERDICT_LIKELY_TRUE',
  file: 'app.py', line: 5, cwe: '', rule_id: 'r-1',
  source_raw: RAW_CTX,
  ...over,
});
const DOC = {
  task_id: 't-1', generated_at: '2026-10-07T17:42:48Z', template: '',
  summary: { total_findings: 2, true_positives: 1, false_positives: 0, not_reviewed: 1 },
  findings: [FINDING({}), FINDING({ finding_id: 'f-2', severity: 'SEVERITY_LOW', verdict: 'AI_VERDICT_FALSE_POSITIVE', source_raw: '' })],
};

// 可变载荷：报告正文 / 列表行（格式枚举）——按用例置值
let downloadContent: string = '';
let listFormat: string = 'REPORT_FORMAT_JSON';
const routes: Record<string, RouteValue> = {
  'GET /v1/reports/:id/download': () => downloadContent,
  'GET /v1/reports': () => ({
    reports: [{ report_id: 'r-nav', task_id: 't-1', format: listFormat, url: '', generated_at: '2026-09-01T00:00:00Z' }],
    pagination: { next_cursor: '', has_next: false, total: 1 },
  }),
  'GET /v1/tasks': { tasks: [{ task_id: 't-1', project_id: 'p1' }], pagination: { next_cursor: '', has_next: false, total: 1 } },
  'GET /v1/projects': { projects: [{ project_id: 'p1', name: '演示项目' }], pagination: { next_cursor: '', has_next: false, total: 1 } },
};
useFakeGateway(routes);

beforeEach(() => {
  downloadContent = '';
  listFormat = 'REPORT_FORMAT_JSON';
});

function renderAt(path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/reports/:id" element={<ReportDetailPage />} />
          <Route path="/reports" element={<ReportsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('reportContent 解析器（形状防御）', () => {
  it('JSON 报告 → doc（summary/findings/source_raw 明文 JSON 证据）', () => {
    const p = parseReportContent(JSON.stringify(DOC));
    expect(p.kind).toBe('json');
    if (p.kind !== 'json') return;
    expect(p.doc.summary.truePositives).toBe(1);
    expect(p.doc.findings).toHaveLength(2);
    expect(p.doc.findings[0].evidence?.context?.startLine).toBe(4);
    expect(p.doc.findings[0].evidence?.matchedLine).toBe(5);
    expect(p.doc.findings[1].evidence).toBeUndefined(); // 空 source_raw → 无证据（如实降级）
  });
  it('老档 code 带行号前缀无 context → 原样文本证据，不臆造行号', () => {
    const p = parseReportContent(JSON.stringify({
      findings: [FINDING({ source_raw: JSON.stringify({ code: '5     x = 1' }) })], summary: {},
    }));
    if (p.kind !== 'json') throw new Error('kind');
    expect(p.doc.findings[0].evidence?.context).toBeUndefined();
    expect(p.doc.findings[0].evidence?.text).toContain('5     x = 1');
  });
  it('嗅探分型：空→empty；<开头→html；解析失败/非报告形状→text', () => {
    expect(parseReportContent('')).toEqual({ kind: 'empty' });
    expect(parseReportContent('   \n ')).toEqual({ kind: 'empty' });
    expect(parseReportContent('<!doctype html><p>x</p>').kind).toBe('html');
    expect(parseReportContent('a,b,c\n1,2,3').kind).toBe('text');
    expect(parseReportContent('{"msg":"no findings here"}').kind).toBe('text');
  });
  it('无 finding_id 的条目整条不采；未知 severity 原样保留、归未知桶', () => {
    const p = parseReportContent(JSON.stringify({
      findings: [FINDING({ finding_id: '' }), FINDING({ severity: 'SEVERITY_WAT' })], summary: {},
    }));
    if (p.kind !== 'json') throw new Error('kind');
    expect(p.doc.findings).toHaveLength(1);
    expect(p.doc.findings[0].severity).toBe('SEVERITY_WAT');
    expect(severityBucket('SEVERITY_WAT')).toBe('SEVERITY_UNSPECIFIED');
  });
});

describe('ReportDetailPage JSON 阅读视图', () => {
  it('报告头/审计句/光谱带/阅读卡/首卡默认展开代码证据', async () => {
    downloadContent = JSON.stringify(DOC);
    renderAt('/reports/rpt-x');
    // 报告头：任务链接 + 项目名（两级索引解析）+ 生成时间 + 总数
    const taskLink = await screen.findByText('t-1');
    expect(taskLink.closest('a')?.getAttribute('href')).toBe('/tasks/t-1');
    await waitFor(() => expect(screen.getByText('演示项目').closest('a')?.getAttribute('href')).toBe('/projects/p1'));
    expect(document.body.textContent).toContain(`生成于 ${dayjs('2026-10-07T17:42:48Z').format('YYYY-MM-DD')}`); // 本地时区口径（与列表页一致，勿硬编码 UTC 日期）
    expect(document.body.textContent).toContain('共 2 条发现');
    // 审计句（服务端复核口径）
    expect(document.body.textContent).toContain('确认真 1');
    expect(document.body.textContent).toContain('待复核 1');
    // 光谱带图例：阶梯五级计数
    expect(screen.getByText('严重 0')).toBeTruthy();
    expect(screen.getByText('高危 1')).toBeTruthy();
    expect(screen.getByText('低危 1')).toBeTruthy();
    expect(screen.getByText('提示 0')).toBeTruthy();
    // 阅读卡：最危（高危）在前、默认展开证据；verdict 中文标签
    const cards = screen.getAllByTestId(/^finding-card-/);
    expect(cards).toHaveLength(2);
    expect(cards[0].getAttribute('data-testid')).toBe('finding-card-f-1');
    expect(cards[0].textContent).toContain('可能为真');
    const ev = screen.getByTestId('evidence-code');
    expect(ev.textContent).toContain('x = 1');
    // 命中行高亮（jsdom 内联样式断言：matchBg=rgba(255,210,75,0.12)）
    const matched = ev.querySelector('[data-line="5"]') as HTMLElement;
    expect(matched.style.background).toContain('rgba(255, 210, 75');
    // 无证据卡：如实标注（不伪造空面板）
    expect(screen.getByText('该发现未携带代码片段。')).toBeTruthy();
  });

  it('光谱过滤：点高危图例滤出仅高危卡，再点恢复；已滤出计数可见', async () => {
    downloadContent = JSON.stringify(DOC);
    renderAt('/reports/rpt-x');
    await screen.findByTestId('finding-card-f-1');
    fireEvent.click(screen.getByText('高危 1').closest('button')!);
    await waitFor(() => expect(screen.getAllByTestId(/^finding-card-/)).toHaveLength(1));
    expect(document.body.textContent).toContain('已滤出 1/2 条');
    fireEvent.click(screen.getByText('高危 1').closest('button')!);
    await waitFor(() => expect(screen.getAllByTestId(/^finding-card-/)).toHaveLength(2));
  });

  it('收起代码证据 + 原始 JSON 抽屉原文可见', async () => {
    downloadContent = JSON.stringify(DOC);
    renderAt('/reports/rpt-x');
    await screen.findByTestId('finding-card-f-1');
    fireEvent.click(screen.getByText('收起代码证据'));
    await waitFor(() => expect(screen.queryByTestId('evidence-code')).toBeNull());
    // 原始 JSON 抽屉：与下载正文一致（信任链）
    fireEvent.click(screen.getByText('原始 JSON'));
    await waitFor(() => expect(screen.getByTestId('raw-json').textContent).toContain('"total_findings":2')); // 紧凑 JSON 无空格
  });
});

describe('ReportDetailPage 非 JSON 分型（诚实降级）', () => {
  it('HTML 档：不直染成品、原文卡展示、沙箱窗入口', async () => {
    downloadContent = '<!doctype html><html><body><h1>扫描报告</h1></body></html>';
    renderAt('/reports/rpt-h');
    expect(await screen.findByText('该报告是 HTML 渲染档')).toBeTruthy();
    expect(screen.getByText('在新窗口渲染')).toBeTruthy();
    expect(screen.getByTestId('raw-content').textContent).toContain('<h1>扫描报告</h1>');
  });
  it('CSV 档：原文卡直显 + 引导 JSON 重生成', async () => {
    downloadContent = 'id,severity\n1,HIGH';
    renderAt('/reports/rpt-c');
    expect(await screen.findByText('该报告不是结构化 JSON 档')).toBeTruthy();
    expect(screen.getByTestId('raw-content').textContent).toContain('id,severity');
  });
  it('空正文：空态引导回报告中心重新生成', async () => {
    downloadContent = '';
    renderAt('/reports/rpt-e');
    expect(await screen.findByText('该报告没有正文（生成失败或已被清除）')).toBeTruthy();
    expect(screen.getByText('回报告中心重新生成')).toBeTruthy();
  });
});

describe('ReportsPage 在线查看分流（2026-10-08 报告人性化批）', () => {
  it('JSON/UNSPECIFIED/CSV → 站内阅读视图', async () => {
    for (const format of ['REPORT_FORMAT_JSON', 'REPORT_FORMAT_UNSPECIFIED', 'REPORT_FORMAT_CSV']) {
      listFormat = format;
      downloadContent = JSON.stringify(DOC);
      const { unmount } = renderAt('/reports');
      fireEvent.click(await screen.findByText('在线查看'));
      await screen.findByTestId('severity-spectrum');
      unmount();
    }
  });
  it('HTML → 维持沙箱窗弹窗（不导航）', async () => {
    listFormat = 'REPORT_FORMAT_HTML';
    downloadContent = '<!doctype html><p>ok</p>';
    const open = vi.spyOn(window, 'open').mockReturnValue({ closed: false } as Window);
    renderAt('/reports');
    fireEvent.click(await screen.findByText('在线查看'));
    await waitFor(() => expect(open).toHaveBeenCalled());
    expect(screen.queryByTestId('severity-spectrum')).toBeNull();
    open.mockRestore();
  });
});
