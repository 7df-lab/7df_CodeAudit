// 登录页（14号 §3.2 P0；设计整改 P3: AuthLayout 统一认证壳+品牌块+注册互链）
import { Alert, Form, Input, Button, Typography, Spin } from 'antd';
import { Link, Navigate, useNavigate } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { getRegistrationPolicy } from '../api/client';
import { useSession } from '../auth/session';
import { useState } from 'react';
import { usePageTitle } from '../hooks/usePageTitle';
import AuthLayout from '../components/AuthLayout';

// RegisterLink — 注册互链的档位自适应子组件（policy 查询与登录页解耦）
function RegisterLink() {
  const { data: policy } = useQuery({
    queryKey: ['registration-policy'],
    staleTime: 60_000,
    retry: false,
    queryFn: getRegistrationPolicy,
  });
  const mode = policy?.mode ?? 'invitation';
  if (mode === 'disabled') {
    return <Typography.Text disabled title="当前部署已关闭自助注册">注册（未开放）</Typography.Text>;
  }
  return <Link to="/register">{mode === 'open' ? '注册' : '注册（需邀请码）'}</Link>;
}

export default function LoginPage() {
  usePageTitle('登录');
  const { user, booting, login } = useSession();
  const navigate = useNavigate();
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  // ADR-147+ADR-257: 已登录用户访问 /login 回总览（总览全角色可达；/projects 不再是全员保证面）
  if (booting) return <div style={{ display: 'flex', justifyContent: 'center', paddingTop: 120 }}><Spin /></div>;
  if (user) return <Navigate to="/" replace />;

  return (
    <AuthLayout title="登录">
      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} showIcon />}
      <Form
        layout="vertical"
        onFinish={async ({ username, password }) => {
          setLoading(true);
          setError(null);
          try {
            await login(username, password);
            navigate('/');
          } catch {
            // 凭证错与服务不可用保持同话术（既有安全决策：不泄露内部细节，测试锁定）
            setError('登录失败：用户名或密码错误，或服务不可用');
          } finally {
            setLoading(false);
          }
        }}
      >
        <Form.Item name="username" label="用户名" rules={[{ required: true, message: '请输入用户名' }]}>
          <Input autoFocus autoComplete="username" />
        </Form.Item>
        <Form.Item name="password" label="密码" rules={[{ required: true, message: '请输入密码' }]}>
          <Input.Password autoComplete="current-password" />
        </Form.Item>
        <Button type="primary" htmlType="submit" block loading={loading}>
          登录
        </Button>
        {/* 设计整改 P3: 注册互链（此前登录页无注册入口，注册页全站不可发现）。
            V2.2 (ADR-255, Q10 审查批补半边)：档位自适应——invitation=需邀请码；
            open=免码；disabled=置灰+一句说明。拉取失败=维持 invitation 文案
            （后端 FAILED_PRECONDITION 仍是最终防线）。 */}
        <div style={{ marginTop: 12, textAlign: 'center' }}>
          <Typography.Text type="secondary">还没有账号？</Typography.Text>{' '}
          <RegisterLink />
        </div>
      </Form>
    </AuthLayout>
  );
}
