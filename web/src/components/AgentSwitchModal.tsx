import { useMemo, useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { Alert, Modal, Radio, Tag, Typography, message } from 'antd';
import { api, apiErrorMessage, errStatus, listAgents, switchTaskAgent } from '../api/client';
import type { AgentProviderInfo } from '../api/types';

// 切换 Agent Modal（TP20-T2 切换 Agent 收口；主仓库计划 §3.1/§3.2）。
// 数据源=GET /v1/agents（与创建页同源，硬编码清单退役）；当前 agent 项禁用+「当前」
// 徽标；每项能力摘要四行如实（来自 capabilities，未声明=false）；端点失败=整体禁用
// +如实错误——不回落硬编码（计划 §0 降级裁决）。
// 术语：用户可见面统一「切换 Agent」；pen/锚/简报为内部术语不外泄（计划 §0）。

export type SwitchMode = 'pause-switch' | 'switch'; // RUNNING=暂停并切换；PAUSED/QUEUED=切换

export function capabilitySummary(a: AgentProviderInfo): string[] {
  // §3.2 能力摘要：四行如实；结构化提交=无 时标注兜底通道（降级来源可预期）
  return [
    `工具事件：${a.tool_events ? '有' : '无'}`,
    `结构化提交：${a.structured_submit ? '有' : '无（围栏 JSON 兜底）'}`,
    `交互：${a.interaction ? '有' : '无'}`,
    `会话恢复：${a.session_resume ? '有' : '无'}`,
  ];
}

export function AgentSwitchModal(props: {
  open: boolean;
  taskId: string;
  mode: SwitchMode;
  currentAgent: string; // 空=部署默认（is_default 条目）
  onClose: () => void;
  onDone: () => void; // 成功后失效刷新（任务快照/日志/时间线）
}) {
  const { open, taskId, mode, currentAgent, onClose, onDone } = props;
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['agents'],
    queryFn: listAgents,
    staleTime: 60_000,
    enabled: open,
  });
  const agents = useMemo(() => data?.agents ?? [], [data]);

  // 当前项判定：任务快照 agent 空=部署默认=is_default 条目
  const currentId = currentAgent || agents.find((a) => a.is_default)?.id || '';
  const [selected, setSelected] = useState('');
  const value = selected || agents.find((a) => a.id !== currentId)?.id || '';

  const confirm = useMutation({
    mutationFn: async (agent: string) => {
      if (mode === 'pause-switch') {
        // RUNNING：回合边界暂停+换手（PauseTask?agent=——agent_to 由引擎写 AI marker）
        return (await api.post(`/v1/tasks/${encodeURIComponent(taskId)}/pause?agent=${encodeURIComponent(agent)}`)).data;
      }
      return switchTaskAgent(taskId, agent);
    },
    onSuccess: () => {
      message.success(mode === 'pause-switch' ? '已提交暂停并切换，回合边界生效' : '切换已提交');
      setSelected('');
      onClose();
      onDone();
    },
    onError: (e) =>
      message.error(`切换被拒绝${errStatus(e) ? `（HTTP ${errStatus(e)}）` : ''}：${apiErrorMessage(e)}`),
  });

  const title = mode === 'pause-switch' ? '暂停并切换 Agent' : '切换 Agent';
  return (
    <Modal
      open={open}
      title={title}
      okText="确认切换"
      okButtonProps={{ disabled: isError || agents.length === 0, loading: confirm.isPending }}
      onOk={() => value && confirm.mutate(value)}
      onCancel={() => { setSelected(''); onClose(); }}
      destroyOnClose
    >
      {isError ? (
        <Alert
          type="error"
          showIcon
          message="Agent 清单加载失败"
          description={`${apiErrorMessage(error)}——切换入口不可用（不回落内置清单）。请稍后重试或检查引擎服务。`}
        />
      ) : (
        <>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 12 }}>
            {mode === 'pause-switch'
              ? '任务将在当前回合边界暂停，切换后由新 Agent 从现有进度接续（工作区与已提交发现保留）'
              : '切换后由新 Agent 从现有进度接续（工作区与已提交发现保留）'}
          </Typography.Paragraph>
          <Radio.Group
            value={value}
            onChange={(e) => setSelected(e.target.value)}
            disabled={isLoading || isError}
            style={{ display: 'grid', gap: 10, width: '100%' }}
          >
            {agents.map((a) => {
              const isCurrent = a.id === currentId;
              return (
                <Radio key={a.id} value={a.id} disabled={isCurrent} style={{ marginInlineEnd: 0, width: '100%' }}>
                  <div style={{ display: 'inline-flex', gap: 8, alignItems: 'baseline' }}>
                    <span style={{ fontWeight: 600 }}>{a.id}</span>
                    {a.is_default && <Tag style={{ marginInlineEnd: 0 }}>默认</Tag>}
                    {isCurrent && <Tag style={{ marginInlineEnd: 0 }} color="blue">当前</Tag>}
                    <span style={{ fontSize: 12, opacity: 0.55 }}>{a.dialect}</span>
                  </div>
                  <div style={{ fontSize: 12, opacity: 0.75, whiteSpace: 'pre-line', paddingLeft: 24 }}>
                    {capabilitySummary(a).join('\n')}
                  </div>
                </Radio>
              );
            })}
          </Radio.Group>
        </>
      )}
    </Modal>
  );
}
