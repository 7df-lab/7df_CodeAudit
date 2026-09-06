import * as assert from 'assert';
import { buildViewUpdate, escapeHtml, renderAiContextHtml } from '../src/aiContextView';
import { applyFrame, createProgressState } from '../src/progressModel';
import type { TaskSnapshot } from '../src/types';

const b64 = (s: string): string => Buffer.from(s, 'utf8').toString('base64');

describe('aiContextView：escapeHtml（AI 正文/日志为模型产出，一律转义）', () => {
  it('五个 HTML 元字符全转义', () => {
    assert.strictEqual(escapeHtml(`<a href="x">&'</a>`), '&lt;a href=&quot;x&quot;&gt;&amp;&#39;&lt;/a&gt;');
  });

  it('渲染结果中注入的 <script> 以转义形态出现，绝无活体标签', () => {
    const s = createProgressState('t1');
    applyFrame(s, {
      task: { task_id: 't1', project_id: '', scan_mode: '', sast_tools: [], status: 'TASK_STATUS_RUNNING', stages: [], error_message: '' },
      progress: { task_id: 't1', status: 'TASK_STATUS_RUNNING', overall_percent: 10, stages: [] },
      logs: { logs: [{ log_id: '1', task_id: 't1', ts_ms: '1', level: 'TASK_LOG_LEVEL_INFO', source: 'sandbox', message: '<script>alert(1)</script>' }] },
      ai: { chunk: b64('<img src=x onerror=alert(1)>'), next_cursor: '28', complete: false, total_bytes: '28' },
    });
    const html = renderAiContextHtml({ state: s });
    assert.ok(!/<script>alert/.test(html), '日志注入的 script 必须被转义');
    assert.ok(!/<img src=x/.test(html), 'AI 正文注入的 img 必须被转义');
    assert.ok(html.includes('&lt;script&gt;alert(1)&lt;/script&gt;'));
    assert.ok(html.includes('&lt;img src=x onerror=alert(1)&gt;'));
    // CSP：default-src 'none'，仅内联样式/脚本（滚底）放行
    assert.match(html, /Content-Security-Policy[^]+default-src 'none'/);
  });
});

