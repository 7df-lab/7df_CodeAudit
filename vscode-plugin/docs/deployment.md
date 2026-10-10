# 插件部署设计（deployment）


> 事实性速查（命令/文件）以仓 README 与主仓库 `docs/dev-prod-map.md` §2.3 为准；
> 本文回答"为什么是这样、边界在哪"，并封存一次被纠正的设计弯路。

## 1. 第一性原理：本仓的本质与部署的真正含义

- 本仓是**平台客户端产物的编译器**：输入 TS 源码，输出一个版本化工件
  （`codeaudit-vscode-<version>.vsix`）。扩展运行在审计员的 VS Code 进程里，
  在平台栈内**零运行态、零服务依赖**（运行时直连 engine 网关 REST/WS）。
- 因此本仓的**部署 = 编译出 VSIX**，完整流程就是一条构建链：

  ```
  npm ci → npm run compile（tsc）→ npm test（263 用例）→ npm run package
         → codeaudit-vscode-<version>.vsix（gitignored 构建产物）
  ```

- VS Code 扩展的分发模型由生态既定：VSIX 手工侧载
  （`code --install-extension <file>.vsix`）或 marketplace。为静态工件自建
  分发基础设施不产生任何产品能力。

## 2. 设计裁决

**插件不做成微服务/镜像——直接编译出插件即为部署。**

裁决时点：主仓库多会话微服务化攻坚（manager R31 / web console / engine 
"一仓一镜像"面向**有运行态**的服务）进行中，本仓如何对齐被提上议程。裁决逻辑：

1. 微服务化的收益对象是运行态（独立伸缩/部署/回滚）——本仓没有运行态，
   无收益面；
2. 给静态工件配常驻服务 = 新增端口/网段/镜像/健康探针/部署阶段，全是
   **运行时负债**，换来的能力（浏览器下载一个文件）生态已有更轻的答案；
3. 奥卡姆剃刀：如无必要，勿增实体。本仓的"专业"体现在**构建链**上
  （可复现/带门禁/产物校验），不在运行时。

对照口径：三仓"一仓一镜像" ⇄ 本仓"**一仓一工件**"——构建定义唯一
（`npm run package` 一条链），不进任何栈 compose，不占端口/网段/镜像名，
prod-sync 携带本仓仅为 check-wiring A8 枚举 parity 锚点（源码级，非构建输入）。

## 3. 构建链的专业性落点（改造后保留项）

| 项 | 事实 |
|----|------|
| **打包工具可复现** | `@vscode/vsce` 落 devDependencies：版本随 `package-lock.json` 钉死，`npx` 解析本地安装——构建零外网，vsce 升级不再无锁漂移 |
| **门禁不旁路** | `npm run package` = vsce 打包 + `scripts/verify-vsix.js`（adm-zip 必在包内，缺它扩展激活即崩）——本地/CI/任何构建路径同一关卡 |
| **产物不入库** | `*.vsix` gitignored；版本随 `package.json` 演进，交付时现构建 |

交付：VSIX 手工 sideload；安装后 `codeaudit.serverUrl` 指向平台网关。
若未来需随发布树分发 VSIX，属 release 打包层（主仓库 release-sanitize-map 口径）
决策，**不预建**。

## 4. 否决方案档案（防再发明）

**plugin-dist 分发镜像**：
多阶段 Dockerfile（node 打包 VSIX → nginx 静态托管 VSIX + latest.json 版本清单 +
index.html 入口页）+ 仓 compose（项目名钉死/GIT_REV 溯源）+ sim overlay image
引用消费 + 一键链第 6 阶段 + e2e 用例 12 + 端口 8089/18089。实施动机="栈自身
可下载匹配版本 VSIX"，被裁决否定的根因见 §2——**分发通道不是必须自建的基础
设施**。此处留档边界：若未来人类明确要求"部署好的栈提供插件下载入口"，正确
复活的也不是本方案，而应先重新过第一性原理（例如评估 console 静态目录捎带
等更轻形态），并视为新产品决策而非部署补全。
