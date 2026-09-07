# REGRESSIONS — 回归档案与防回归机制

> 本仓的防回归机制 = **三类档案 + 一道证明 + 一道门禁**。每次发现新缺陷，
> 修复 commit 必须同时补齐三样东西（缺一不算收口）：
> ① 本文档案加一行（类别→实例→守卫→注入）；② tests/run.sh 加守卫用例；
> ③ selfcheck 加对应变异注入（证明守卫活着）。
> 机制的本意：测试套不是写完就完，`--selfcheck` 随时可以证明"这些测试真的
> 拦得住"，防止测试腐化成摆设。

## 一、用法（改本仓任何文件后的门禁）

| 层 | 命令 | 何时跑 | 耗时 |
|---|---|---|---|
| 静态门禁（U6） | `bash tests/run.sh --fast` | 每次 commit 前（pre-commit 钩子自动跑） | <5s |
| 全量离线 | `bash tests/run.sh` | 改 compose/TOML/两脚本后 | ~40s |
| 变异自检 | `bash tests/run.sh --selfcheck` | 加新守卫时 / 定期体检测试套 | ~1min |
| 运行时门禁 | `bash tests/run.sh --with-runtime` | 有 LXC 107 可达时（只读动作） | 取决于远端 |

## 二、回归档案（历史缺陷 → 守卫 → 注入证明）

### R1　REMOTE 空串本机执行契约
- **实例**：5dbc735（gateway_lifecycle.sh `:-`→`-`，dind 生产态实测暴露：空串 REMOTE
  不得触发 pct 缺省，伞仓 production-deploy.sh 依赖空串=本机）；2026-09-07 通读又
  发现 deploy.sh 潜伏同族变体（`:-`，与 README"同变量"口径相悖），同批修复。
- **守卫**：T-B1a/T-B1b（减号缺省逐字断言）、T-B2 系（空串→桩 pct 零调用）、
  T-B3/T-B4（pct 前缀/缺省两态）、T-D4（两脚本缺省逐字一致）。
- **注入**：M-4、M-5（退化回 `:-` → 均被拦截）。

### R2　compose `command: []`（TOML 唯一配置源的根据）
- **实例**：README/docker-compose 头注固化的历史坑——镜像默认 CMD
  （`--bind-address 0.0.0.0`）优先级高于 TOML，不清 CMD 则 loopback bind 被静默忽略。
- **守卫**：T-A3b（`command == []`）。
- **注入**：M-1（删空 CMD → 拦截）。

### R3　ensure 自足三连（全新宿主/清空后重部署即崩）
- **实例**（107 全量退役实测连续暴露三处隐藏手工步骤）：
  7ca41c4 容器缺失时 ensure 对空栈等 liveness 60s（现先 `compose up -d`）；
  7fa75d6 JWT 签名密钥缺失启动即崩（现一次性 generate-certs 预置）；
  6a04978 supervisor 镜像 `:local` 上游 404 拉取失败退出（现拉 `:latest` retag）。
- **守卫**：T-C7a/T-C7b（缺失先 up -d 且时序正确）、T-C5a–d（JWT 自举+幂等）、
  T-C6a–c（retag 自举+幂等）、T-A1n（jwt 路径在 bind 内）、T-D1/T-D3/T-D6（同源静态）。
- **注入**：M-9（jwt 路径漂出 bind → 拦截）。

### R4　server_sans 单行 + 幂等钉住
- **实例**：路由域漂移 = 沙箱服务 URL 全部失联；双行 = ensure 只认首行、
  次行成暗雷（patch 的 awk 去重即为防它）。
- **守卫**：T-A1c/T-A2（仓内静态）、T-C1a–f（错域纠正+备份+restart）、
  T-C2a–c（幂等）、T-C3a–b（双行去重）、T-C4a–b（无表报错拒钉）、T-B5（verify 两态）。
- **注入**：M-2（域漂移）、M-3（双行）→ 均拦截。

### R5　结构化配置 parse（U6 / LESSONS #8）
- **实例**：伞仓纪律：compose/yaml 重键静默覆盖、TOML 语法错会在运行时才爆。
- **守卫**：T-A1a（tomllib，重键即抛）、T-A3a（yaml dup-key loader——PyYAML
  默认**不报**重键，本套自造 loader）、T-A4/T-A5（bash -n）、T-A6（shellcheck 可选）。
- **注入**：M-6（compose 重键）、M-7（bash 语法损坏）→ 均拦截。

### R6　端口同号三角（沙箱回调路由的根）
- **实例**：`grpc_endpoint=host.openshell.internal:8080` 只留 scheme，宿主端口必须
  同号发布 8080 才能路由进网关容器——改任何一角=全链路失联。
- **守卫**：T-A1d/T-A1h、T-A3f、T-D2（三角逐字断言）。
- **注入**：M-8（宿主端口改 9090 → 拦截）。

### R7　挂载链只读与同路径 bind
- **实例**：README 固化的因果——`/var/lib/openshell` 同路径 bind 是宿主 daemon
  解析沙箱 bind source 的前提；TOML 只读挂载防运行时篡改配置。
- **守卫**：T-A3i/T-A3j/T-A3k。
- **注入**：M-10（read_only 丢失 → 拦截）。

### R8　文档/口径失真
- **实例**：7326e27（README 按实测整体重写：8081"发布但不可达"、Dockerfile
  staging 路径属上游等两处口径修正）。文档断言必须实测（伞仓 U7）。
- **守卫**：流程性——改入口/端口/SSOT 同 commit 同步 dev-prod-map + playbook；
  docs/ 三份接口文档即本文档体系的基准，改动须同步。测试层守住可静态化的
  口径（T-A3g 的 8081 映射在位等）。

## 三、机制维护纪律

1. **新缺陷**：修复 commit = 代码修复 + 守卫用例 + 注入 + 本档案加行，一次收口；
   伞仓账本（`.agent/status.md`）同步记一行。
2. **改测试**：改完必跑 `--selfcheck`，守卫失效（注入不拦）即 FAIL。
3. **信任链**：pre-commit 钩子（`git config core.hooksPath tests/githooks`）只跑
   `--fast` 静态层（秒级、离线），行为层与注入层按上表节奏跑；解锁提交用
   `git commit --no-verify`（须在账本说明理由）。
