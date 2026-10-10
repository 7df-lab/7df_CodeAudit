# web 部署设计


> 事实性速查（命令/端口/文件）以仓 README 与主仓库 `docs/dev-prod-map.md` §2.2/§3 为准；
> 本文回答"为什么是这样、边界在哪"。

## 1. 第一性原理：web 仓的本质

- 本仓是**编译器**：输入 = React/TS 源码，输出 = 纯静态资产（`dist/`）+ 一份 nginx
  边缘策略（SPA 回退、`/v1` 反代、WS 升级、上传上限）。
- 因此本仓的**唯一交付物 = 一个 OCI 镜像**（静态资产 + nginx 配置模板）。
  它不持有运行态、不持有数据、无南北向依赖以外的状态——是全系统最简单的微服务。
- 其余一切（端口、上游网关地址、容器名、网络、探针节奏）都是**部署配置**，
  属于环境不属于镜像。配置差异必须走 env/overlay，绝不应催生第二个镜像身份。

推论（奥卡姆剃刀）：**一源码修订 × 一 docker daemon = 一次编译、一个镜像身份；
N 个环境 = N 次部署消费同一镜像**。任何"每个环境各自从源码编译一份"的流程，
都是在为同一产物复制构建定义——耦合与漂移的根源。

## 2. 旧状病灶

| # | 病灶 | 事实 |
|---|------|------|
| 1 | **跨仓编译耦合** | sim overlay（`deploy/docker-compose.sim.yml`）以 `build.context: ../web` 越仓编译 web 树——engine 侧栈生命周期牵连 web 仓构建；同一源码在 <host-id> 同一 daemon 上被编译成第二镜像身份 `codeaudit-sim-web` |
| 2 | **项目名由目录 basename 决定** | `web/docker-compose.yml` 无顶层 `name:`，项目名 = `--project-directory` 的 basename（<host-id> 远端 `<legacy-path>/deploy/web` → "web"）——manager R31 已根治的同型病根 |
| 3 | **镜像不可溯源** | 恒 `:latest`、零标签，`docker inspect` 无法回答"这个容器是哪个提交构建的" |
| 4 | **陈旧字段** | compose `version: '3.8'`（compose v2 已废弃，manager R31 模板无此字段） |

## 3. 目标态原则

1. **仓界 = 构建界**：`codeaudit-web` 镜像的构建定义只存在于 web 仓
   （`Dockerfile` + `docker-compose.yml`），任何外部栈只做 `image:` 引用消费。
2. **镜像身份唯一**：一个 daemon 上 `codeaudit-web:latest` 只由 web 仓的
   compose 构建产生；sim 栈与生产栈共用同一镜像，环境差异全部收敛在
   env（`CODEAUDIT_GATEWAY_UPSTREAM=gateway:8080` vs `host.docker.internal:8090`）。
   "在模拟里通过"从此是对**字面同一镜像**的证明。
3. **项目名钉死**：compose 顶层 `name: codeaudit-web`，本地/<host-id>/一键三路径收敛。
4. **可溯源**：`GIT_REV` 构建参数 → OCI `org.opencontainers.image.revision` 标签，
   部署入口（web/deploy.sh、production-deploy.sh）自动注入源码树 HEAD。

## 4. 三链编译部署流程（改造后）

```
                    web/Dockerfile + web/docker-compose.yml   ← 构建定义唯一事实源
                                      │ (compose build, image: codeaudit-web:latest)
        ┌─────────────────────────────┼─────────────────────────────┐
   ① 模拟栈(<host-id>)                  ② <host-id> 生产                     ③ 生产一键(任意 docker 宿主)
   sim-sync push(源码树)          web/deploy.sh deploy           production-deploy.sh deploy_web
   sim.sh up:                     ├ tar 收敛式同步源码树          ├ (源码经 prod-sync 就位)
   ├ ensure_web_image:        ├ 一次性迁移守卫(旧项目 web)     ├ 一次性迁移守卫(同型)
   │  web 树在位→经 web compose    ├ compose up -d --build        ├ compose up -d --build
   │  构建;缺失→复用既有镜像       └ 健康门+/v1 401 透传检查       └ 五层验收门(401/深链/产物/WS/登录+上传)
   └ 栈 up: web 服务=image 引用(pull_policy never)
```

- ① sim overlay 的 web 服务不再含 `build:`——engine 检出台**零 `../web` 引用**，
  engine-only 检出在镜像在位时也能起栈（降级口径见 sim.sh）。
