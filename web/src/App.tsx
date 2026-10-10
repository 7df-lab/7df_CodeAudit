// 路由表 = 14号 §3.2 页面清单（T1 仅实现 P0 三页骨架，其余路由占位到 T2+）
import { Navigate, Route, Routes, useNavigate, useLocation, useParams } from 'react-router-dom';
import { Badge, Button, Dropdown, Layout, Menu, Typography } from 'antd';
import { DownOutlined } from '@ant-design/icons';
import { useQueryClient, useQuery } from '@tanstack/react-query';
import { useSession } from './auth/session';
import { api } from './api/client';
import LoginPage from './pages/LoginPage';
import RegisterPage from './pages/RegisterPage';
import ChangePasswordPage from './pages/ChangePasswordPage';
import DashboardPage from './pages/dashboard/DashboardPage';
import ProjectsPage from './pages/ProjectsPage';
import ProjectNewPage from './pages/ProjectNewPage';
import ProjectDetailPage from './pages/ProjectDetailPage';
import TasksPage from './pages/tasks/TasksPage';
import OrchestrationsPage from './pages/OrchestrationsPage';
import TaskNewPage from './pages/tasks/TaskNewPage';
import TaskDetailPage from './pages/tasks/TaskDetailPage';
import FindingsPage from './pages/findings/FindingsPage';
import FindingDetailPage from './pages/findings/FindingDetailPage';
import FusionView from './pages/views/FusionView';
import ComparisonView from './pages/views/ComparisonView';
import ReportsPage from './pages/reports/ReportsPage';
import ReportDetailPage from './pages/reports/ReportDetailPage';
import NotificationsPage from './pages/notifications/NotificationsPage';
import UsersPage from './pages/admin/UsersPage';
import WorkspacesPage from './pages/admin/WorkspacesPage';
import { ApiErrorOverlay, ErrorPage } from './components/errors';
import BrandMark from './components/BrandMark';
import { PageLoading } from './components/states';
import type { ReactNode } from 'react';

// ADR-156: 顶部"通知"菜单挂未读角标——通知的价值在"不在该页也知道有事"；
// 60s 静默轮询兜底（triage/任务事件触发时由 notify-unread 缓存失效即时刷新）。
export function useUnreadCount(): number {
  const { user } = useSession();
  const { data } = useQuery({
    queryKey: ['notify-unread', user?.user_id],
    enabled: !!user,
    refetchInterval: 60_000,
    queryFn: async () =>
      (await api.get('/v1/notifications', { params: { user_id: user?.user_id ?? '' } })).data as {
        notifications: { read: boolean }[];
      },
  });
  return (data?.notifications ?? []).filter((n) => !n.read).length;
}

