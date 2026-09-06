// T2 完成标准：04 §3 五模式向导分支覆盖（ADR-186）
// ADR-203: 迁移到 fakeGateway（axios adapter 层）——api/client 真实代码全量执行
// （类型化端点/FormData 上传/拦截器链），handler 返回响应，未建模路由响亮失败。
// 此前 vi.mock 整模块曾长期掩盖 mock 缺 api 具名导出（queryFn 抛错被 react-query 吞，
// 数据从未加载仍全绿）。创建请求体矩阵（upload_file_id/project_path 优先级）在本文件锁定。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import TaskNewPage, { DEFAULT_SCAN_MODE, MODE_SPECS } from '../pages/tasks/TaskNewPage';
import { useFakeGateway } from '../testsupport/fakeGateway';

// 向导共用的最小网关模型（文件级注册，beforeEach 对全部用例生效；请求日志按用例隔离）
const gateway = useFakeGateway({
  'GET /v1/projects': {
    projects: [{ project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null }],
    pagination: { next_cursor: '', has_next: false, total: 1 },
  },
  // proto L845: GetProject 返回裸 Project；repo_url 为空 → 非仓库模式（手填路径/上传件）
  'GET /v1/projects/p1': { project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null },
  'GET /v1/projects/p1/config': { project_id: 'p1', config: {} },
  'GET /v1/tools': {
    tools: [
      { tool_id: 'bandit', name: 'bandit', supported_languages: ['python'], output_format: 'bandit', valid: true, errors: [] },
      { tool_id: 'codeql', name: 'codeql (parser only; no executor mapping)', supported_languages: [], output_format: 'json', valid: false, errors: ['no executor mapping'] },
    ],
  },
  // ADR-200: storage 直传（gateway 返回 file_id → config.upload_file_id）
  'POST /v1/uploads/archive': { upload_id: 'u1', file_id: 'fid-1', file_path: 'uploads/u1.zip', size_bytes: 2048 },
  'POST /v1/tasks': { task_id: 't-new-1' },
});

