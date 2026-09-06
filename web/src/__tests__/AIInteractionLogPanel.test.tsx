// AI 交互日志面板回归（ADR-168 补遗②/170 受控化/173 DSH web 时间线/181 交互重做/
// 188 内联主视图）：标记行解析为分色时间线条目/内联时间线常驻渲染/思考流式展开与
// 归档折叠/任务全文折叠块/子任务骨架/渐进回看/整页 Modal 辅入口。
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import AIInteractionLogPanel from '../components/AIInteractionLogPanel';

function renderProps(text: string, totalBytes: number, complete: boolean) {
  return { text, totalBytes, complete, onRefresh: () => {}, refreshing: false };
}

const TEXT =
  '══ DSH 会话开始（bridge）══\n' +
  '── 第 1 轮开始 ──\n' +
  '📋 [任务下发]（3295 字节）\n# CodeAudit 代码安全分析任务\n## 任务 全文在此\n' +
  '💭 [思考]\nLet me analyze…\n' +
  '✍ [输出]\n最终结论：共 2 项发现\n' +
  '🤖 [子任务 4bb5a865] 启动\n' +
  '🤖 [子任务 4bb5a865] 任务（1977 字节）\n你是资深渗透测试员，正在对 Java 项目做白盒审计\n' +
  '📋 [子任务回报]（710 字节）\nBackground subagent reported: 共 3 项发现\n' +
  '── 回合结束: completed ──\n■ 会话空闲（收束）';

function renderPanel(props: { text: string; totalBytes: number; complete: boolean }) {
  return render(
    <AIInteractionLogPanel
      text={props.text}
      totalBytes={props.totalBytes}
      complete={props.complete}
      onRefresh={() => {}}
      refreshing={false}
    />,
  );
}

