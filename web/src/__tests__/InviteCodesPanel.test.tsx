// 邀请码面板回归（V2.2 ADR-255）：列表渲染/派生态 Tag（用尽/过期不入库此处派生）/
// 配置注入只读区/空态文案（Q7 收敛）/生成 Modal 三控件。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import InviteCodesPanel from '../pages/admin/InviteCodesPanel';
import { useFakeGateway } from '../testsupport/fakeGateway';

vi.mock('../auth/session', () => ({ useCanWrite: () => true,
  useSession: () => ({ user: { user_id: 'u-1', role: 'ROLE_ADMIN' } }),
}));

const active = {
  id: 'invite-1', code: 'CODEAUDIT-AAAAA-BBBBB', max_uses: 1, used_count: 0,
  expires_at: null, state: 'active', note: '给张三', created_by: 'admin', created_at: '2026-10-04T00:00:00Z',
};
const exhausted = { ...active, id: 'invite-2', code: 'CODEAUDIT-CCCCC-DDDDD', max_uses: 5, used_count: 5, note: '' };
const revoked = { ...active, id: 'invite-3', code: 'CODEAUDIT-EEEEE-FFFFF', state: 'revoked' };

const routes: Record<string, unknown> = {};
const gateway = useFakeGateway(routes);

// antd Popconfirm 隐藏容器会让 getByText 多重/破碎匹配——按钮一律走元素级断言
// antd 对两汉字按钮自动插空格（「吊销」→「吊 销」，判例：工作区 playwright 三坑）——归一空白后匹配
const buttonsWith = (t: string) => [...document.querySelectorAll('button')].filter((b) => b.textContent?.replace(/\s+/g, '').includes(t));
const hasButton = (t: string) => buttonsWith(t).length > 0;

function renderPanel() {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <InviteCodesPanel />
    </QueryClientProvider>,
  );
}

describe('InviteCodesPanel（V2.2 ADR-255）', () => {
  it('列表渲染：有效 Tag/码可复制文本/备注/配置注入只读区（Q4/Q13）', async () => {
    routes['GET /v1/invites'] = {
      codes: [active],
      config_codes: ['CODEAUDIT-BOOTSTRAP-2026'],
      pagination: { has_next: false },
    };
    renderPanel();
    expect(await screen.findByText('CODEAUDIT-AAAAA-BBBBB')).toBeInTheDocument();
    expect(screen.getByText('有效')).toBeInTheDocument();
    expect(screen.getByText('给张三')).toBeInTheDocument();
    expect(screen.getByText(/配置注入码/)).toBeInTheDocument();
    expect(screen.getByText('CODEAUDIT-BOOTSTRAP-2026')).toBeInTheDocument();
    // 活跃码：吊销在、删除不在（Q14 两步走）——行寻址到操作单元格断言其文本
    const cell = await waitFor(() => {
      const el = document.querySelector('tr[data-row-key="invite-1"] td:last-child');
      if (!el?.textContent?.replace(/\s+/g, '').includes('吊销')) throw new Error(`操作格内容: ${el?.textContent ?? '(空)'}`);
      return el;
    });
    expect(cell.textContent).not.toContain('删除');
  });

  it('终态码：已用尽/已吊销派生 Tag（存储不入库，前端从数据派生）+删除按钮出现', async () => {
    routes['GET /v1/invites'] = { codes: [exhausted, revoked], pagination: { has_next: false } };
    renderPanel();
    expect(await screen.findByText('CODEAUDIT-CCCCC-DDDDD')).toBeInTheDocument();
    expect(screen.getByText('已用尽')).toBeInTheDocument();
    expect(screen.getByText('已吊销')).toBeInTheDocument();
    await waitFor(() => expect(buttonsWith('删除')).toHaveLength(2));
  });

  it('空态=「尚无邀请码」（Q7 收敛文案，不啰嗦）', async () => {
    routes['GET /v1/invites'] = { codes: [], pagination: { has_next: false } };
    renderPanel();
    expect(await screen.findByText('尚无邀请码')).toBeInTheDocument();
  });

  it('生成 Modal：上限/备注控件 + 永不过期勾选切换过期时间栏', async () => {
    routes['GET /v1/invites'] = { codes: [], pagination: { has_next: false } };
    renderPanel();
    fireEvent.click(await screen.findByText('生成邀请码'));
    expect(await screen.findByText('使用上限')).toBeInTheDocument();
    expect(screen.getByLabelText('备注')).toBeInTheDocument();
    // 过期时间锚=extra 文案（表头同名列会撞字面量）
    expect(screen.getByText('留空=默认 30 天（服务端策略）')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('checkbox'));
    await waitFor(() => expect(screen.queryByText('留空=默认 30 天（服务端策略）')).not.toBeInTheDocument());
  });
});

// 审查批修复锁：>20 枚码静默截断根治——has_next 时出「加载更多」，点击按游标取下页
describe('InviteCodesPanel 分页（审查批修复锁）', () => {
  it('has_next=加载更多按钮推进游标取第二页', async () => {
    routes['GET /v1/invites'] = ({ query }: { query: URLSearchParams }) => {
      const pag = query.get('pagination');
      const page = pag?.includes('20') ? 2 : 1;
      return page === 1
        ? { codes: [active], pagination: { has_next: true, next_cursor: '20', total: 21 } }
        : { codes: [{ ...active, id: 'invite-2', code: 'CODEAUDIT-ZZZZZ-ZZZZZ' }], pagination: { has_next: false, total: 21 } };
    };
    renderPanel();
    expect(await screen.findByText('CODEAUDIT-AAAAA-BBBBB')).toBeInTheDocument();
    const more = await screen.findByRole('button', { name: /加载更多/ });
    fireEvent.click(more);
    await waitFor(() => expect(screen.getByText('CODEAUDIT-ZZZZZ-ZZZZZ')).toBeInTheDocument());
  });
});
