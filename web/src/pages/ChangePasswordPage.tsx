// 首登/重置后强制改密页（14号 §3.2 / ADR-205）：must_change_password=true 时 Shell 强制跳转至此。
// POST /v1/users/me/password —— user_id 由网关从 JWT 注入（self），前端不传。
import { Alert, Card, Form, Input, Button, Typography } from 'antd';
import { useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import { useSession } from '../auth/session';
import { useState } from 'react';

export default function ChangePasswordPage() {
  const { refreshUser } = useSession();
  const navigate = useNavigate();
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  return (
    <div style={{ display: 'flex', justifyContent: 'center', paddingTop: 60 }}>
      <Card style={{ width: 420 }} title={<Typography.Title level={4} style={{ margin: 0 }}>修改密码</Typography.Title>}>
        <Typography.Paragraph type="secondary">
          当前账号为临时密码或首次登录，必须设置新密码后才能继续使用。
        </Typography.Paragraph>
        {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} showIcon />}
        <Form
          layout="vertical"
          onFinish={async ({ old_password, new_password }) => {
            setLoading(true);
            setError(null);
            try {
              await api.post('/v1/users/me/password', { old_password, new_password });
              await refreshUser(); // must_change_password 已清除，Shell 放行
              navigate('/projects');
            } catch {
              setError('修改失败：旧密码不正确或新密码不满足要求（至少 8 位，含字母与数字）');
            } finally {
              setLoading(false);
            }
          }}
        >
          <Form.Item name="old_password" label="当前密码" rules={[{ required: true, message: '请输入当前密码' }]}>
            <Input.Password autoComplete="current-password" />
          </Form.Item>
          <Form.Item
            name="new_password"
            label="新密码"
            extra="至少 8 位，须同时包含字母与数字（07 §账号安全）"
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
      </Card>
    </div>
  );
}