function Shell({ children }: { children: ReactNode }) {
  const { user, booting, logout } = useSession();
  const navigate = useNavigate();
  const location = useLocation();
  const qc = useQueryClient();
  const unread = useUnreadCount();
  if (booting) {
    return (
      <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
        <PageLoading tip="会话恢复中…" />
      </div>
    );
  }
  if (!user) return <Navigate to="/login" replace />;
  // V2.1 (ADR-205): 首登/重置后强制改密——未改密前锁死在改密页（放行改密页自身防回环）
  if (user.must_change_password && location.pathname !== '/change-password') {
    return <Navigate to="/change-password" replace />;
  }
  // ADR-156: 导航按当前路径高亮（此前 selectable={false} 恒无选中态，用户不知道"你在哪"）
  // 设计整改 P4: 重排 = 首页(态势总览) → 项目 → 任务 → 报告中心 → 通知 │ admin 低频管理面置尾
  const selectedKey =
    location.pathname === '/' ? 'home'
    : location.pathname.startsWith('/projects') ? 'projects'
    : location.pathname.startsWith('/tasks') ? 'tasks'
    // 2026-10-04 导航重整：发现详情=任务产物下钻（无独立列表入口），归"任务"高亮——
    // 下钻页不断"你在哪"的灯（此前 startsWith 链无 findings 分支，全导航无选中态）
    : location.pathname.startsWith('/findings') ? 'tasks'
    : location.pathname.startsWith('/orchestrations') ? 'orchestrations'
    : location.pathname.startsWith('/reports') ? 'reports'
    : location.pathname.startsWith('/notifications') ? 'notifications'
    : location.pathname.startsWith('/admin/users') ? 'users'
    : location.pathname.startsWith('/admin/workspaces') ? 'workspaces'
    : '';
  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Layout.Header style={{ display: 'flex', alignItems: 'center' }}>
        {/* 设计整改 P3: 品牌区挂 BrandMark（靛墨 </> + severity 点，与 favicon 同构）。
            2026-10-04 导航重整：点品牌回家=最普适的 Web 惯例——此前是死 div，
            回首页只能去菜单找"首页"；补键盘可达（Enter/Space 等价点击） */}
        <div
          role="link"
          tabIndex={0}
          aria-label="返回首页"
          onClick={() => navigate('/')}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); navigate('/'); }
          }}
          style={{ color: '#fff', fontWeight: 600, marginRight: 24, display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer' }}
        >
          <BrandMark size={22} />
          CodeAudit
        </div>
        {/* 2026-10-04 导航重整（用户指令"基于第一性原理"）：分界线编码使用节奏——
            左 Menu=每次访问的工件流（首页锚点+材料→执行→产物，P4 人类指令链原样保留）；
            右 Menu=配置与治理（设一次反复用）。编排自"任务→报告中心"之间迁出：它是被
            任务引用的定义库（内置只读、写面 admin），不是管道环节——2026-09-20 入口
            打通批就地插位未带排序理由，把工件因果链凿断了。
            注：antd v5 横向菜单 {type:'divider'} 无横式样式（实测渲染 0 宽隐形，
            像素级验证 2026-10-04），故拆双 Menu + 自绘分隔线 */}
        <Menu
          theme="dark"
          mode="horizontal"
          selectedKeys={selectedKey ? [selectedKey] : []}
          items={[
            { key: 'home', label: '首页', onClick: () => navigate('/') },
            { key: 'projects', label: '项目', onClick: () => navigate('/projects') },
            { key: 'tasks', label: '任务', onClick: () => navigate('/tasks') },
            { key: 'reports', label: '报告中心', onClick: () => navigate('/reports') },
          ]}
        />
        <div
          data-testid="nav-zone-divider"
          aria-hidden
          style={{ width: 1, alignSelf: 'center', height: 16, background: 'rgba(255,255,255,0.25)', margin: '0 12px' }}
        />
        <Menu
          theme="dark"
          mode="horizontal"
          selectedKeys={selectedKey ? [selectedKey] : []}
          items={[
            { key: 'orchestrations', label: '编排', onClick: () => navigate('/orchestrations') },
            // P4 修订: 通知移至最末位——工作流（项目→任务→报告）
            // 优先于横切提醒；admin 管理面居通知之前
            // V2.1 (ADR-205): 用户管理仅管理员可见（路由另有 RequireAdmin 守卫）
            ...(user.role === 'ROLE_ADMIN'
              ? [{ key: 'users', label: '用户管理', onClick: () => navigate('/admin/users') }]
              : []),
            // ADR-217+切片3b+父含子合并批：工作区=一等治理对象（team 上车
            // 状态机：建区→配推理→就绪），推理配置为其内嵌面（行内抽屉）——导航收敛单入口
            ...(user.role === 'ROLE_ADMIN'
              ? [{ key: 'workspaces', label: '工作区', onClick: () => navigate('/admin/workspaces') }]
              : []),
            {
              key: 'notifications',
              // 未读角标挂在导航上（ADR-156）；进入通知页即拉取，读后角标随缓存失效消失。
              // 设计整改 P2: 文本括号"（N 未读）"→ Badge 计数角标
              label: (
                <span onClick={() => { qc.invalidateQueries({ queryKey: ['notify-unread'] }); navigate('/notifications'); }}>
                  通知{' '}<Badge count={unread} size="small" />
                </span>
              ),
            },
          ]}
          style={{ flex: 1 }}
        />
        {/* 设计整改 P2: 用户区两按钮收 Dropdown——窄屏 Header 不再溢出；登出属破坏性动作
            收进菜单，顶栏只保留身份 */}
        <Dropdown
          menu={{
            items: [
              { key: 'pwd', label: '修改密码', onClick: () => navigate('/change-password') },
              { type: 'divider' },
              { key: 'logout', label: '登出', onClick: () => logout() },
            ],
          }}
          placement="bottomRight"
        >
          <Button type="text" style={{ color: '#fff' }}>
            {user.username} <DownOutlined style={{ fontSize: 10 }} />
          </Button>
        </Dropdown>
      </Layout.Header>
      {/* 14号 §3.5: 全局 API 错误组件挂载点（403/501 整页 fixed 不受挂点影响；503 降级
          横幅原挂 Header 首位会作为 flex 项嵌进深色导航行内——移到 Header 之下随文档流
          全宽展开，下推内容不再遮导航） */}
      <ApiErrorOverlay />
      {/* 设计整改 P4（2026-09-13 人类指令）：内容区改全宽流式——1440 定宽在 ≥1920 屏两侧
          各浪费 ~240px+（任务详情尤甚），数据密集型控制台应吃满宽度 */}
      <Layout.Content style={{ padding: 24 }}>{children}</Layout.Content>
    </Layout>
  );
}

