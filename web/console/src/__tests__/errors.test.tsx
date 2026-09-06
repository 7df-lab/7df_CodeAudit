// 14号 §3.5 统一错误组件回归（ADR-157）: 403/404/501 组件 + 503 降级横幅事件流
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { ApiErrorOverlay, ErrorPage } from '../components/errors';
import { API_ERROR_EVENT, API_OK_EVENT } from '../api/apiEvents';

function wrap(ui: React.ReactNode) {
  return <MemoryRouter initialEntries={['/x']}>{ui}</MemoryRouter>;
}

describe('统一错误组件（14号 §3.5 / ADR-157）', () => {
  it('403 权限不足页：标题+返回动作', () => {
    render(wrap(<ErrorPage code={403} />));
    expect(screen.getByText('权限不足')).toBeTruthy();
    expect(screen.getByText('返回项目页')).toBeTruthy();
  });
  it('404 空态：标题+返回动作', () => {
    render(wrap(<ErrorPage code={404} />));
    expect(screen.getByText('404')).toBeTruthy();
    expect(screen.getByText(/页面不存在/)).toBeTruthy();
  });
  it('501 能力未接入灰卡：诚实降级声明而非故障话术', () => {
    render(wrap(<ErrorPage code={501} />));
    expect(screen.getByText(/能力未接入/)).toBeTruthy();
    expect(screen.getByText(/尚未实现/)).toBeTruthy();
  });
  it('ApiErrorOverlay：503 事件→横幅, 成功事件→撤除, 403 事件→整页且可关闭', async () => {
    render(wrap(<><ApiErrorOverlay /><div>页面主体</div></>));
    window.dispatchEvent(new CustomEvent(API_ERROR_EVENT, { detail: 503 }));
    await waitFor(() => expect(screen.getByText(/暂不可用/)).toBeTruthy());
    window.dispatchEvent(new Event(API_OK_EVENT));
    await waitFor(() => expect(screen.queryByText(/暂不可用/)).toBeNull());
    window.dispatchEvent(new CustomEvent(API_ERROR_EVENT, { detail: 403 }));
    await waitFor(() => expect(screen.getByText('权限不足')).toBeTruthy());
    fireEvent.click(screen.getByText('关闭并返回项目页'));
    await waitFor(() => expect(screen.queryByText('权限不足')).toBeNull());
  });
});
