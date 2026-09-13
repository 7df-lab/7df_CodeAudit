// 控制台 antd 主题（2026-09-13）：主色换装靛墨 #3056D3（"安全实验室"方向，
// 人类裁决 2026-09-13）——深靛与 severity 暖色阶梯、success/error 保持安全色距；中文优先
// 字体栈。克制的 components token：个性收敛在"证据面"（theme/evidence.ts 碳黑面板），
// 亮色组件区交给 antd 默认体系，只做表头浅靛底的气质接续。
import type { ThemeConfig } from 'antd';

export const UI_FONT =
  "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', 'HarmonyOS Sans SC', 'Microsoft YaHei', 'Noto Sans SC', sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji'";

export const consoleTheme: ThemeConfig = {
  token: {
    colorPrimary: '#3056D3',
    colorInfo: '#3056D3',
    colorLink: '#3056D3',
    fontFamily: UI_FONT,
  },
  components: {
    Table: { headerBg: '#F4F6FB' },
  },
};
