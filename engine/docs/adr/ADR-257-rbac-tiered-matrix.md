# ADR-257: RBAC 层级制权限矩阵（读面放开/操作面 DEVELOPER+/判定面保持 owner-admin）

> 来源：2026-10-10 122 生产栈（v0.1.2, f90eb9a）GUI 全量测试暴露 P1——邀请码注册的
> 非 admin 用户登录即陷入全屏 403 死局（落地 /projects 必 403 → 全局错误层毁导航壳
> → 无法登出），用户裁决升级为 RBAC 正式设计（grill 会 Q1-Q12 全拍板，2026-10-10）。

## 裁决（权限矩阵终表，Q11 批准）

| 动作 | VIEWER | DEVELOPER | ADMIN |
|---|---|---|---|
| 读：项目/任务/发现/报告/AI 日志（列表+详情+下载） | ✅ | ✅ | ✅ |
| 建项目/建任务/更新项目配置/生命周期动作/图编排互动/报告生成 | ❌ | ✅ | ✅ |
| verdict 复核回写 | ❌ | owner 或 admin（不变） | ✅ |
| 删项目/用户·工作区·推理·邀请码·编排写面 | ❌ | ❌ | ✅ |
| 通知 | 本人分域（ADR-212，不变） | 本人 | 本人 |

1. **层级制而非资源粒度制（Q7-A）**：审计数据面=所有认证角色可读（单团队工具，
   "登录用户=可信操作员"）；资源属主隔离（DEVELOPER 只见自己+成员项目）确认为
   立项级工程（proto 无 owner 过滤载体+成员管理 UI 缺位），留待多租户需求成立时另立。
2. **R89-R91 读面反转**：projects/reports/findings 裸列表 admin-only、单资源
   owner/admin 读门，自本 ADR 起放开为认证即读——一期收紧（2026-09-13 设计提案
   §一"服务端 owner 过滤立项后可放宽"）的放宽条件由本决议以层级制形式兑现。
   **判定面（verdict 回写）不随读面反转**，canAccessTask/ViaFinding/ViaReport
   收敛为写面专用。
3. **项目写面拆分（R91 修正）**：create/update/config-update 从 admin-only 放宽为
   操作面 DEVELOPER+（requireOperator）；delete 维持 admin（requireAdmin）。
   任务生命周期动作（start/cancel/retry/pause/resume/agent/complete）与图编排
   节点操作（message/reopen/claim/release）、报告生成同属操作面。
4. **角色集沿用 proto 三角色**（ADMIN/DEVELOPER/VIEWER）；注册默认 DEVELOPER
   （既有实现不变，Q6-A）；web 端按 role 隐藏操作按钮（后端为最终防线）。
5. **邀请码绑角色（ADR-255 扩展）**：InviteCode/CreateInviteCodeRequest 增 role
   字段（仅 DEVELOPER|VIEWER；UNSPECIFIED=DEVELOPER；ADMIN 禁发——admin 经控制
   台直建，防自助注册自封）；注册按码落地角色（旧码/静态配置码兼容=DEVELOPER）。
   PG invite_codes 为 payload JSONB 整体存储，零 DDL 变更。
6. **ADR-231 欠账销账**：/v1/tasks 裸列表无归属过滤（api-external.md:121 挂账）
   自本决议由"欠账"转正为设计语义（审计数据面全员可读）。

## 关联

- 网关：authz.go（isOperator/requireOperator 新增；canAccessProject/gateTaskScopedList
  删除）+ transcode.go 五区门禁改写（projects/tasks/findings/reports 32 处）。
- project-service：invite.go（CreateInviteCode role 校验+inviteCodeRole 解析）、
  user_lifecycle.go（RegisterUser 按码落地角色）。
- proto：InviteCode.role=13 / CreateInviteCodeRequest.role=7。
- 测试：TestAuthz_* 三矩阵改写（新语义含 VIEWER 断言）+ TestInviteCode_RoleBinding
  新增 + TestTP12T0_FindingsByTask 口径更新（403→400）。
- 台账：R143；GUI 侧配套（403 呈现/落地路由/文案）随 web 同批（伞仓 v0.1.3）。
