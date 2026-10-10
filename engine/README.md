# CodeAudit Engine

平台引擎：API 网关（REST + WebSocket 对外唯一入口，JWT 认证）+ 6 个 gRPC 微服务
（project / task / result / storage / sast-adapter / runtime）。数据契约以
`proto/codeaudit_common.proto` 为单一事实源；构建与部署走主仓库 `deploy/` 一键链
（Linux：`production-deploy.sh`；Windows：`deploy.ps1`）。

