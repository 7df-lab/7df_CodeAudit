// 邀请码面板（V2.2 ADR-255，grill Q1-Q14 定稿）：UsersPage「邀请码」Tab 的主体。
// 生成=Modal（上限/有效期/备注三控件，系统随机码不可自造 Q3）；列=码(等宽+复制 Q12)/
// 状态 Tag（派生态：用尽/过期不入库，此处从数据派生）/剩余次数/过期/创建者/备注/操作；
// 吊销带确认（活跃码唯一下线通道 Q11）；删除仅终态码（Q14 两步走，后端护栏+前端仅
// 终态出按钮双面）；空态「尚无邀请码」（Q7 收敛文案）；默认筛有效可切全部（Q11）；
// 配置注入码只读分区置底（Q4/Q13 bootstrap 层，管理归配置文件）。
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Button, Checkbox, DatePicker, Form, Input, InputNumber, Modal, Popconfirm, Select, Space, Switch, Table, Tag, Tooltip, Typography, message } from 'antd';
import dayjs, { type Dayjs } from 'dayjs';
import { useState } from 'react';
import { apiErrorMessage, createInviteCode, deleteInviteCode, listInviteCodes, revokeInviteCode } from '../../api/client';
import type { CreateInviteCodeBody, InviteCode } from '../../api/types';
import { MONO_FONT } from '../../dict/tokens';

// 派生态口径与后端 repo.InviteTerminal 同源（存储只存 active|revoked，防漂移）
function terminalOf(c: InviteCode): boolean {
  if (c.state === 'revoked') return true;
  if (c.max_uses > 0 && c.used_count >= c.max_uses) return true;
  if (c.expires_at && new Date(c.expires_at).getTime() <= Date.now()) return true;
  return false;
}

function statusTag(c: InviteCode) {
  if (c.state === 'revoked') return <Tag>已吊销</Tag>;
  if (c.max_uses > 0 && c.used_count >= c.max_uses) return <Tag color="default">已用尽</Tag>;
  if (c.expires_at && new Date(c.expires_at).getTime() <= Date.now()) return <Tag color="default">已过期</Tag>;
  return <Tag color="green">有效</Tag>;
}

function fmtTime(v: string | null | undefined): string {
  if (!v) return '—';
  return dayjs(v).format('YYYY-MM-DD HH:mm');
}

