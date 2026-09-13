// Source→Sink 证据链（ 签名元素，2026-09-13）：污点传播的一眼主视图。
// 数据优先级由调用方决定（FindingDetailBody：OpenGrep dataflow_trace 变量级链 >
// AI 结论解析链）；source=青（入口）、中间 hop=灰、sink=红（危险点）——与全站
// "红=危险"对齐。chip 等宽（file:line 是机器产物=证据嗓音），可点选定位代码行。
// 少于 2 步不构成"链"，不渲染（不为单点造流程感）。
import { ArrowRightOutlined } from '@ant-design/icons';
import { Tag, Tooltip } from 'antd';
import { MONO_FONT } from '../dict/tokens';

export interface ChainStep {
  path: string;
  line?: number;
  endLine?: number;
  content?: string;
  role?: 'source' | 'sink';
}

function baseName(p: string): string {
  return p.split('/').pop() ?? p;
}

export default function EvidenceChain(
  { steps, onSelect, activePath, activeLine }: {
    steps: ChainStep[];
    onSelect?: (s: ChainStep) => void;
    activePath?: string;
    activeLine?: number;
  },
) {
  if (steps.length < 2) return null;
  return (
    <div style={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: 6 }} data-testid="evidence-chain">
      {steps.map((s, i) => {
        const isFirst = i === 0;
        const isLast = i === steps.length - 1;
        const color = isFirst ? 'cyan' : isLast ? 'red' : 'default';
        const label = `${isFirst ? '源 ' : isLast ? '汇 ' : ''}${baseName(s.path)}${s.line ? `:${s.line}` : ''}`;
        const active = activePath === s.path && activeLine === s.line;
        return (
          // 箭头在两步之间（首项之前无箭头）——链的方向即阅读方向
          <span key={`${s.path}:${s.line}-${i}`} style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
            {i > 0 && <ArrowRightOutlined style={{ color: '#8c8c8c', fontSize: 12 }} />}
            <Tooltip title={s.content ? `${s.path}${s.line ? `:${s.line}` : ''}\n${s.content.slice(0, 120)}` : `${s.path}${s.line ? `:${s.line}` : ''}`}>
              <Tag
                color={color}
                style={{ fontFamily: MONO_FONT, marginInlineEnd: 0, cursor: onSelect ? 'pointer' : 'default', borderWidth: active ? 2 : 1 }}
                onClick={onSelect ? () => onSelect(s) : undefined}
              >
                {label}
              </Tag>
            </Tooltip>
          </span>
        );
      })}
    </div>
  );
}