function ComparisonWithParams() {
  const { id = '' } = useParams();
  return <ComparisonView taskId={id} />;
}
function FindingDetailWithParams() {
  const { fid = '' } = useParams();
  return <FindingDetailPage findingId={fid} />;
}

function TaskDetailWithParams() {
  const { id = '' } = useParams();
  // B4-1（审计修复）：key 随任务 id 重建组件实例——同路由 `/tasks/:id` 切换任务时
  // react-router 默认复用组件实例，旧任务的增量游标（logAfter/aiCursor/seenLogIds refs）
  // 与已吸收的日志/AI 正文会带进新任务（快照带旧 logs_after 游标、面板残留旧 AI 片段）。
  // 实例重建 = 全部 state/refs 自然复位，无需逐项 reset。
  return <TaskDetailPage key={id} taskId={id} />;
}

// V2.1 (ADR-205): 管理端路由守卫——非 ROLE_ADMIN 渲染 403（后端网关 requireAdmin 为最终防线）
function RequireAdmin({ children }: { children: ReactNode }) {
  const { user } = useSession();
  if (user?.role !== 'ROLE_ADMIN') {
    return <ErrorPage code={403} />;
  }
  return <>{children}</>;
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      {/* V2.1 (ADR-205): 注册页为公开路由（与 /login 同级，Shell 之外） */}
      <Route path="/register" element={<RegisterPage />} />
      {/* P4: 登录后落总览（此前无首页，'/' 仅重定向 /projects） */}
      <Route path="/" element={<Shell><DashboardPage /></Shell>} />
      <Route path="/projects" element={<Shell><ProjectsPage /></Shell>} />
      {/* 流程统一批（2026-10-07）：/projects/new 薄壳与 /tasks/new 同构（v6 静态段
          自动优先于动态段 :id，先后仅可读性） */}
      <Route path="/projects/new" element={<Shell><ProjectNewPage /></Shell>} />
      <Route path="/projects/:id" element={<Shell><ProjectDetailPage /></Shell>} />
      <Route path="/tasks" element={<Shell><TasksPage /></Shell>} />
      <Route path="/tasks/new" element={<Shell><TaskNewPage /></Shell>} />
      <Route path="/orchestrations" element={<Shell><OrchestrationsPage /></Shell>} />
      <Route path="/tasks/:id" element={<Shell><TaskDetailWithParams /></Shell>} />
      <Route path="/findings/:fid" element={<Shell><FindingDetailWithParams /></Shell>} />
      <Route path="/tasks/:id/comparison" element={<Shell><ComparisonWithParams /></Shell>} />
      <Route path="/reports" element={<Shell><ReportsPage /></Shell>} />
      <Route path="/reports/:id" element={<Shell><ReportDetailPage /></Shell>} />
      <Route path="/notifications" element={<Shell><NotificationsPage /></Shell>} />
      {/* V2.1 (ADR-205): 首登强改密页 + 管理端用户列表（admin 门禁） */}
      <Route path="/change-password" element={<Shell><ChangePasswordPage /></Shell>} />
      <Route path="/admin/users" element={<Shell><RequireAdmin><UsersPage /></RequireAdmin></Shell>} />
      {/* ADR-217: 推理 Provider 管理（admin 门禁；/v1/inference/* 全路由 requireAdmin） */}
      {/* 父含子合并批（2026-10-05）：推理配置收敛入工作区页——旧路径重定向保书签 */}
      <Route path="/admin/providers" element={<Navigate to="/admin/workspaces" replace />} />
      <Route path="/admin/workspaces" element={<Shell><RequireAdmin><WorkspacesPage /></RequireAdmin></Shell>} />
      {/* 14号 §3.5: 未知路由 → 404 空态（此前静默重定向回项目页, 用户不知道发生了什么） */}
      <Route path="*" element={<Shell><ErrorPage code={404} /></Shell>} />
    </Routes>
  );
}