export default function InviteCodesPanel() {
  const qc = useQueryClient();
  const [includeTerminal, setIncludeTerminal] = useState(false);

  // 审查批修复：>20 枚码静默截断——沿 UsersPage 无限查询范式（queryKey 含筛选=
  // 切换重置分页；加载更多按钮推进 next_cursor）
  const {
    data, isLoading, isError, error, fetchNextPage, hasNextPage, isFetchingNextPage,
  } = useInfiniteQuery({
    queryKey: ['invites', includeTerminal],
    retry: false,
    queryFn: async ({ pageParam }) => listInviteCodes({ includeTerminal, cursor: pageParam || undefined }),
    initialPageParam: '',
    getNextPageParam: (last) => (last.pagination?.has_next ? last.pagination.next_cursor : undefined),
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: ['invites'] });

  // 生成（三值语义：上限未填=1 一次性/0=不限；有效期默认 30 天，勾选永不过期 Q2）
  const [genOpen, setGenOpen] = useState(false);
  const [genForm] = Form.useForm();
  const generate = useMutation({
    mutationFn: async (v: { max_uses: number | null; never_expires: boolean; expires_at: Dayjs | null; note: string; role?: string }) => {
      const body: CreateInviteCodeBody = {
        max_uses: v.max_uses ?? 1,
        note: v.note || undefined,
        role: v.role || undefined,
      };
      if (v.never_expires) body.never_expires = true;
      else if (v.expires_at) body.expires_at = v.expires_at.toISOString();
      // 都不设=后端默认 30 天（07 §7.1）
      return createInviteCode(body);
    },
    onSuccess: (created) => {
      message.success('已生成，可直接复制发送');
      setGenOpen(false);
      genForm.resetFields();
      invalidate();
      if (created?.code) {
        void navigator.clipboard?.writeText(created.code).catch(() => undefined);
      }
    },
    onError: (e) => message.error(`生成失败：${apiErrorMessage(e)}`),
  });

  const revoke = useMutation({
    mutationFn: async (c: InviteCode) => {
      await revokeInviteCode(c.id);
    },
    onSuccess: () => {
      message.success('已吊销');
      invalidate();
    },
    onError: (e) => message.error(`吊销失败：${apiErrorMessage(e)}`),
  });

  const del = useMutation({
    mutationFn: async (c: InviteCode) => {
      await deleteInviteCode(c.id);
    },
    onSuccess: () => {
      message.success('已删除');
      invalidate();
    },
    onError: (e) => message.error(`删除失败：${apiErrorMessage(e)}`),
  });

  const rows = data?.pages.flatMap((p) => p.codes) ?? [];
  const columns = [
    {
      title: '邀请码',
      dataIndex: 'code',
      key: 'code',
      render: (v: string) => (
        <Typography.Text copyable={{ text: v }} style={{ fontFamily: MONO_FONT }}>{v}</Typography.Text>
      ),
    },
    { title: '状态', key: 'state', render: (_: unknown, c: InviteCode) => statusTag(c) },
    { title: '角色', key: 'role', render: (_: unknown, c: InviteCode) => (c.role === 'ROLE_VIEWER' ? <Tag>访客</Tag> : <Tag color="blue">开发者</Tag>) }, // ADR-257
    {
      title: '剩余次数',
      key: 'remaining',
      render: (_: unknown, c: InviteCode) => (c.max_uses === 0 ? '不限' : `${Math.max(0, c.max_uses - c.used_count)} / ${c.max_uses}`),
    },
    { title: '过期时间', dataIndex: 'expires_at', key: 'expires', render: (v: string | null) => (v ? fmtTime(v) : '永不过期') },
    { title: '创建者', dataIndex: 'created_by', key: 'created_by' },
    {
      title: '备注',
      dataIndex: 'note',
      key: 'note',
      render: (v: string, c: InviteCode) => (
        <Tooltip title={c.last_used_by ? `最近使用：${c.last_used_by}（${fmtTime(c.last_used_at)}）` : undefined}>
          {v || '—'}
        </Tooltip>
      ),
    },
    {
      title: '操作',
      key: 'actions',
      render: (_: unknown, c: InviteCode) => (
        <Space>
          {c.state === 'active' && (
            <Popconfirm title={`确认吊销 ${c.code}？`} description="吊销后立即失效，已注册用户不受影响。" onConfirm={() => revoke.mutate(c)}>
              <Button size="small" danger loading={revoke.isPending && revoke.variables?.id === c.id}>吊销</Button>
            </Popconfirm>
          )}
          {/* Q14 两步走：删除仅终态码出按钮（后端护栏是最终防线，此处只是不摆无效按钮） */}
          {terminalOf(c) && (
            <Popconfirm title={`彻底删除 ${c.code}？`} description="删除后不留任何记录，不可恢复。" onConfirm={() => del.mutate(c)}>
              <Button size="small" danger type="text" loading={del.isPending && del.variables?.id === c.id}>删除</Button>
            </Popconfirm>
          )}
        </Space>
      ),
    },
  ];

  return (
    <div>
      <Space wrap style={{ marginBottom: 12 }}>
        <Button type="primary" onClick={() => setGenOpen(true)}>生成邀请码</Button>
        {/* 视觉映射（2026-10-04 用户裁决）：仅有效=有效过滤态→蓝（激活色）；
            全部=中性无过滤态→灰。checked 取反映射，状态语义 includeTerminal 不动。 */}
        <Switch
          checkedChildren="仅有效"
          unCheckedChildren="全部"
          checked={!includeTerminal}
          onChange={(v) => setIncludeTerminal(!v)}
        />
      </Space>
      {isError && <Typography.Text type="danger">加载失败：{(error as Error).message}</Typography.Text>}
      <Table
        rowKey="id"
        size="small"
        loading={isLoading}
        dataSource={rows}
        columns={columns}
        pagination={false}
        locale={{ emptyText: '尚无邀请码' }}
        footer={() =>
          hasNextPage ? (
            <Button size="small" loading={isFetchingNextPage} onClick={() => fetchNextPage()}>
              加载更多（已列 {rows.length}/{data?.pages[0]?.pagination?.total ?? '?'}）
            </Button>
          ) : (
            rows.length > 0 && (
              <Typography.Text type="secondary">共 {data?.pages[0]?.pagination?.total ?? rows.length} 枚码</Typography.Text>
            )
          )
        }
      />

      {/* 配置注入码（bootstrap 层）：只读如实告知（Q4/Q13）——列表空而配置码能注册的幽灵码困惑由此根除 */}
      {((data?.pages[0]?.config_codes?.length ?? 0) > 0) && (
        <Typography.Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0 }}>
          另有配置注入码（只读，生灭归配置文件/env）：
          {(data?.pages[0]?.config_codes ?? []).map((c) => (
            <Typography.Text key={c} copyable style={{ fontFamily: MONO_FONT, marginRight: 12 }}> {c}</Typography.Text>
          ))}
        </Typography.Paragraph>
      )}

      <Modal
        title="生成邀请码"
        open={genOpen}
        onCancel={() => setGenOpen(false)}
        onOk={() => genForm.submit()}
        confirmLoading={generate.isPending}
        destroyOnClose
      >
        <Form
          form={genForm}
          layout="vertical"
          initialValues={{ max_uses: 1, never_expires: false }}
          onFinish={(v) => generate.mutate(v)}
        >
          <Form.Item
            name="max_uses"
            label="使用上限"
            extra="1=一次性（默认）；0=不限（仅靠吊销下线）；N=可发给 N 人"
            rules={[{ required: true, message: '请输入使用上限' }]}
          >
            <InputNumber min={0} max={100000} precision={0} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item noStyle shouldUpdate={(a, b) => a.never_expires !== b.never_expires}>
            {({ getFieldValue }) => (
              <>
                <Form.Item name="never_expires" valuePropName="checked" label="有效期">
                  <Checkbox>永不过期</Checkbox>
                </Form.Item>
                {!getFieldValue('never_expires') && (
                  <Form.Item
                    name="expires_at"
                    label="过期时间"
                    extra="留空=默认 30 天（服务端策略）"
                  >
                    <DatePicker showTime style={{ width: '100%' }} />
                  </Form.Item>
                )}
              </>
            )}
          </Form.Item>
          <Form.Item name="note" label="备注">
            <Input placeholder="如：给张三入职用" maxLength={100} />
          </Form.Item>
          <Form.Item
            name="role"
            label="注册后角色"
            initialValue="ROLE_DEVELOPER"
            extra="开发者=可建项目跑任务；访客=只读审计数据（ADR-257；管理员请经用户管理直建）"
          >
            <Select
              options={[
                { value: 'ROLE_DEVELOPER', label: '开发者（DEVELOPER）' },
                { value: 'ROLE_VIEWER', label: '访客（VIEWER，只读）' },
              ]}
            />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
}
