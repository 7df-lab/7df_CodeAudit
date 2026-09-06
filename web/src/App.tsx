// 路由表 = 14号 §3.2 页面清单（T1 仅实现 P0 三页骨架，其余路由占位到 T2+）
import { Navigate, Route, Routes, useNavigate, useLocation } from 'react-router-dom';
import { Layout, Menu, Button } from 'antd';
import { useQueryClient } from '@tanstack/react-query';
import { useSession } from './auth/session';
import { api } from './api/client';
import { useQuery } from '@tanstack/react-query';
import LoginPage from './pages/LoginPage';
import RegisterPage from './pages/RegisterPage';
import ChangePasswordPage from './pages/ChangePasswordPage';
import ProjectsPage from './pages/ProjectsPage';
import ProjectDetailPage from './pages/ProjectDetailPage';
import TasksPage from './pages/tasks/TasksPage';
import TaskNewPage from './pages/tasks/TaskNewPage';
import TaskDetailPage from './pages/tasks/TaskDetailPage';
import FindingsPage from './pages/findings/FindingsPage';
import FindingDetailPage from './pages/findings/FindingDetailPage';
import FusionView from './pages/views/FusionView';
import ComparisonView from './pages/views/ComparisonView';
import ReportsPage from './pages/reports/ReportsPage';
import NotificationsPage from './pages/notifications/NotificationsPage';
import UsersPage from './pages/admin/UsersPage';
import { ApiErrorOverlay, ErrorPage } from './components/errors';

import { useParams } from 'react-router-dom';
import { Typography } from 'antd';
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
      <div style={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '100vh' }}>
        <Typography.Text type="secondary">会话恢复中…</Typography.Text>
      </div>
    );
  }
  if (!user) return <Navigate to="/login" replace />;
  // V2.1 (ADR-205): 首登/重置后强制改密——未改密前锁死在改密页（放行改密页自身防回环）
  if (user.must_change_password && location.pathname !== '/change-password') {
    return <Navigate to="/change-password" replace />;
  }
  // ADR-156: 导航按当前路径高亮（此前 selectable={false} 恒无选中态，用户不知道"你在哪"）
  const selectedKey =
    location.pathname.startsWith('/projects') ? 'projects'
    : location.pathname.startsWith('/tasks') ? 'tasks'
    : location.pathname.startsWith('/reports') ? 'reports'
    : location.pathname.startsWith('/notifications') ? 'notifications'
    : location.pathname.startsWith('/admin/users') ? 'users'
    : '';
  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Layout.Header style={{ display: 'flex', alignItems: 'center' }}>
        {/* 14号 §3.5: 全局 API 错误组件挂载点（403/501 整页 + 503 降级横幅） */}
        <ApiErrorOverlay />
        <div style={{ color: '#fff', fontWeight: 600, marginRight: 32 }}>CodeAudit</div>
        <Menu
          theme="dark"
          mode="horizontal"
          selectedKeys={selectedKey ? [selectedKey] : []}
          items={[
            { key: 'projects', label: '项目', onClick: () => navigate('/projects') },
            { key: 'tasks', label: '任务', onClick: () => navigate('/tasks') },
            { key: 'reports', label: '报告中心', onClick: () => navigate('/reports') },
            {
              key: 'notifications',
              // 未读角标挂在导航上（ADR-156）；进入通知页即拉取，读后角标随缓存失效消失
              label: (
                <span onClick={() => { qc.invalidateQueries({ queryKey: ['notify-unread'] }); navigate('/notifications'); }}>
                  通知{unread > 0 ? `（${unread} 未读）` : ''}
                </span>
              ),
            },
            // V2.1 (ADR-205): 用户管理仅管理员可见（路由另有 RequireAdmin 守卫）
            ...(user.role === 'ROLE_ADMIN'
              ? [{ key: 'users', label: '用户管理', onClick: () => navigate('/admin/users') }]
              : []),
          ]}
          style={{ flex: 1 }}
        />
        <span style={{ color: '#fff', marginRight: 12 }}>{user.username}</span>
        <Button onClick={() => navigate('/change-password')} style={{ marginRight: 8 }}>
          修改密码
        </Button>
        <Button onClick={() => logout()}>登出</Button>
      </Layout.Header>
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
  return <TaskDetailPage taskId={id} />;
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
      <Route path="/" element={<Shell><Navigate to="/projects" replace /></Shell>} />
      <Route path="/projects" element={<Shell><ProjectsPage /></Shell>} />
      <Route path="/projects/:id" element={<Shell><ProjectDetailPage /></Shell>} />
      <Route path="/tasks" element={<Shell><TasksPage /></Shell>} />
      <Route path="/tasks/new" element={<Shell><TaskNewPage /></Shell>} />
      <Route path="/tasks/:id" element={<Shell><TaskDetailWithParams /></Shell>} />
      <Route path="/findings/:fid" element={<Shell><FindingDetailWithParams /></Shell>} />
      <Route path="/tasks/:id/comparison" element={<Shell><ComparisonWithParams /></Shell>} />
      <Route path="/reports" element={<Shell><ReportsPage /></Shell>} />
      <Route path="/notifications" element={<Shell><NotificationsPage /></Shell>} />
      {/* V2.1 (ADR-205): 首登强改密页 + 管理端用户列表（admin 门禁） */}
      <Route path="/change-password" element={<Shell><ChangePasswordPage /></Shell>} />
      <Route path="/admin/users" element={<Shell><RequireAdmin><UsersPage /></RequireAdmin></Shell>} />
      {/* 14号 §3.5: 未知路由 → 404 空态（此前静默重定向回项目页, 用户不知道发生了什么） */}
      <Route path="*" element={<Shell><ErrorPage code={404} /></Shell>} />
    </Routes>
  );
}
