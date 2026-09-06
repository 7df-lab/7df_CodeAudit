// 注册页（14号 §3.2 / ADR-205）：公开路由；注册即登录（后端返回令牌对）。
// 邀请码制为默认策略（configs auth.registration_mode）——字段保留，disabled 态由后端
// FAILED_PRECONDITION 语义如实呈现，不前端臆测开关状态。
import { Alert, Card, Form, Input, Button, Typography } from 'antd';
import { Link, useNavigate } from 'react-router-dom';
import { useSession } from '../auth/session';
import { useState } from 'react';

interface ApiErrShape {
  response?: { status?: number; data?: { error?: string } };
}

export default function RegisterPage() {
  const { register } = useSession();
  const navigate = useNavigate();
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  return (
    <div style={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '100vh', background: '#f0f2f5' }}>
      <Card style={{ width: 400 }} title={<Typography.Title level={4} style={{ margin: 0 }}>注册 CodeAudit 账号</Typography.Title>}>
        {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} showIcon />}
        <Form
          layout="vertical"
          onFinish={async ({ username, email, password, invite_code }) => {
            setLoading(true);
            setError(null);
            try {
              await register(username, email, password, invite_code ?? '');
              navigate('/projects');
            } catch (e) {
              const err = e as ApiErrShape;
              const detail = err.response?.data?.error;
              if (detail?.includes('invite code') || detail?.includes('disabled')) {
                setError('注册失败：邀请码无效或注册未开放');
              } else if (err.response?.status === 409) {
                setError('注册失败：用户名或邮箱已被使用');
              } else if (err.response?.status === 400) {
                setError(`注册失败：${detail ?? '字段格式不通过'}`);
              } else {
                setError('注册失败：服务不可用');
              }
            } finally {
              setLoading(false);
            }
          }}
        >
          <Form.Item
            name="username"
            label="用户名"
            rules={[
              { required: true, message: '请输入用户名' },
              { pattern: /^[a-zA-Z0-9_-]{3,32}$/, message: '3-32 位字母/数字/下划线/短横线' },
            ]}
          >
            <Input autoFocus autoComplete="username" />
          </Form.Item>
          <Form.Item
            name="email"
            label="邮箱"
            rules={[
              { required: true, message: '请输入邮箱' },
              { type: 'email', message: '邮箱格式不正确' },
            ]}
          >
            <Input autoComplete="email" />
          </Form.Item>
          <Form.Item
            name="password"
            label="密码"
            extra="至少 8 位，须同时包含字母与数字（07 §账号安全）"
            rules={[
              { required: true, message: '请输入密码' },
              { min: 8, message: '至少 8 位' },
              {
                validator: (_, value: string) =>
                  !value || /[a-zA-Z]/.test(value) && /\d/.test(value)
                    ? Promise.resolve()
                    : Promise.reject(new Error('须同时包含字母与数字')),
              },
            ]}
          >
            <Input.Password autoComplete="new-password" />
          </Form.Item>
          <Form.Item
            name="confirm"
            label="确认密码"
            dependencies={['password']}
            rules={[
              { required: true, message: '请再次输入密码' },
              ({ getFieldValue }) => ({
                validator: (_, value: string) =>
                  !value || value === getFieldValue('password')
                    ? Promise.resolve()
                    : Promise.reject(new Error('两次输入不一致')),
              }),
            ]}
          >
            <Input.Password autoComplete="new-password" />
          </Form.Item>
          <Form.Item name="invite_code" label="邀请码" extra="开放注册模式下可留空">
            <Input autoComplete="off" />
          </Form.Item>
          <Button type="primary" htmlType="submit" block loading={loading}>
            注册并登录
          </Button>
          <div style={{ marginTop: 12, textAlign: 'center' }}>
            <Link to="/login">已有账号？去登录</Link>
          </div>
        </Form>
      </Card>
    </div>
  );
}
