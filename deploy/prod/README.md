# CodeAudit (codeaudit) 部署（docker）

CodeAudit 七服务 + 四中间件的部署事实源（ai-inference 删除 ADR-175、
knowledge-service 删除 ADR-197，neo4j/milvus 随之下线）。**服务代码与开发态 compose 在
`/root/deepseek-harness/codeaudit/`（TP10 已完成），本目录只承载部署态**：
overlay compose、密钥 env、下发脚本；运行位置为 **LXC 107 的 docker**。

链路：`CodeAudit dsh-runtime-service → manager(:18800) → gateway → dsh-pentest-sse 沙箱`

## 文件

| 文件 | 作用 |
|---|---|
| `docker-compose.deploy.yml` | overlay：restart 策略、dsh-runtime 的 manager 接线、钉网段 `codeaudit-engine-net` = 10.10.110.0/24 |
| `env` / `env.template` | JWT 密钥、网关发布端口、manager token（**gitignore**） |
| `deploy.sh` | 源码树同步（tar 管道，排除 .git/node_modules/.toolchain/archive）+ overlay + `.env` 生成 + `compose up -d --build` + 健康等待 |

## 与开发态 compose 的差异（部署态约定）

- **API 网关发布端口 8090**（默认 8080 与 openshell-gateway 冲突，经
  `.env` 的 `CODEAUDIT_HOST_GATEWAY` 压制），对外入口
  `http://gateway.internal:8090/health`。
- `dsh-runtime` 注入 `OPENSHELL_MANAGER_URL/TOKEN`（token 单一事实源 =
  `CD/openshell-manager/env`，deploy 时自动跟随，不手工维护两份）。
- **数据库只建空库**（`init_dbs`）：各服务启动时 `CREATE TABLE IF NOT
  EXISTS` 自迁移；`scripts/init-db.sql` 的表结构已落后于服务迁移（如
  `findings.verdict` 列），执行它反而会让服务起不来——弃用。
- 网络显式钉 `codeaudit-engine-net`（10.10.110.0/24）：daemon.json 默认池含
  192.168.0.0/16 会撞物理 LAN；旧 `codeaudit_default`（10.10.3.0/24）为
  无标签残留网，不复用。
- JWT 密钥首次随机生成后驻留远端 `.env`，重复 deploy 不轮换。
- ~~`CODEAUDIT_MIMO_API_KEY/ENDPOINT` 留空 = ai-inference 降级运行~~（已失效：
  ai-inference 删除 ADR-175、knowledge-service 删除 ADR-197，现为七服务；
  LLM 语义分析全部走沙箱内 DSH）。

### 上游仓库已修的缺陷（随 deploy 同步生效）

- `services/sast-adapter-service/Dockerfile`：WORKDIR 少进一层导致
  go.mod 找不到；
- `services/knowledge-service/Dockerfile`：PYTHONPATH 缺 proto-gen 路径；
  `requirements.txt` 缺 PyYAML（app/global_config.py `import yaml`）；
- `docker-compose.yml`：volumes 块缩进错位导致 task 服务缺失
  `codeaudit.yaml` 挂载（ADR-137 fail-loud panic），已在 base 补齐，
  overlay 不再重复声明。

## 日常操作

```bash
./deploy.sh deploy    # 全量：同步 → build → up → 等待网关健康
./deploy.sh check     # 源码树/overlay 漂移检查
./deploy.sh status
./deploy.sh logs 100 [service]
```

或仓库根统一入口：`../deploy.sh codeaudit [deploy|status|…]`。
首次 deploy 需拉 golang/milvus/kafka 镜像并做九次 Go 构建，耗时较长
（后续增量构建快）；中间件数据在 named volume，deploy 不清数据。

## 宿主机残留进程退役

宿主机上曾手跑 codeaudit 的 `ai-inference`（.toolchain）与 web console
（vite preview :4173），与容器栈并存无端口冲突；本栈就位后它们失去意义，
随引擎接入（部署优先级最后）一并清理。
