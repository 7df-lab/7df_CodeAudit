// 注册页（14号 §3.2 / ADR-205）：公开路由；注册即登录（后端返回令牌对）。
// V2.2 (ADR-255, Q10)：挂载时拉 /v1/auth/registration-policy 随档位自适应——
// invitation=邀请码必填（动态文案，替代曾误导的静态提示）；open=隐藏邀请码栏；
// disabled=表单不渲染+如实说明。拉取失败=维持 invitation 形态（后端仍是最终防线）。
import { Alert, Form, Input, Button } from 'antd';
import { Link, useNavigate } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import { useSession } from '../auth/session';
import { useState } from 'react';
import { usePageTitle } from '../hooks/usePageTitle';
import AuthLayout from '../components/AuthLayout';

interface ApiErrShape {
  response?: { status?: number; data?: { error?: string } };
}

export default function RegisterPage() {
  usePageTitle('注册');
  const { register } = useSession();
  const navigate = useNavigate();
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const { data: policy } = useQuery({
    queryKey: ['registration-policy'],
    staleTime: 60_000,
    retry: false,
    queryFn: async () => {
      const resp = await api.get<{ mode?: string }>('/v1/auth/registration-policy');
      return resp.data;
    },
  });
  const mode = policy?.mode ?? 'invitation'; // 失败/未达=保守形态（后端 FAILED_PRECONDITION 兜底）

  if (mode === 'disabled') {
    return (
      <AuthLayout title="注册 CodeAudit 账号">
        <Alert type="info" showIcon message="注册未开放" description="当前部署已关闭自助注册，请联系管理员开通账号。" />
        <div style={{ marginTop: 12, textAlign: 'center' }}>
          <Link to="/login">返回登录</Link>
        </div>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title="注册 CodeAudit 账号">
        {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} showIcon />}
        <Form
          layout="vertical"
          onFinish={async ({ username, email, password, invite_code }) => {
            setLoading(true);
            setError(null);
            try {
              await register(username, email, password, invite_code ?? '');
              navigate('/'); // ADR-257: 落总览（全角色可达；此前 /projects 对 VIEWER 死局）
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
            extra="至少 8 位，须同时包含字母与数字"
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
          {mode === 'invitation' && (
            <Form.Item name="invite_code" label="邀请码" extra="请向管理员索取" rules={[{ required: true, message: '请输入邀请码' }]}>
              <Input autoComplete="off" />
            </Form.Item>
          )}
          <Button type="primary" htmlType="submit" block loading={loading}>
            注册并登录
          </Button>
          <div style={{ marginTop: 12, textAlign: 'center' }}>
            <Link to="/login">已有账号？去登录</Link>
          </div>
        </Form>
    </AuthLayout>
  );
}
