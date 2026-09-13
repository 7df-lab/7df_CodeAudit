// 发现详情（14号 §3.3 ④，P0 triage 工作台）：字段全部溯源 proto UnifiedFinding

// 路由薄壳（/findings/:fid 深链用）：功能实体在 FindingDetailBody（可被任务详情/列表内嵌复用）
import FindingDetailBody from './FindingDetailBody';

export default function FindingDetailPage({ findingId }: { findingId: string }) {
  return <FindingDetailBody findingId={findingId} />;
}
