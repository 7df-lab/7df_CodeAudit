// 统一认证壳（2026-09-13）：登录/注册/改密三页共用。
// 此前三页三种容器（380/400/420 三档定宽、两种对齐方式）、零品牌元素、登录页无注册
// 互链（注册入口全站不可发现）。fullscreen=登录/注册（Shell 之外，整页灰底居中）；
// 改密页在 Shell 内容区内（fullscreen=false，仅居中卡）。
import { Card, Typography } from 'antd';
import type { ReactNode } from 'react';
import BrandMark from './BrandMark';

export default function AuthLayout(
  { title, subtitle, fullscreen = true, children }: {
    title: ReactNode; subtitle?: ReactNode; fullscreen?: boolean; children: ReactNode;
  },
) {
  const brand = (
    <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 24 }}>
      <BrandMark size={40} />
      <div>
        <Typography.Title level={4} style={{ margin: 0 }}>CodeAudit 控制台</Typography.Title>
        <Typography.Text type="secondary">SAST × AI 双引擎代码审计工作台</Typography.Text>
      </div>
    </div>
  );
  const card = (
    <Card style={{ width: '100%', maxWidth: 400 }}>
      <Typography.Title level={5} style={{ marginTop: 0, marginBottom: subtitle ? 4 : 16 }}>{title}</Typography.Title>
      {subtitle && (
        <Typography.Paragraph type="secondary" style={{ marginBottom: 16 }}>{subtitle}</Typography.Paragraph>
      )}
      {children}
    </Card>
  );
  if (!fullscreen) {
    return <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center' }}>{brand}{card}</div>;
  }
  return (
    <div style={{
      minHeight: '100vh', padding: 24,
      display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center',
      background: '#F5F6FA',
    }}>
      {brand}
      {card}
    </div>
  );
}