function renderWizard() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/tasks/new']}>
        <TaskNewPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('MODE_SPECS（ADR-186 五模式分支单一来源）', () => {
  it('A 纯SAST/B 纯AI/C 融合/D AI增强/E 对比：仅 B 无工具；旧 D 审核配置保留在弃用项', () => {
    expect(MODE_SPECS.SCAN_MODE_SAST_ONLY).toMatchObject({ needsSastTools: true, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_AI_ONLY).toMatchObject({ needsSastTools: false, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_PARALLEL).toMatchObject({ needsSastTools: true, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_AI_ENHANCED_SAST).toMatchObject({ needsSastTools: true, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_COMPARE).toMatchObject({ needsSastTools: true, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_SAST_REVIEW).toMatchObject({ needsSastTools: true, needsReviewConfig: true, deprecated: true });
  });
  it('五新模式齐备 + 两弃用项；默认推荐模式C', () => {
    expect(Object.keys(MODE_SPECS)).toHaveLength(7);
    expect(DEFAULT_SCAN_MODE).toBe('SCAN_MODE_PARALLEL');
  });
});

describe('TaskNewPage 向导', () => {
  it('渲染向导骨架（项目选择器+四步步骤条）', async () => {
    renderWizard();
    await screen.findByText('选择项目');
    // 步骤条四步
    for (const t of ['项目', '模式', '参数', '确认']) {
      expect(screen.getAllByText(t).length).toBeGreaterThan(0);
    }
    // 下一步按钮初始禁用（未选项目）
    const next = screen.getByRole('button', { name: '下一步' });
    expect(next).toHaveProperty('disabled', true);
    // ADR-203 数据到达性：列表真经类型化端点加载（假网关下占位符与数据并存）
    fireEvent.mouseDown(screen.getByRole('combobox'));
    expect(await screen.findByText('Demo (p1)')).toBeTruthy();
  });

  // ADR-154 回归（行为级锁）：确认页只随第4步出现；创建按钮不在向导前段渲染。
  // （.agent/evidence/gui-audit/ 为一次性 GUI 取证脚本，非常设回归套件；
  //   行为锁由本文件 vitest 用例承担——ADR-203 mock 纪律）
  it('ADR-154: 确认页按钮不前漏（步骤边界正确）', async () => {
    renderWizard();
    await screen.findByText('选择项目');
    expect(screen.queryByRole('button', { name: '创建任务' })).toBeNull();
  });
});

describe('TaskNewPage 上传件与扫描路径（ADR-202 回归）', () => {
  // 走到参数步：选项目 → 模式B 纯AI（needsSastTools=false，绕开工具多选）
  async function gotoParamsStep() {
    fireEvent.mouseDown(screen.getByRole('combobox'));
    fireEvent.click(await screen.findByText('Demo (p1)'));
    fireEvent.click(screen.getByRole('button', { name: '下一步' })); // step0 → 1
    fireEvent.click(await screen.findByText(/模式B 纯AI/));
    fireEvent.click(screen.getByRole('button', { name: '下一步' })); // step1 → 2
    await screen.findByText('上传代码压缩包（推荐）');
  }

  function uploadZip(container: HTMLElement) {
    const input = container.querySelector('input[type="file"]') as HTMLInputElement;
    const file = new File(['PK\x03\x04'], 'code.zip', { type: 'application/zip' });
    Object.defineProperty(input, 'files', { value: [file], configurable: true });
    fireEvent.change(input);
  }

  function pathInput(): HTMLInputElement {
    return screen.getByPlaceholderText('/path/to/project') as HTMLInputElement;
  }

  // ADR-203 资金流：关掉自动启动（start 链路属 stateMachine 测试域），触发创建并捕获请求体
  async function createAndCaptureTaskBody(): Promise<Record<string, unknown>> {
    fireEvent.click(screen.getByRole('button', { name: '下一步：确认' }));
    fireEvent.click(await screen.findByText(/创建后立即启动/)); // 关闭自动启动
    fireEvent.click(await screen.findByRole('button', { name: '创建任务' }));
    await screen.findByText('任务已创建');
    const post = gateway.requests.find((r) => r.method === 'POST' && r.url === '/v1/tasks');
    expect(post).toBeTruthy();
    return post!.body as Record<string, unknown>;
  }

  it('上传成功后扫描路径免填：非必填+置灰，空路径可进确认页', async () => {
    const { container } = renderWizard();
    await gotoParamsStep();
    expect(pathInput().disabled).toBe(false);

    uploadZip(container);
    await screen.findByText(/已上传至存储/); // ADR-200 直传 storage 成功提示（真 uploadArchive 经 fakeGateway）
    expect(pathInput().disabled).toBe(true); // ADR-202: 上传件优先，路径置灰免填

    fireEvent.click(screen.getByRole('button', { name: '下一步：确认' }));
    // 空路径通过校验直达确认页，且回显 storage 通道而非路径
    expect(await screen.findByText(/已上传存储（启动时从 storage 拉回解包）/)).toBeTruthy();

    // 受控 fileList（ADR-202）：上一步往返后上传件展示不丢，路径仍置灰
    fireEvent.click(screen.getByRole('button', { name: '上一步' }));
    expect(await screen.findByText('code.zip')).toBeTruthy();
    expect(pathInput().disabled).toBe(true);
  });

  it('移除已上传件后路径恢复手填模式：重新启用且必填', async () => {
    const { container } = renderWizard();
    await gotoParamsStep();
    uploadZip(container);
    await screen.findByText(/已上传至存储/);

    fireEvent.click(container.querySelector('.ant-upload-list-item-actions button')!); // 移除上传件
    expect(pathInput().disabled).toBe(false); // ADR-202: file_id 已清，路径模式恢复
    expect(container.querySelector('.ant-upload-list-item')).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: '下一步：确认' }));
    // 空路径被必填规则拦下，停留参数步
    expect(await screen.findByRole('button', { name: '下一步：确认' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: '创建任务' })).toBeNull();
  });

  it('ADR-202/200 创建请求体矩阵①：上传件优先——config 只含 upload_file_id，无 project_path', async () => {
    const { container } = renderWizard();
    await gotoParamsStep();
    uploadZip(container);
    await screen.findByText(/已上传至存储/);
    const body = (await createAndCaptureTaskBody()) as { config: Record<string, string> };
    expect(body.config).toEqual({ upload_file_id: 'fid-1' });
  });

  it('ADR-202/200 创建请求体矩阵②：未上传手填路径——config.project_path 兜底生效', async () => {
    renderWizard();
    await gotoParamsStep();
    fireEvent.change(pathInput(), { target: { value: '/srv/code/demo' } });
    const body = (await createAndCaptureTaskBody()) as { sast_tools: string[]; config: Record<string, string> };
    expect(body.config).toEqual({ project_path: '/srv/code/demo' }); // 无上传件 → 路径兜底，键不混杂
    expect(body.sast_tools).toEqual([]); // 模式B 无工具
  });
});
