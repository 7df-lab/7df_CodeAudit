// 路由级页面标题（2026-09-13）：各页挂载/标题变化时同步 document.title——
// 此前全站恒为 index.html 的"CodeAudit 控制台"，多标签页无法区分。离开时复位基名。
import { useEffect } from 'react';

const BASE_TITLE = 'CodeAudit 控制台';

export function usePageTitle(title?: string) {
  useEffect(() => {
    document.title = title ? `${title} · ${BASE_TITLE}` : BASE_TITLE;
    return () => { document.title = BASE_TITLE; };
  }, [title]);
}