describe('AIInteractionLogPanel（AI 交互日志，ADR-168/170/181/188）', () => {
  it('ADR-188 内联主视图：时间线常驻渲染（不再默认折叠），状态徽标与工具按钮可见', () => {
    renderPanel({ text: TEXT, totalBytes: 2048, complete: true });
    // 内联时间线直接可见（无需点开）
    const box = screen.getByTestId('ai-interaction-log-box');
    expect(box.textContent!).toContain('DSH 会话开始');
    expect(screen.getByText(/已收束/)).toBeTruthy();
    expect(screen.getByText('2.0 KB')).toBeTruthy();
    expect(screen.getByText('下载完整日志')).toBeTruthy();
    expect(screen.getByText('整页查看')).toBeTruthy();
  });

  it('内联时间线条目语义：任务下发/子任务回报折叠块（摘要含字节数与首行预览），思考归档后折叠', () => {
    renderPanel({ text: TEXT, totalBytes: 2048, complete: true });
    const box = screen.getByTestId('ai-interaction-log-box');
    // ADR-181 补遗（人类反馈"任务下发也要可以支持折叠"）：默认折叠，摘要=图标+标签(字节)+首行
    expect(box.textContent!).toMatch(/任务下发（3295 字节）\s*·\s*# CodeAudit 代码安全分析任务/);
    expect(box.textContent!).toMatch(/子任务回报（710 字节）\s*·\s*Background subagent reported: 共 3 项发现/);
    expect(box.textContent!).toMatch(/子任务 4bb5a865\] 任务（1977 字节）\s*·\s*你是资深渗透测试员，正在对 Java 项目做白盒审计/);
    expect(box.querySelectorAll('details summary').length).toBeGreaterThanOrEqual(4); // 三个折叠块+归档思考
    // 全文仍在折叠块内（DOM 可检索）
    expect(box.textContent!).toContain('## 任务 全文在此');
    expect(box.textContent!).toContain('最终结论：共 2 项发现');
    expect(box.textContent!).toContain('🤖 [子任务 4bb5a865] 启动');
    // 归档后思考可折叠（summary 呈现）
    expect(screen.getByText('💭 模型思考（已归档，点击展开）')).toBeTruthy();
  });

  it('整页查看：Modal 打开同内容时间线（testid 区分于内联）', () => {
    renderPanel({ text: TEXT, totalBytes: 2048, complete: true });
    expect(screen.queryByTestId('ai-interaction-log-box-modal')).toBeNull();
    fireEvent.click(screen.getByText('整页查看'));
    const modalBox = screen.getByTestId('ai-interaction-log-box-modal');
    expect(modalBox.textContent!).toContain('DSH 会话开始');
    expect(modalBox.textContent!).toContain('Let me analyze…');
  });

  it('未收束（实时）：思考流式展开而非折叠（任务下发保持可折叠——静态文本无流式语义）', () => {
    renderPanel({ text: TEXT, totalBytes: 2048, complete: false });
    const box = screen.getByTestId('ai-interaction-log-box');
    expect(box.textContent!).toContain('Let me analyze…'); // 流式展开
    expect(screen.queryByText('💭 模型思考（已归档，点击展开）')).toBeNull();
    expect(box.textContent!).toMatch(/任务下发（3295 字节）/);
    expect(screen.getByText('实时接收中')).toBeTruthy();
  });

  it('渐进回看：超窗口时提供"加载更早/显示全部"，而非只让下载', () => {
    // 构造 >400 条条目（每条一行标记+正文）
    const many = Array.from({ length: 460 }, (_, i) => `✍ [输出]\n第 ${i} 段输出`).join('\n');
    renderPanel({ text: many, totalBytes: 99999, complete: true });
    const box = screen.getByTestId('ai-interaction-log-box');
    expect(box.textContent!).toContain('前面还有 60 条未显示');
    expect(box.textContent!).not.toContain('第 0 段输出'); // 窗口外不渲染
    expect(screen.getByText('显示全部')).toBeTruthy();
    fireEvent.click(screen.getByText('显示全部'));
    expect(screen.queryByText(/前面还有 \d+ 条未显示/)).toBeNull();
    expect(screen.getByTestId('ai-interaction-log-box').textContent!).toContain('第 0 段输出');
  });

  it('空态：内联时间线显示实时回显引导文案', () => {
    renderPanel({ text: '', totalBytes: 0, complete: false });
    expect(screen.getByText(/暂无交互日志/)).toBeTruthy();
    expect(screen.getByText('实时接收中')).toBeTruthy();
  });

  it('修复回归：Modal 开关后内联时间线仍持有滚底 ref（此前共享 boxRef 被 Modal 内容抢占，关 Modal 后内联自动滚底永久失效）', () => {
    const { rerender } = renderPanel({ text: TEXT, totalBytes: 2048, complete: false });
    const inlineBox = screen.getByTestId('ai-interaction-log-box');
    // 劫持内联盒子的 scrollTop setter，观测"新内容到达时是否仍滚内联盒子"
    let scrollWrites = 0;
    const desc = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollTop');
    Object.defineProperty(inlineBox, 'scrollTop', {
      configurable: true,
      get: () => (desc?.get ? desc.get.call(inlineBox) : 0),
      set: () => { scrollWrites += 1; },
    });
    // 打开 Modal（其时间线挂载并抢占共享 ref）→ 关闭（antd Modal 默认不销毁内容）
    fireEvent.click(screen.getByText('整页查看'));
    expect(screen.getByTestId('ai-interaction-log-box-modal')).toBeTruthy();
    fireEvent.click(document.querySelector<HTMLElement>('.ant-modal-close')!);
    // 新内容到达 → effect 应写内联盒子的 scrollTop（修复前 boxRef 指向隐藏 Modal 盒子，不写内联）
    rerender(
      <AIInteractionLogPanel
        {...renderProps(TEXT + '\n✍ [输出]\n增量到达', 4096, false)}
      />,
    );
    expect(scrollWrites).toBeGreaterThan(0);
  });
});
