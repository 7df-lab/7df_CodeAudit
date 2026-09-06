// 统一错误组件（14号 §3.5 统一 UX 约定）：
//   401=静默刷新/跳登录（client.ts 拦截器, 不在本套件）；403=权限不足页；
//   404=空态；501=「能力未接入」专用组件（灰卡+说明）；503=降级横幅（自动重试 3 次退避，
//   重试逻辑在 client.ts, 此处为重试耗尽后的提示条）。
import { Alert, Button, Card, Result, Typography } from 'antd';
import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { API_ERROR_EVENT, API_OK_EVENT, type ApiErrorCode } from '../api/apiEvents';

// 路由级错误页：404 兜底路由 / 显式引用
export function ErrorPage({ code }: { code: 403 | 404 | 501 }) {
  const navigate = useNavigate();
  if (code === 403) {
    return (
      <Result
        status="403"
        title="权限不足"
        subTitle="当前账号无权访问该资源或执行该操作。"
        extra={<Button type="primary" onClick={() => navigate('/projects')}>返回项目页</Button>}
      />
    );
  }
  if (code === 501) {
    // 「能力未接入」灰卡（14号 §3.5）：诚实降级说明, 非故障
    return (
      <div style={{ display: 'flex', justifyContent: 'center', paddingTop: 80 }}>
        <Card style={{ width: 560, background: '#fafafa', border: '1px dashed #d9d9d9' }} title="能力未接入（501）">
          <Typography.Text type="secondary">
            该能力在当前版本尚未实现——这是诚实的降级声明而非故障。
            对应设计见仓库 14 号《展现层设计》§3.5 与各服务设计文档。
          </Typography.Text>
          <div style={{ marginTop: 16 }}>
            <Button onClick={() => navigate('/projects')}>返回项目页</Button>
          </div>
        </Card>
      </div>
    );
  }
  return (
    <Result
      status="404"
      title="404"
      subTitle="页面不存在或已被移除。"
      extra={<Button type="primary" onClick={() => navigate('/projects')}>返回项目页</Button>}
    />
  );
}

// 全局挂载于 Shell：403/501 → 整页错误组件（可返回）；503 → 顶部降级横幅；
// 任意请求成功即撤横幅（服务恢复的即时反馈）。
export function ApiErrorOverlay() {
  const [pageCode, setPageCode] = useState<403 | 501 | null>(null);
  const [degraded, setDegraded] = useState(false);
  const navigate = useNavigate();

  useEffect(() => {
    const onError = (e: Event) => {
      const code = (e as CustomEvent<ApiErrorCode>).detail;
      if (code === 403 || code === 501) setPageCode(code);
      if (code === 503) setDegraded(true);
    };
    const onOk = () => setDegraded(false);
    window.addEventListener(API_ERROR_EVENT, onError);
    window.addEventListener(API_OK_EVENT, onOk);
    return () => {
      window.removeEventListener(API_ERROR_EVENT, onError);
      window.removeEventListener(API_OK_EVENT, onOk);
    };
  }, []);

  return (
    <>
      {degraded && (
        <Alert
          banner
          type="warning"
          showIcon
          message="部分服务暂不可用（已自动重试 3 次仍未恢复）——数据可能不完整，稍后操作将自动恢复。"
          closable
          onClose={() => setDegraded(false)}
        />
      )}
      {pageCode && (
        <div style={{ position: 'fixed', inset: 0, background: '#fff', zIndex: 1000, paddingTop: 60 }}>
          <ErrorPage code={pageCode} />
          <div style={{ textAlign: 'center' }}>
            <Button type="link" onClick={() => { setPageCode(null); navigate('/projects'); }}>
              关闭并返回项目页
            </Button>
          </div>
        </div>
      )}
    </>
  );
}
