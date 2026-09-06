// 任务创建向导（14号 §3.3 ①；04 §3 五模式分流）
// Step4 创建 → POST /v1/tasks（网关生成幂等键）；project_path 经 config map 传递
// （proto L1098 config；Q4 裁决：V1 为网关宿主机路径，限制在 UI 如实标注）
import { useMutation, useQuery } from '@tanstack/react-query';
import { Alert, Button, Card, Checkbox, Form, Input, Radio, Select, Steps, Typography, Upload, message } from 'antd';
import type { UploadFile } from 'antd';
import { autoRunTask } from '../../tasks/stateMachine';
import { UploadOutlined } from '@ant-design/icons';
import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { createTask, getProject, getProjects, getTools, uploadArchive } from '../../api/client';
import { SCAN_MODE, REVIEW_DEPTH, zh } from '../../dict';

// ADR-186 五模式矩阵（人类决策 2026-09-03）：每模式需要的参数分支（向导分支覆盖的单一来源）
// A=纯SAST多工具并行 / B=纯AI / C=SAST+AI并行融合（默认推荐） / D=AI增强SAST / E=SAST+AI并行对比
export interface ModeSpec {
  needsSastTools: boolean;
  needsReviewConfig: boolean;
  blurb: string;
  deprecated?: boolean; // ADR-182 弃用模式：不进入新建入口，历史任务展示仍可用
}
export const MODE_SPECS: Record<string, ModeSpec> = {
  SCAN_MODE_SAST_ONLY: { needsSastTools: true, needsReviewConfig: false, blurb: '多个 SAST 工具并行审计，结果去重后合并产出' },
  SCAN_MODE_AI_ONLY: { needsSastTools: false, needsReviewConfig: false, blurb: '纯 AI 语义审计（沙箱 DSH；不可用时走降级链并如实标注）' },
  SCAN_MODE_PARALLEL: { needsSastTools: true, needsReviewConfig: false, blurb: '★推荐（默认）：SAST 工具与 AI 并行审计，结果融合去重后输出单一清单' },
  SCAN_MODE_AI_ENHANCED_SAST: { needsSastTools: true, needsReviewConfig: false, blurb: 'SAST 扫描发现先按同文件同段去重，再逐条交 DSH 沙箱验证真伪，SAST+AI 判定汇总后融合出报告' },
  SCAN_MODE_COMPARE: { needsSastTools: true, needsReviewConfig: false, blurb: 'SAST 与 AI 并行各自完成，按 单SAST / 单AI / SAST+AI 三类同维度对比（ADR-186 前称"模式D"）' },
  SCAN_MODE_TRADITIONAL_FIRST: { needsSastTools: true, needsReviewConfig: false, deprecated: true, blurb: '【已弃用】SAST→AI 逐条增强验证（历史兼容）' },
  SCAN_MODE_SAST_REVIEW: { needsSastTools: true, needsReviewConfig: true, deprecated: true, blurb: '【已弃用】SAST 结果交 AI 审核（历史兼容）' },
};
// ADR-182 默认推荐模式C（ADR-186 五模式下维持不变）
export const DEFAULT_SCAN_MODE = 'SCAN_MODE_PARALLEL';