- ②③ 与①共用同一构建定义；同一 daemon 上 sim 与生产 web 是**同一镜像的两个
  部署实例**，不再存在 `codeaudit-sim-web` 第二身份（旧镜像在 <host-id> 上成为
  dangling，可手工清理，不影响运行）。
- 同 tag 共用的边界约定：<host-id> 上 sim-check 树与部署树 web**必须同批同源同步**
  （sim-sync 与 web/deploy.sh 均从本地 git 已提交树取源，正常工作流天然满足）；
  后构建者刷新 latest tag，已运行容器钉住自身镜像 ID 不受影响。

## 5. 全局微服务镜像拓扑（主仓库视角）

| 镜像 | 构建定义 SSOT（归属仓） | 消费面 |
|------|------------------------|--------|
| `codeaudit-web:latest` | `web/Dockerfile`（web 仓 compose 构建） | web compose（②③）+ sim overlay（①） |
| engine 7 服务镜像 | `engine/services/*/Dockerfile`（engine compose 构建） | engine compose + sim/prod overlay |
| `openshell-manager:2.0.0` | `manager/deploy/Dockerfile.manager`（manager compose，R31） | manager compose |
| `dsh-pentest-sse:latest` | dsh-pentest-sse 仓 Dockerfile（其 deploy.sh） | gateway DooD 按需拉起 |
| openshell gateway / PG / Redis / MinIO / Kafka | 上游镜像（digest/tag 钉制） | 各 compose |

本仓改造把 web 行对齐到与 manager R31 相同的"仓自有构建、栈引用消费"模式。
**engine 侧对齐已同日落地**（同一的并行会话，engine 仓 /

${ENGINE_IMAGE_TAG:-latest}` 钉名（tag 由部署入口钉 engine 树 sha），与本文
"镜像身份唯一"原则同源——全系统 11 个自有镜像自此身份全部钉死，无一随
compose 项目名漂移。

## 6. 迁移与回滚

- **一次性迁移守卫**（web/deploy.sh 与 production-deploy.sh 各内联一份，同型）：
  检出旧项目名 `web` 的残留资源（钉名容器/网络的属主 label）则 `compose -p web down`
  后以新项目名 up；迁移完成后守卫幂等 no-op。web 无状态，秒级窗口无数据风险。
- 回滚 = 回退本仓提交重跑 deploy（守卫反向场景不存在：旧版 compose 在新项目名
  环境下 up 会因同名网络 label 属主不同而失败——如需回滚镜像内容而非部署体系，
  直接 checkout 旧提交重新 deploy 即可，项目名体系保持新版）。

## 7. 扩展口径：未来多前端镜像（"最小代价解耦"的答案）

本仓今日只有一个可部署单元，**不预建多应用机制**（奥卡姆）。当第二个前端出现时
（如落地页/独立文档站），边际成本 = ：
1. 应用目录一个（`apps/<name>/`，含自己的 `index.html`/`vite` 入口）；
2. 参数化或独立 `Dockerfile`（静态产物镜像高度同构，可共用一份参数化模板）；
3. 本 compose 追加一个 service 块（各自 `image: codeaudit-<name>:latest`）。

部署链零改动：sim overlay 引用新 service 的镜像、deploy.sh/一键链的 service
枚举各加一行。**不引入** monorepo 工具链、不引入私有 registry、不引入 CI——
当前规模下三者皆为负资产。


## Phase 2 2a：镜像 tag 变量化与 promote

- `docker-compose.yml` 的 `image:` 行改为 `codeaudit-web:${WEB_IMAGE_TAG:-latest}`：
  显式 env 尊重（promote=引用在位 tag / 回滚=引用回退）；无 env 时缺省 latest
  （手工 `docker compose build` 行为不变）。
- `deploy.sh`：缺省 `WEB_IMAGE_TAG=树 sha`（git > .buildrev，与 engine 同推导次序）
  ——构建直出 sha tag，生产/S 级预建比对不再依赖 `:latest` 同源间接；`BUILD=1`
  仍为强制重建兜底；`BUILD=0`（预建在位）纯引用 up。
- `production-deploy.sh deploy_web`：预建快路径改为 sha tag 在位即跳过构建；
  调用子仓时显式传 `WEB_IMAGE_TAG=$ctag`。
- sim 链（sim.sh ensure_web_image）：构建传 `WEB_IMAGE_TAG` 直出 sha tag，
  retag 桥退役（latest 兜底分支保留）。
