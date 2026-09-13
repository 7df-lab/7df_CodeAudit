// 首登/重置后强制改密页（14号 §3.2 / ADR-205）：must_change_password=true 时 Shell 强制跳转至此。
// POST /v1/users/me/password —— user_id 由网关从 JWT 注入（self），前端不传。
import { Alert, Form, Input, Button, Typography, message } from 'antd';
import { useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import { useSession } from '../auth/session';
import { useState } from 'react';
import { usePageTitle } from '../hooks/usePageTitle';
import AuthLayout from '../components/AuthLayout';

export default function ChangePasswordPage() {
  usePageTitle('修改密码');
  const { refreshUser, logout } = useSession();
  const navigate = useNavigate();
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  return (
    <AuthLayout title="修改密码" subtitle="当前账号为临时密码或首次登录，必须设置新密码后才能继续使用。" fullscreen={false}>
        {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} showIcon />}
        <Form
          layout="vertical"
          onFinish={async ({ old_password, new_password }) => {
            setLoading(true);
            setError(null);
            try {
              await api.post('/v1/users/me/password', { old_password, new_password });
            } catch {
              setError('修改失败：旧密码不正确或新密码不满足要求（至少 8 位，含字母与数字）');
              setLoading(false);
              return;
            }
            // (P3-j)：改密请求成功后 refreshUser（GET /v1/users/me）失败不再落同一个
            // catch——密码已改成功，误报"旧密码不正确"会引导用户拿旧密码重试必败。
            // 复审 R：放行 /projects 也不行——旧 user 缓存仍 must_change_password，守卫弹回
            // 改密页且表单已清空；改走登出（清 user/缓存）落登录页，以新密码重登自愈。
            try {
              await refreshUser(); // must_change_password 已清除，Shell 放行
            } catch {
              message.warning('密码已修改成功，但会话状态刷新失败——请用新密码重新登录');
              await logout();
              navigate('/login');
              return;
            }
            setLoading(false);
            navigate('/projects');
          }}
        >
          <Form.Item name="old_password" label="当前密码" rules={[{ required: true, message: '请输入当前密码' }]}>
            <Input.Password autoComplete="current-password" />
          </Form.Item>
          <Form.Item
            name="new_password"
            label="新密码"
            extra="至少 8 位，须同时包含字母与数字"
            rules={[
              { required: true, message: '请输入新密码' },
              { min: 8, message: '至少 8 位' },
              {
                validator: (_, value: string) =>
                  !value || (/[a-zA-Z]/.test(value) && /\d/.test(value))
                    ? Promise.resolve()
                    : Promise.reject(new Error('须同时包含字母与数字')),
              },
            ]}
          >
            <Input.Password autoComplete="new-password" />
          </Form.Item>
          <Form.Item
            name="confirm"
            label="确认新密码"
            dependencies={['new_password']}
            rules={[
              { required: true, message: '请再次输入新密码' },
              ({ getFieldValue }) => ({
                validator: (_, value: string) =>
                  !value || value === getFieldValue('new_password')
                    ? Promise.resolve()
                    : Promise.reject(new Error('两次输入不一致')),
              }),
            ]}
          >
            <Input.Password autoComplete="new-password" />
          </Form.Item>
          <Button type="primary" htmlType="submit" block loading={loading}>
            确认修改
          </Button>
        </Form>
    </AuthLayout>
  );
}
