// AI 交互日志面板回归（ADR-168 补遗②/170 受控化/173 DSH web 时间线）：
// 标记行解析为分色时间线条目/收束徽标/空态。
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import AIInteractionLogPanel from '../components/AIInteractionLogPanel';

const TEXT =
  '══ DSH 会话开始（bridge）══\n[模型路由] provider=deepseek-official · model=glm-5.3-flash\n' +
  '[任务] 已下发（3295 字节）首行: # AuditMind 代码安全分析任务\n── 第 1 轮开始 ──\n💭 [思考]\nLet me analyze…\n── 回合结束: 正常完成 ──';

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

describe('AIInteractionLogPanel（AI 交互日志，ADR-168/170）', () => {
  it('渲染人性化文本与收束徽标、下载按钮', () => {
    renderPanel({ text: TEXT, totalBytes: 2048, complete: true });
    expect(screen.getByTestId('ai-interaction-log-box').textContent!).toContain('DSH 会话开始');
    // ADR-173 时间线：💭 [思考] 标记渲染为可折叠的「模型思考」条目
    expect(screen.getByText('💭 模型思考')).toBeTruthy();
    expect(screen.getByTestId('ai-interaction-log-box').textContent!).toContain('Let me analyze…');
    expect(screen.getByText('已收束')).toBeTruthy();
    expect(screen.getByText('2.0 KB')).toBeTruthy();
    expect(screen.getByText('下载完整日志')).toBeTruthy();
  });

  it('空态：显示实时回显引导文案', () => {
    renderPanel({ text: '', totalBytes: 0, complete: false });
    expect(screen.getByTestId('ai-interaction-log-box').textContent!).toContain('暂无交互日志');
    expect(screen.getByText('实时接收中')).toBeTruthy();
  });
});
