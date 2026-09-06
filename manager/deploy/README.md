# openshell-manager 部署（docker）

OpenShell 网关管理微服务的部署事实源：服务源码即本仓根
（开发态 `./run.sh`，bind 127.0.0.1），本目录把它容器化并部署到
**LXC 107 的 docker**，为 codeaudit（以及未来接入的引擎）提供
openshell-gateway 的统一访问服务。

调用链：`codeaudit → manager(:18800) → gateway.internal:8080 → 沙箱`

> 2026-09-05 仓库重构注：本目录收编自原 `CD/openshell-manager/`
> 部署 overlay（CD 已析出归档），源码仓与部署配置自此同居一仓。

## 文件

| 文件 | 作用 |
|---|---|
| `Dockerfile.manager` | 镜像 `openshell-manager:2.0.0`：python:3.12-slim + FastAPI/uvicorn + 服务源码 + 供应商 SDK |
| `docker-compose.yml` | 编排：发布 18800、extra_hosts 指向网关、healthz、钉网段 10.10.109.0/24 |
| `env` / `env.template` | `OPENSHELL_MANAGER_TOKEN`（**gitignore**，模板入库）；token 与 `codeaudit/env` 同值 |

## 纪律

- 容器绑定 `0.0.0.0` 属非环回，服务自身红线强制要求 token（`config.py validate()`）；
  `/api/*` 全部 Bearer 鉴权，`/healthz` 豁免。
- 不挂 docker.sock：manager 只经 gRPC 访问网关，沙箱容器由网关创建。
- 服务代码改动在本仓根做，`./deploy.sh` 负责同步进镜像；部署形态
  （端口/env/网络）改动只在本目录做。
- `config.json` 不入镜像部署流：镜像里那份仅为兜底，运行态配置全部来自
  `.env`（优先级 env > config.json）。

## 日常操作

```bash
./deploy.sh deploy   # 同步源码+产物+密钥 → build → up → healthz → 网关可达性
./deploy.sh check    # 漂移检查（源码树 / 部署产物 / .env）
./deploy.sh status   # compose ps + healthz
./deploy.sh logs 100
```

## 宿主机裸进程退役

宿主机上曾以 `python3 -m openshell_manager` 手跑过开发实例
（127.0.0.1:18800）。容器实例就位后它仍可并存（不同命名空间，端口不冲突），
待宿主机引擎迁移到 `http://gateway.internal:18800`（引擎部署项，优先级最后）
时一并停掉，避免双实例分叉。
