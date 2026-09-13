// （2026-09-13）签名元素回归：Source→Sink 证据链组件。
// 规则：≥2 步才成"链"（单点不渲染）；首=源（青）/末=汇（红）/中间灰；chip 可点选定位。
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import EvidenceChain from '../components/EvidenceChain';

const STEPS = [
  { path: '/app/web/OrderController.java', line: 93, content: 'String orderNo = req.getParameter("order_no");', role: 'source' as const },
  { path: '/app/service/OrderService.java', line: 120, content: 'return dao.findByNo(orderNo);' },
  { path: '/app/dao/OrderDao.java', line: 49, content: 'st.executeQuery(sql);', role: 'sink' as const },
];

describe('EvidenceChain（签名元素）', () => {
  it('三步链：源/汇标签 + 箭头连接 + 点击回调带行号', () => {
    const onSelect = vi.fn();
    const { container } = render(<EvidenceChain steps={STEPS} onSelect={onSelect} />);
    expect(screen.getByText(/源 OrderController\.java:93/)).toBeTruthy();
    expect(screen.getByText(/OrderService\.java:120/)).toBeTruthy();
    expect(screen.getByText(/汇 OrderDao\.java:49/)).toBeTruthy();
    // 两个箭头（步数-1）
    expect(container.querySelectorAll('.anticon-arrow-right').length).toBe(2);
    fireEvent.click(screen.getByText(/汇 OrderDao\.java:49/));
    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(onSelect.mock.calls[0][0]).toMatchObject({ path: '/app/dao/OrderDao.java', line: 49 });
  });
  it('少于 2 步不渲染（不为单点造流程感）', () => {
    const { container } = render(<EvidenceChain steps={[STEPS[0]]} />);
    expect(container.querySelector('[data-testid="evidence-chain"]')).toBeNull();
  });
});
