// 登录页（14号 §3.2 P0； AuthLayout 统一认证壳+品牌块+注册互链）
import { Alert, Form, Input, Button, Typography, Spin } from 'antd';
import { Link, Navigate, useNavigate } from 'react-router-dom';
import { useSession } from '../auth/session';
import { useState } from 'react';
import { usePageTitle } from '../hooks/usePageTitle';
import AuthLayout from '../components/AuthLayout';

export default function LoginPage() {
  usePageTitle('登录');
  const { user, booting, login } = useSession();
  const navigate = useNavigate();
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  // ADR-147 修复：已登录用户访问 /login 应回项目页（此前矛盾：登录态下仍显示登录表单）
  if (booting) return <div style={{ display: 'flex', justifyContent: 'center', paddingTop: 120 }}><Spin /></div>;
  if (user) return <Navigate to="/projects" replace />;

  return (
    <AuthLayout title="登录">
      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} showIcon />}
      <Form
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
        {/*  注册互链（此前登录页无注册入口，注册页全站不可发现） */}
        <div style={{ marginTop: 12, textAlign: 'center' }}>
          <Typography.Text type="secondary">还没有账号？</Typography.Text>{' '}
          <Link to="/register">注册（需邀请码）</Link>
        </div>
      </Form>
    </AuthLayout>
  );
}
