// 登录页（14号 §3.2 P0）
import { Alert, Card, Form, Input, Button, Typography, Spin } from 'antd';
import { Navigate, useNavigate } from 'react-router-dom';
import { useSession } from '../auth/session';
import { useState } from 'react';

export default function LoginPage() {
  const { user, booting, login } = useSession();
  const navigate = useNavigate();
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  // ADR-147 修复：已登录用户访问 /login 应回项目页（此前矛盾：登录态下仍显示登录表单）
  if (booting) return <div style={{ display: 'flex', justifyContent: 'center', paddingTop: 120 }}><Spin /></div>;
  if (user) return <Navigate to="/projects" replace />;

  return (
    <div style={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '100vh', background: '#f0f2f5' }}>
      <Card style={{ width: 380 }} title={<Typography.Title level={4} style={{ margin: 0 }}>CodeAudit 控制台</Typography.Title>}>
        {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} showIcon />}
        <Form
          onFinish={async ({ username, password }) => {
            setLoading(true);
            setError(null);
            try {
              await login(username, password);
              navigate('/');
            } catch (e) {
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
        </Form>
      </Card>
    </div>
  );
}
