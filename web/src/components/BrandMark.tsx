// 品牌标识（2026-09-13）：靛墨圆角方块 + 等宽白 </>（代码审计的载体）+
// 右上角绛红 severity 点（产品最重要的语义色——发现"严重"级）。用于 Header 品牌区/
// 认证页品牌块；favicon 为同构图（public/favicon.svg）。
export default function BrandMark({ size = 32 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 64 64" aria-hidden="true" role="img">
      <rect x="4" y="4" width="56" height="56" rx="12" fill="#3056D3" />
      <text
        x="29" y="43" textAnchor="middle"
        fontFamily="'JetBrains Mono', 'SFMono-Regular', Menlo, Consolas, monospace"
        fontSize="24" fontWeight="700" fill="#FFFFFF"
      >
        {'</>'}
      </text>
      <circle cx="52" cy="13" r="7" fill="#A8071A" />
      <circle cx="52" cy="13" r="2.8" fill="#FFFFFF" />
    </svg>
  );
}
