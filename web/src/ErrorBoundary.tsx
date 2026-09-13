// 全局错误边界（ADR-143 附带）：渲染期异常显示可读错误而非白屏（白屏=不可诊断的静默失败）。
// （2026-09-13）：裸 h2/pre/button + 自造黄色系 → antd Result 归队全站语言；
// 错误详情保留等宽渲染（机器产物=证据嗓音）。
import { Button, Result, Typography } from 'antd';
import { Component, type ReactNode } from 'react';
import { MONO_FONT } from './dict/tokens';

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
        <Result
          status="error"
          title="页面渲染出错（已拦截，不再是白屏）"
          subTitle="渲染期异常被全局错误边界捕获；重载通常可恢复，若复现请携带下方信息反馈。"
          extra={
            <Button type="primary" onClick={() => { this.setState({ error: null }); window.location.reload(); }}>
              重载页面
            </Button>
          }
        >
          <Typography.Paragraph type="secondary" style={{ textAlign: 'left', wordBreak: 'break-all' }}>
            <pre style={{ fontFamily: MONO_FONT, fontSize: 12, whiteSpace: 'pre-wrap', margin: 0 }}>
              {String(this.state.error)}
            </pre>
          </Typography.Paragraph>
        </Result>
      );
    }
    return this.props.children;
  }
}
