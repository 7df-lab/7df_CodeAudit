// 全局错误边界（ADR-143 附带）：渲染期异常显示可读错误而非白屏（白屏=不可诊断的静默失败）。
import { Component, type ReactNode } from 'react';

interface Props { children: ReactNode }
interface State { error: Error | null }

export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: { componentStack?: string }) {
    // 留痕到控制台便于排障（不上报后端——演示口径）
    console.error('[ErrorBoundary]', error, info.componentStack);
  }

  render() {
    if (this.state.error) {
      return (
        <div style={{ margin: 40, padding: 24, border: '1px solid #ffa940', borderRadius: 8, background: '#fffbe6' }}>
          <h2 style={{ color: '#d46b08', marginTop: 0 }}>页面渲染出错（已拦截，不再是白屏）</h2>
          <pre style={{ whiteSpace: 'pre-wrap', fontSize: 13, color: '#333' }}>{String(this.state.error)}</pre>
          <button onClick={() => { this.setState({ error: null }); window.location.reload(); }}>重载页面</button>
        </div>
      );
    }
    return this.props.children;
  }
}