describe('aiContextView：renderAiContextHtml 结构与状态', () => {
  it('无任务空态：底部面板视图常驻内容（state=null）', () => {
    const html = renderAiContextHtml({ state: null });
    assert.match(html, /暂无进行中或最近的任务/);
    assert.ok(!html.includes('id="aiPane"'), '空态不渲染任务主体分区');
    assert.match(html, /Content-Security-Policy/);
  });

  const mk = (): ReturnType<typeof createProgressState> => {
    const s = createProgressState('gw-abcdef12');
    applyFrame(s, {
      task: { task_id: 'gw-abcdef12', project_id: '', scan_mode: '', sast_tools: [], status: 'TASK_STATUS_RUNNING', stages: [], error_message: '' },
      progress: {
        task_id: 'gw-abcdef12', status: 'TASK_STATUS_RUNNING', overall_percent: 42,
        stages: [
          { stage_id: '1', type: 'STAGE_TYPE_SAST_SCAN', status: 'STAGE_STATUS_COMPLETED', error_message: '', started_at: null, completed_at: null },
          { stage_id: '2', type: 'STAGE_TYPE_AI_INFERENCE', status: 'STAGE_STATUS_RUNNING', error_message: '', started_at: null, completed_at: null },
        ],
      },
      logs: { logs: [{ log_id: '1', task_id: 't', ts_ms: '1756851600000', level: 'TASK_LOG_LEVEL_ERROR', source: 'dsh-agent', message: 'boom' }] },
      ai: { chunk: b64('# 审计会话\n正在分析…'), next_cursor: '30', complete: false, total_bytes: '30' },
    });
    return s;
  };

  it('头部：任务/状态/百分比/连接徽标 + 进度条宽度 + 阶段 chips', () => {
    const html = renderAiContextHtml({ state: mk() });
    assert.match(html, /任务 gw-abcde/);
    assert.match(html, /运行中 · 42%/);
    assert.match(html, /width:42%/);
    assert.match(html, /chip done[^]*SAST 扫描|SAST 扫描[^]*chip done/);
    assert.match(html, /chip running[^]*AI 推理|AI 推理[^]*chip running/);
    assert.match(html, /1\.0 KB · 流式中|30 B · 流式中/);
  });

  it('分区布局：任务日志独立滚动窗在上方，AI 主区在下方；流式增量不整页重载', () => {
    const html = renderAiContextHtml({ state: mk() });
    // 任务日志在上方（DOM 序先于 AI 主区）
    const logsAt = html.indexOf('class="logs-pane"');
    const aiAt = html.indexOf('id="aiPane"');
    assert.ok(logsAt > 0 && aiAt > logsAt, '任务日志窗口应位于 AI 交互主区上方');
    // 日志窗：固定高度 + 自滚动 + 可拖边缘调高度（resize: vertical）
    assert.match(html, /resize: vertical/);
    assert.match(html, /\.logs \{[^}]*overflow-y: auto/);
    // AI 主区：占满剩余空间、自身滚动，页面 body 不滚动（推流不被日志挤动）
    assert.match(html, /section\.ai-pane \{[^}]*flex: 1[^}]*overflow-y: auto/);
    assert.match(html, /body \{[^}]*overflow: hidden/);
    // 折叠态支持（details）
    assert.match(html, /<details class="logs-pane" open>/);
    // 载入贴底（初始尾随视角）+ 多时机补偿（布局晚于载入时单次赋值落空 → 停在第一行）
    assert.match(html, /aiPane\.scrollTop = aiPane\.scrollHeight/);
    assert.match(html, /requestAnimationFrame\(stick\)/);
    assert.match(html, /document\.fonts\.ready/);
    assert.match(html, /addEventListener\('toggle', stick\)/);
    // 流式增量：postMessage 更新 DOM，绝不整页重载（重载会不断销毁滚动位置 → 推流中滑不动）
    assert.match(html, /addEventListener\('message'/);
    // 贴底才跟随：内容替换前记录 nearBottom，上滑阅读历史则保持位置不拽回
    assert.match(html, /function nearBottom/);
    assert.match(html, /var aiPin = aiPane \? nearBottom\(aiPane\) : true/);
    assert.match(html, /if \(aiPin && aiPane\) aiPane\.scrollTop = aiPane\.scrollHeight/);
    // 更新目标分区有稳定锚点 id（页内脚本据此替换内容）
    for (const id of ['id="h1"', 'id="barFill"', 'id="chips"', 'id="logsBox"', 'id="aiBody"']) {
      assert.ok(html.includes(id), `缺少更新锚点 ${id}`);
    }
  });

  it('AI 正文与日志分区渲染（错误级别高亮类）', () => {
    const html = renderAiContextHtml({ state: mk() });
    assert.ok(html.includes('# 审计会话'));
    assert.match(html, /class="log err"/);
    assert.match(html, /dsh-agent/);
    assert.match(html, /boom/);
  });

  it('buildViewUpdate：增量消息携带头部/进度/阶段/日志/AI 各分区内容', () => {
    const u = buildViewUpdate(mk());
    assert.strictEqual(u.type, 'update');
    assert.strictEqual(u.percent, 42);
    assert.match(u.h1Html, /任务 gw-abcde/);
    assert.match(u.h1Html, /运行中 · 42%/);
    assert.match(u.chipsHtml, /chip done/);
    assert.match(u.chipsHtml, /chip running/);
    assert.match(u.logsHtml, /class="log err"/);
    assert.match(u.aiHtml, /<pre id="ai">/);
    // AI 内容经转义进入增量消息（与整页渲染同口径）
    assert.ok(!/<img /.test(u.aiHtml));
  });

  it('无 AI 内容时给出空态说明而非空白区', () => {
    const s = createProgressState('t1');
    applyFrame(s, {
      task: { task_id: 't1', project_id: '', scan_mode: '', sast_tools: [], status: 'TASK_STATUS_RUNNING', stages: [], error_message: '' },
      progress: null, logs: null, ai: null,
    });
    const html = renderAiContextHtml({ state: s });
    assert.match(html, /尚无 AI 交互内容/);
    assert.match(html, /暂无任务日志/);
  });

  it('超长 AI 正文按渲染上限保尾（防巨帧卡死 webview）', () => {
    const s = createProgressState('t1');
    const big = 'x'.repeat(300 * 1024);
    applyFrame(s, {
      task: { task_id: 't1', project_id: '', scan_mode: '', sast_tools: [], status: 'TASK_STATUS_RUNNING', stages: [], error_message: '' },
      progress: null, logs: null,
      ai: { chunk: b64(big), next_cursor: String(big.length), complete: true, total_bytes: String(big.length) },
    });
    const html = renderAiContextHtml({ state: s });
    assert.ok(html.length < big.length + 64 * 1024, `渲染 HTML 应明显小于原始正文（实际 ${html.length}）`);
    assert.ok(html.includes('x'.repeat(1000)), '尾部内容保留');
  });
});
