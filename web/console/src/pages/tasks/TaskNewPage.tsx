// 任务创建向导（14号 §3.3 ①；04 §3 四模式分流）
// Step4 创建 → POST /v1/tasks（网关生成幂等键）；project_path 经 config map 传递
// （proto L1098 config；Q4 裁决：V1 为网关宿主机路径，限制在 UI 如实标注）
import { useMutation, useQuery } from '@tanstack/react-query';
import { Alert, Button, Card, Checkbox, Form, Input, Radio, Select, Steps, Typography, Upload, message } from 'antd';
import { autoRunTask } from '../../tasks/stateMachine';
import { UploadOutlined } from '@ant-design/icons';
import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api, uploadArchive } from '../../api/client';
import type { Project, ToolInfo } from '../../api/types';
import { SCAN_MODE, REVIEW_DEPTH, zh } from '../../dict';

// 04 §3 模式定义：每模式需要的参数分支（向导分支覆盖的单一来源）
export interface ModeSpec {
  needsSastTools: boolean;
  needsReviewConfig: boolean;
  blurb: string;
}
export const MODE_SPECS: Record<string, ModeSpec> = {
  SCAN_MODE_AI_ONLY: { needsSastTools: false, needsReviewConfig: false, blurb: 'AI 全流程推理（LLM 未接入时走规则引擎降级）' },
  SCAN_MODE_TRADITIONAL_FIRST: { needsSastTools: true, needsReviewConfig: false, blurb: '★推荐：SAST 扫描后 AI 逐条增强验证' },
  SCAN_MODE_PARALLEL: { needsSastTools: true, needsReviewConfig: false, blurb: 'SAST 与 AI 并行，四象限对比 + F1' },
  SCAN_MODE_SAST_REVIEW: { needsSastTools: true, needsReviewConfig: true, blurb: 'SAST 结果交 AI 逐条审核（约为模式B耗时一半）' },
};

export default function TaskNewPage() {
  const navigate = useNavigate();
  const [step, setStep] = useState(0);
  const [projectId, setProjectId] = useState<string>('');
  const [mode, setMode] = useState<string>('');
  // 人类指令 2026-09-01：创建后默认自动执行（提交→批准→启动）；勾掉则走人工门
  const [autoStart, setAutoStart] = useState<boolean>(true);
  // ADR-154: 第2步 Form 在 setStep(3) 时卸载、字段注销，确认页 validateFields() 只能取到空对象
  // （GUI 实测 POST body 为 sast_tools:[]/config:{} → 任务必然失败）。参数在此暂存，确认页消费。
  const [params, setParams] = useState<{
    project_path?: string;
    sast_tools?: string[];
    review_depth?: string;
    review_opts?: string[];
  }>({});
  const [form] = Form.useForm();

  const { data: projects } = useQuery({
    queryKey: ['projects'],
    queryFn: async () => (await api.get('/v1/projects')).data as { projects: Project[] },
  });
  // ADR-148: 选中项目后从项目 config 预填扫描路径（新建项目上传压缩包的解包目录存于 config）
  const { data: projCfg } = useQuery({
    queryKey: ['project-config', projectId],
    queryFn: async () => (await api.get(`/v1/projects/${projectId}/config`)).data as {
      config: { project_path?: string };
    },
    enabled: !!projectId,
  });
  // ADR-163: 仓库模式——项目配置 repo_url 且未上传/未手填路径时，启动时后端自动 clone
  const { data: projInfo } = useQuery({
    queryKey: ['project', projectId],
    queryFn: async () => (await api.get(`/v1/projects/${projectId}`)).data as {
      project?: { repo_url?: string };
      repo_url?: string;
    },
    enabled: !!projectId,
  });
  const repoURL = projInfo?.project?.repo_url ?? projInfo?.repo_url ?? '';
  const repoMode = !!repoURL;
  useEffect(() => {
    const p = projCfg?.config?.project_path;
    if (p && step >= 2) form.setFieldsValue({ project_path: p });
  }, [projCfg, step, form]);
  const { data: tools, isLoading: toolsLoading } = useQuery({
    queryKey: ['tools'],
    queryFn: async () => (await api.get('/v1/tools')).data as { tools: ToolInfo[] },
    enabled: MODE_SPECS[mode]?.needsSastTools === true, // 14号 §3.3 ①：仅需工具的模式才探测
  });

  const create = useMutation({
    mutationFn: async (values: { project_path?: string; sast_tools?: string[]; review_depth?: string; review_opts?: string[] }) => {
      const config: Record<string, string> = {};
      if (values.project_path) config.project_path = values.project_path;
      if (values.review_depth) config.review_depth = values.review_depth;
      if (values.review_opts?.length) {
        config.assess_severity = String(values.review_opts.includes('assess_severity'));
        config.verify_location = String(values.review_opts.includes('verify_location'));
        config.generate_suggestions = String(values.review_opts.includes('generate_suggestions'));
      }
      return api.post('/v1/tasks', {
        project_id: projectId,
        scan_mode: mode,
        sast_tools: MODE_SPECS[mode]?.needsSastTools ? values.sast_tools ?? [] : [],
        config,
      });
    },
    onSuccess: (resp) => {
      const tid = resp.data.task_id;
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
        options={Object.entries(SCAN_MODE).map(([value, label]) => ({ value, label: `${label} —— ${MODE_SPECS[value]?.blurb ?? ''}` }))}
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
            accept=".zip,.tgz,.tar.gz"
            beforeUpload={async (file) => {
              try {
                const res = await uploadArchive(file);
                message.success(`已上传并解包 ${res.files} 个文件`);
                form.setFieldsValue({ project_path: res.dir });
              } catch {
                message.error('上传失败（仅支持 zip/tar.gz，≤25MB）');
              }
              return false; // 阻止 antd 默认上传
            }}
          >
            <Button icon={<UploadOutlined />}>选择 zip / tar.gz（≤25MB）</Button>
          </Upload>
        </Form.Item>
        <Form.Item
          name="project_path"
          label={repoMode
            ? "扫描路径（可选：留空则启动时自动拉取仓库）"
            : "扫描路径（上传后自动填入；也可手填网关宿主机路径）"}
          extra={repoMode
            ? `仓库模式：未上传/未填路径时，启动时自动 clone ${repoURL}`
            : "上传解包目录或手填路径均可直接扫描；仓库项目留空路径则启动时自动 clone"}
          rules={repoMode ? [] : [{ required: true }]}
        >
          <Input placeholder="/path/to/project" />
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
          扫描路径：<b>{params.project_path || (repoMode ? `仓库自动拉取（${repoURL}）` : '—')}</b>
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