export default function TaskNewPage() {
  const navigate = useNavigate();
  const [step, setStep] = useState(0);
  const [projectId, setProjectId] = useState<string>('');
  const [mode, setMode] = useState<string>(DEFAULT_SCAN_MODE); // ADR-182: 默认推荐模式C
  // 人类指令 2026-09-01：创建后默认自动执行（提交→批准→启动）；勾掉则走人工门
  const [autoStart, setAutoStart] = useState<boolean>(true);
  // ADR-154: 第2步 Form 在 setStep(3) 时卸载、字段注销，确认页 validateFields() 只能取到空对象
  // （GUI 实测 POST body 为 sast_tools:[]/config:{} → 任务必然失败）。参数在此暂存，确认页消费。
  const [uploadFileId, setUploadFileId] = useState<string>('');
  // ADR-202: 受控 fileList——第2步 Form 卸载重建（上一步/确认页往返）后上传件展示不丢，
  // 且与 uploadFileId 单一状态源，杜绝"输入框置灰但列表无文件"的矛盾呈现
  const [uploadList, setUploadList] = useState<UploadFile[]>([]);
  const [params, setParams] = useState<{
    project_path?: string;
    sast_tools?: string[];
    review_depth?: string;
    review_opts?: string[];
  }>({});
  const [form] = Form.useForm();

  // ADR-203: 响应形状经 client.ts 类型化端点锚定（不再页面内 as-cast）
  const { data: projects } = useQuery({
    queryKey: ['projects'],
    queryFn: () => getProjects(),
  });
  // ADR-163: 仓库模式——项目配置 repo_url 且未上传/未手填路径时，启动时后端自动 clone
  // ADR-203 补遗: 原 ADR-148"项目 config project_path 预填"随该遗留档退役移除——
  // 项目源码来源只剩 upload_file_id/repo_url 两档，任务未上传时手填路径仍为任务级兜底
  const { data: projInfo } = useQuery({
    queryKey: ['project', projectId],
    queryFn: () => getProject(projectId),
    enabled: !!projectId,
  });
  const repoURL = projInfo?.repo_url ?? '';
  const repoMode = !!repoURL;
  const { data: tools, isLoading: toolsLoading } = useQuery({
    queryKey: ['tools'],
    queryFn: getTools,
    enabled: MODE_SPECS[mode]?.needsSastTools === true, // 14号 §3.3 ①：仅需工具的模式才探测
  });

  const create = useMutation({
    mutationFn: async (values: { project_path?: string; sast_tools?: string[]; review_depth?: string; review_opts?: string[] }) => {
      const config: Record<string, string> = {};
      // ADR-200: storage 上传件优先（手填路径仅作未上传时的兜底）
      if (uploadFileId) {
        config.upload_file_id = uploadFileId;
      } else if (values.project_path) {
        config.project_path = values.project_path;
      }
      if (values.review_depth) config.review_depth = values.review_depth;
      if (values.review_opts?.length) {
        config.assess_severity = String(values.review_opts.includes('assess_severity'));
        config.verify_location = String(values.review_opts.includes('verify_location'));
        config.generate_suggestions = String(values.review_opts.includes('generate_suggestions'));
      }
      return createTask({
        project_id: projectId,
        scan_mode: mode,
        sast_tools: MODE_SPECS[mode]?.needsSastTools ? values.sast_tools ?? [] : [],
        config,
      });
    },
    onSuccess: (resp) => {
      const tid = resp.task_id;
      if (autoStart) {
        message.success('任务已创建，正在自动启动（提交→批准→启动）…');
        autoRunTask(tid).then(() => {
          message.success('扫描任务已自动启动');
        }).catch((e) => {
          message.warning(`自动启动失败（${(e as Error).message}），可在任务页手动续走`);
        });
      } else {
        message.success('任务已创建');
      }
      navigate(`/tasks/${tid}`);
    },
    // ADR-154: 此前创建失败静默（无 onError），用户停在确认页无任何反馈
    onError: (e) => message.error(`任务创建失败：${(e as Error).message}`),
  });

  const spec = MODE_SPECS[mode];
  const executableTools = (tools?.tools ?? []).filter((t) => t.valid);
  const unusableTools = (tools?.tools ?? []).filter((t) => !t.valid);

  const stepContent: Record<number, React.ReactNode> = {
    0: (
      <Select
        style={{ width: 420 }}
        placeholder="选择项目"
        value={projectId || undefined}
        onChange={(v) => setProjectId(v)}
        options={(projects?.projects ?? []).map((p) => ({ value: p.project_id, label: `${p.name} (${p.project_id})` }))}
      />
    ),
    1: (
      <Radio.Group
        value={mode}
        onChange={(e) => setMode(e.target.value)}
        options={Object.entries(SCAN_MODE)
          .filter(([value]) => !MODE_SPECS[value]?.deprecated) // ADR-182: 弃用模式不进新建入口
          .map(([value, label]) => ({ value, label: `${label} —— ${MODE_SPECS[value]?.blurb ?? ''}` }))}
      />
    ),
    2: (
      <Form form={form} layout="vertical" style={{ maxWidth: 560 }} onFinish={(v) => { setParams(v); setStep(3); }}>
        {spec?.needsSastTools && (
          <>
            <Form.Item
              name="sast_tools"
              label="SAST 工具（仅列可执行工具）"
              rules={[{ required: true, message: '至少选择一个工具' }]}
            >
              <Select
                mode="multiple"
                loading={toolsLoading}
                options={executableTools.map((t) => ({ value: t.tool_id, label: `${t.tool_id}（${t.output_format}）` }))}
              />
            </Form.Item>
            {unusableTools.length > 0 && (
              <Alert
                type="warning"
                showIcon
                style={{ marginBottom: 16 }}
                message={`以下工具解析器就绪但执行未接入，不可选：${unusableTools.map((t) => t.tool_id).join('、')}`}
              />
            )}
          </>
        )}
        <Form.Item label="上传代码压缩包（推荐）">
          <Upload
            maxCount={1}
            fileList={uploadList}
            accept=".zip,.tgz,.tar.gz"
            beforeUpload={async (file) => {
              try {
                const res = await uploadArchive(file);
                setUploadFileId(res.file_id); // ADR-200: file_id → config.upload_file_id（task 从 storage 拉回解包）
                setUploadList([{ uid: res.file_id, name: file.name, status: 'done' }]);
                message.success(`已上传至存储（${(res.size_bytes / 1024).toFixed(1)} KB），启动时自动解包`);
              } catch {
                message.error('上传失败（仅支持 zip/tar.gz，≤25MB）');
              }
              return false; // 阻止 antd 默认上传
            }}
            // ADR-202: 移除已上传件必须同步清 file_id，否则任务仍按 storage 通道创建（路径模式失效）
            onRemove={() => { setUploadFileId(''); setUploadList([]); }}
          >
            <Button icon={<UploadOutlined />}>选择 zip / tar.gz（≤25MB）</Button>
          </Upload>
        </Form.Item>
        <Form.Item
          name="project_path"
          label={repoMode
            ? "扫描路径（可选：留空则启动时自动拉取仓库）"
            : uploadFileId
              ? "扫描路径（已上传压缩包，无需填写）"
              : "扫描路径（网关宿主机路径；或直接上传压缩包免填）"}
          extra={repoMode
            ? `仓库模式：未上传/未填路径时，启动时自动 clone ${repoURL}`
            : uploadFileId
              ? "启动时 task 从 storage 拉回压缩包自动解包，解包目录即扫描目标（ADR-200）"
              : "上传解包目录或手填路径均可直接扫描；仓库项目留空路径则启动时自动 clone"}
          rules={repoMode || !!uploadFileId ? [] : [{ required: true }]} // ADR-202: 上传件优先（ADR-200 手填路径降为兜底），传包后路径免填
        >
          {/* ADR-202: 上传件优先于手填路径——置灰明示，避免"填了却被静默忽略" */}
          <Input placeholder="/path/to/project" disabled={!!uploadFileId} />
        </Form.Item>
        {spec?.needsReviewConfig && (
          <>
            <Form.Item name="review_depth" label="审核深度（ReviewConfig.depth）" initialValue="REVIEW_DEPTH_STANDARD">
              <Select options={Object.entries(REVIEW_DEPTH).map(([value, label]) => ({ value, label }))} />
            </Form.Item>
            <Form.Item name="review_opts" label="审核选项">
              <Checkbox.Group
                options={[
                  { value: 'assess_severity', label: '评估严重级别' },
                  { value: 'verify_location', label: '校验定位' },
                  { value: 'generate_suggestions', label: '生成修复建议' },
                ]}
              />
            </Form.Item>
          </>
        )}
        <Button type="primary" htmlType="submit">
          下一步：确认
        </Button>
      </Form>
    ),
    3: (
      <Card style={{ maxWidth: 560 }}>
        <Typography.Paragraph>
          项目 <b>{projectId}</b> ｜ 模式 <b>{zh(SCAN_MODE, mode)}</b>
        </Typography.Paragraph>
        {/* ADR-154: 回显第2步参数（此前确认页不可见工具/路径，参数静默丢失无任何提示） */}
        <Typography.Paragraph type="secondary" style={{ marginBottom: 4 }}>
          {spec?.needsSastTools && (
            <>
              SAST 工具：<b>{(params.sast_tools ?? []).join('、') || '—'}</b>
              <br />
            </>
          )}
          扫描路径：<b>{uploadFileId ? '已上传存储（启动时从 storage 拉回解包）' : params.project_path || (repoMode ? `仓库自动拉取（${repoURL}）` : '—')}</b>
          {spec?.needsReviewConfig && (
            <>
              <br />
              审核深度：<b>{params.review_depth ? zh(REVIEW_DEPTH, params.review_depth) : '—'}</b>
            </>
          )}
        </Typography.Paragraph>
        <Typography.Paragraph style={{ marginTop: 12 }}>
          <Checkbox
            checked={autoStart}
            onChange={(e: { target: { checked: boolean } }) => setAutoStart(e.target.checked)}
          >
            创建后立即启动（不勾则停在待启动，需在详情页手动点启动）
          </Checkbox>
        </Typography.Paragraph>
        <Button
          type="primary"
          loading={create.isPending}
          onClick={() => create.mutate(params)}
        >
          创建任务
        </Button>
      </Card>
    ),
  };

  return (
    <div style={{ maxWidth: 720 }}>
      <Typography.Title level={3}>新建扫描任务</Typography.Title>
      <Steps
        items={[{ title: '项目' }, { title: '模式' }, { title: '参数' }, { title: '确认' }]}
        current={step}
        style={{ marginBottom: 24 }}
      />
      {stepContent[step]}
      <div style={{ marginTop: 16 }}>
        {step > 0 && <Button style={{ marginRight: 8 }} onClick={() => setStep(step - 1)}>上一步</Button>}
        {step === 0 && <Button type="primary" disabled={!projectId} onClick={() => setStep(1)}>下一步</Button>}
        {step === 1 && <Button type="primary" disabled={!mode} onClick={() => setStep(2)}>下一步</Button>}
      </div>
    </div>
  );
}
