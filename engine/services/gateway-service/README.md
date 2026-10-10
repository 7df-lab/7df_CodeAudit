# Gateway Service

> **指针页**：本 README 历史版本的路由/端口/限流/鉴权描述
> 多处与实现不符（经验与漂移清单见 `docs/api-external.md` §7 D2 行），已停维并缩为指针页，
> 不再承载任何接口事实；历史内容见 git。

| 我需要… | 去哪（仓根相对路径） |
|---|---|
| 路由总表 / 鉴权三链 / 错误映射 / 上传与任务观测 WS 本地端点 | `docs/api-external.md`（守门测试与代码双向锁定） |
| 服务间 gRPC 契约、真实调用边 | `docs/api-internal.md` + `proto/codeaudit_common.proto`（契约 SSOT） |
| 端口口径 / 端到端数据流 | `docs/data-flows.md` |
| 限流/超时等数值 | `07_非功能指标基线.md` §7（数值 SSOT） |
| 构建 / 镜像 | 仓根 `Makefile`（`make build` / `make image-gateway-service`；本目录 `build.sh` 同口径） |
| 部署 | `deploy/README.md`（部署态事实源） |
