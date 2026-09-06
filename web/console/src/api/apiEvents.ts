// API 错误事件总线常量（零依赖：client.ts 拦截器派发, components/errors.tsx 监听）
// 14号 §3.5: 403=权限不足页 / 501=能力未接入 / 503=降级横幅（重试耗尽后）
export const API_ERROR_EVENT = 'auditmind:api-error';
export const API_OK_EVENT = 'auditmind:api-ok';
export type ApiErrorCode = 403 | 404 | 501 | 503;
