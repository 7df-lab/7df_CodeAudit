# pb-C — 沙箱镜像链路（dsh-runtime ↔ dsh-pentest-sse 双仓联动）

> 适用：改 dsh-runtime 运行时源码、bridge/settings/插件配方，或升级沙箱工具层后重建镜像。
> 核心事实：镜像内一切（源码/配方/工具）都是**构建期烘焙**的，无热更路径；
> deploy.sh 用 `git archive HEAD` 取 dsh-runtime——**未提交的改动永远进不了镜像**。

## 前置

1. `bash .agent/session.sh claim dsh-runtime "…"` 且 `claim dsh-pentest-sse "…"`
   （本链路同时占两仓）。
2. 新检出首次构建前：`dsh-pentest-sse/sandbox-artifacts/` 的 vendored 大件必须先拉
   （`fetch.sh` + `fetch-agent-tools.sh`，权威 sbom + sha256，`--verify` 可离线复核）——
   缺它构建必败（dev-prod-map §4）。

## 步骤（源码变更路径）

```bash
# 1. dsh-runtime 仓：开发 + 过闸 + commit（必须 commit！）
cd dsh-runtime && pnpm test && git add <文件> && git commit -m "…" && git push

# 2. dsh-pentest-sse 仓：改配方（bridge/settings/plugins 按工作树拷贝，建议也 commit）
cd ../dsh-pentest-sse

# 3. 若升版本 tag，三处同步（漏一处=清单失真，LESSONS #2）：
#    deploy.sh:16 的 IMAGE 默认 / agent-tools-sbom.json 的 "image" 字段 / 伞仓 deploy/sandbox-deploy.toml

# 4. 构建+部署+冒烟（staging 组装 → pct exec 107 docker build → manager API 冒烟）
./deploy.sh            # 冒烟失败不阻塞镜像就位；SKIP_SMOKE=1 跳过冒烟
```

只改配方不动源码时跳过步骤 1。版本注记（哪个 tag 对应什么变更）写进 commit message。

## 验证

```bash
cd <伞仓根>
bash deploy/sandbox-deploy.sh check dsh-pentest-sse   # 镜像在 107 就位（tag+sha+created 时间）
```

引擎侧消费验证（AI 全链）：模拟栈 e2e 用例 07（pb-A），manager 可达时断言 COMPLETED。

## DoD

- 镜像在 LXC 107 就位且 tag 正确（check 输出为准，不转述）；
- `deploy.sh` 冒烟通过（或 `SKIP_SMOKE=1` 时在账本明确记录跳过原因）；
- tag 若变更，三处同步完成；两仓均已 commit+push；伞仓清单与指针落账。

## 失败出口

- 构建失败在 stage1 sha256 校验 → vendored 大件与 sbom 不符：重跑 fetch 脚本对账。
- 构建失败在 pnpm install/build → 检查 dsh-runtime 是否真已 commit（`git -C dsh-runtime status`）。
- 冒烟失败 → manager/网关链路问题，转 pb-E；镜像本身已就位可先行交付并如实记录。

## 收尾

```bash
bash .agent/session.sh release dsh-runtime
bash .agent/session.sh release dsh-pentest-sse
# 账本记一行：时间 | C | dsh-pentest-sse:<tag> | 变更摘要+两仓 commit sha | check/冒烟结果
```
