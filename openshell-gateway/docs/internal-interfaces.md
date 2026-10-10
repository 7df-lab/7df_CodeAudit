# 内部接口 — 预期输入 / 预期输出

> 本仓工件之间的接口清单（编号 I 起，以本文当次内容为准）。外部接口（对人 /
> 主仓库 / docker / 上游）见 external-interfaces.md，跨仓数据流见 data-flows.md。
> 每条内部接口 = 调用点（文件:语义）+ 预期输入 + 预期输出 + 断裂后果。
> tests/run.sh 对各条接口全部有守卫用例（编号见各行）。

---

## I1. `deploy.sh` → `gateway_lifecycle.sh`（子命令委托）　守卫 T-B3/T-E2

**调用点**：deploy 分支尾部 `./gateway_lifecycle.sh ensure`（同目录相对路径，非 exec）；status/start/stop/restart/logs 走 `exec`（进程替换，退出码零损耗透传）。

**预期输入（环境透传）**：deploy.sh **不主动 export** 任何变量——只透传自己从环境收到的：调用方 export 过的 `REMOTE/DEPLOY_DIR/…` 保持 exported 进入子进程；调用方没设的，deploy.sh 的缺省值只是普通 shell 变量，**不会**传下去，由 gateway_lifecycle.sh 用自己的同值缺省补齐。因此两脚本缺省值必须逐字相同（守卫 ）。

**预期输出**：ensure 分支下 lifecycle 的输出原样透传（ensure/verify OK 行）；exec 分支下输出与退出码完全等同 lifecycle 直跑。

**断裂后果**：两脚本 DEPLOY_DIR 缺省不一致 → 手跑 deploy.sh 与 lifecycle 打不同目录；REMOTE 语义不一致（历史实况：deploy.sh 曾用 `:-`，空串回落 远程执行）→ 本机部署链打错宿主（R1）。

## I2. `gateway_lifecycle.sh` → `docker-compose.yml`（compose 包装）　守卫 T-C 全系

**调用点**：`compose() { run_remote docker compose --project-directory "$DEPLOY_DIR" -f "$DEPLOY_DIR/docker-compose.yml" "$@"; }`。

**预期输入/输出矩阵**：

| compose 调用 | 触发处 | 预期输出 / 效果 |
|---|---|---|
| `ps -q $SERVICE` | ensure 容器存在性检查 | 空 = 容器缺失/未运行 → 触发 `up -d`（R3）；非空 = 已有 |
| `up -d $SERVICE` | ensure 自足路径 / recreate | 应用 compose **文件**（command:[] 生效，发布端口可达性须重验）；recreate 前必打 stderr WARNING |
| `start/stop $SERVICE` | cmd_start/cmd_stop | 原样透传；start 后等 liveness |
| `restart $SERVICE` | ensure 钉域后 / cmd_restart | **保留现行容器规格**仅重读 TOML——日常唯一生效手段 |
| `logs --tail=N $SERVICE` | cmd_logs | N 缺省 50 |

**约束**：`SERVICE`（缺省 gateway）必须等于 compose 文件服务键；`DEPLOY_DIR` 下必须同时有 docker-compose.yml 与 gateway.toml（相对挂载源）。

## I3. `gateway_lifecycle.sh` → `gateway.toml`（读 ×3 + 写 ×1）　守卫 T-A2/T-C1–C4

**读取器**（都打运行副本 `$DEPLOY_DIR/gateway.toml`，取第一个匹配行，剔 `\r`）：

| 函数 | 提取 | 预期输入形态 | 预期输出 |
|---|---|---|---|
| `configured_server_sans` | sed `^server_sans[[:space:]]*=[[:space:]]*(.+)$` 首行 | `server_sans = ["*.sandbox.codeaudit.internal"]` | `[\"*.sandbox.codeaudit.internal\"]`（原样值）；无匹配 → 空串 |
| `configured_supervisor_image` | sed 取双引号值首行 | `supervisor_image  = "ghcr.io/…:local"` | 镜像 ref；无 → 空串（ensure 跳过自举） |

**写入器 `patch_server_sans`**（heredoc 经 `run_remote bash -s <domain> <toml_path>` 下发，编辑不落本机）：

1. 文件缺失 → `missing <path>` stderr、exit 1
2. 已有 `^server_sans *=` 行 → sed 原位改写为 `server_sans = ["*.$ROUTING_DOMAIN"]`，stdout `rewrote server_sans -> …`
3. 无该行但有 `[openshell.gateway]` 表 → 表头后插入，stdout `inserted … under [openshell.gateway]`
4. 两者皆无 → `no [openshell.gateway] table` stderr、exit 1
5. 收尾 awk 去重：**全文件至多一行** `^server_sans *=`（多余行静默删除）
6. 改后 `cp <toml> <toml>.bak.<YYYYMMDDHHMMSS>`，stdout `backup: …`

**断裂后果**：去重缺失 → ensure 每轮只改第一行、第二行成暗雷（R4）；表名错 → 全新 TOML 钉域失败且 ensure 中断。

## I4. `gateway_lifecycle.sh` ↔ compose 环境的一致性三角　守卫 T-C5/C6/–D3

三个文件对同一批资产各持一份引用，**必须同源**：

| 资产 | lifecycle 引用 | compose 引用 | gateway.toml 引用 |
|---|---|---|---|
| 网关镜像 | `GATEWAY_IMAGE` 缺省 `…/gateway@sha256:05cf77bb…`；generate-certs 自举与 `image` 对账共用 | `image: ${GATEWAY_IMAGE:-<同 digest ref>}` | — |
| JWT 密钥目录 | `JWT_DIR` 缺省 `/var/lib/openshell/tls/jwt`；`ensure_jwt_keys` 探测 = **alpine:3.19 容器内 `test -f "$JWT_DIR/signing.pem"`**，缺失则 `docker run --rm --user 0 -v $jwt_parent:$jwt_parent $GATEWAY_IMAGE generate-certs --output-dir $jwt_parent --server-san host.openshell.internal`（PKI 根=`dirname(JWT_DIR)`，B2-3c：generate 自建 jwt/server/client 子层，根传 JWT_DIR 本身会嵌套错位） | `/var/lib/openshell` 同路径 bind（create_host_path） | `gateway_jwt.signing_key_path = /var/lib/openshell/tls/jwt/signing.pem`（public/kid 同目录） |
| supervisor 镜像 | `ensure_supervisor_image`：读 TOML 实值 → `image inspect` 已有则跳过 → `pull` 失败则推导 `:latest`（先剥 `@digest` 再剥 `:tag`）→ `tag <latest> <ref>` | —（镜像由网关运行时用） | `supervisor_image = "…supervisor:local"` |

**预期输入/输出（ensure 自举两函数）**：密钥在 → 静默 return 0；不在 → stdout `JWT signing keys absent at … — one-shot generate-certs ...`。镜像在 → 静默 return；不在且上游 404 → stdout 拉取/retag 两行；两者都拉不到 → `ERROR: supervisor image unavailable (…)` stderr、exit 1。

**断裂后果**：bind 路径与 JWT 路径不同源 → generate-certs 产物落错位置、网关启动即崩（7fa75d6）；retag 推导被改坏 → `:local` 404 无人兜底（6a04978）。

## I5. `docker-compose.yml` → `gateway.toml`（挂载链）　守卫 T-A3

**链路**：compose bind `./gateway.toml`（相对 compose 文件目录解析，**与调用者 CWD 无关**）→ 容器 `/etc/openshell/gateway.toml`（read_only）→ env `OPENSHELL_GATEWAY_CONFIG` 指向同一路径。

**断裂后果**：三者任一脱钩（env 指错路径 / 挂载目标改名 / read_only 丢失）→ 网关加载不到 TOML，回落镜像默认 CMD 行为（绑 0.0.0.0），TOML 全部口径静默失效。

## I6. `deploy.sh` FILES 清单 ↔ 仓库文件集 ↔ 远端副本　守卫 T-A7//T-E1

**契约**：`FILES=(docker-compose.yml gateway.toml gateway_db_backup.sh)`——仓库必须恰好含这些部署文件（缺失 = 立即 `missing in CD` exit 1，防半成品提交）；远端同路径 md5 比对驱动差量；推送前远端留 `.bak.<时间戳>`。lifecycle/deploy 脚本本体不在 FILES：远程执行 模式下它们在管理机本地执行，远端只需 compose 面文件 + cron 要在 部署主机 上执行的 `gateway_db_backup.sh`。

**断裂后果**：新增部署文件不进 FILES → 静默不下发（漂移检查盲区）；FILES 列了不存在的文件 → deploy 全挂（防呆设计，勿删）。

## I7. 两脚本共享缺省值矩阵　守卫 

| 变量 | deploy.sh | gateway_lifecycle.sh | 契约 |
|---|---|---|---|
| `REMOTE` | `${REMOTE-ssh <deploy-host> --}` | `${REMOTE-ssh <deploy-host> --}` | **必须同为减号 `-`**：空串 = 本机执行（R1；README"同变量"口径） |
| `DEPLOY_DIR` | `/opt/codeaudit/openshell-gateway` | 同左 | 逐字相同且钉部署树现役目录 |
| 其余（SERVICE/ROUTING_DOMAIN/LIVENESS_*/JWT_DIR） | 不设，全托 lifecycle | 各自缺省 | deploy.sh 不得私设第二缺省 |

## I8. `gateway_db_backup.sh` → gateway.db / tls 树 / 备份根　守卫 T-A5b/T-A5c/T-F 全系

**调用点**：部署主机 上 cron 调度（生产=操作单人工投运）或手跑；`REMOTE` 契约同款（空串=本机，测试即此路径）。

**输入/输出矩阵**：

| 子命令 | 读 | 写 | 预期输出 / 效果 |
|---|---|---|---|
| `backup [--quiet]` | `DB_PATH`（sqlite 一致性快照：sqlite3 CLI `.backup` → python3 兜底，双缺=拒拷贝硬失败） | `BACKUP_ROOT/snap-<ts>/`：gateway.db + tls.tgz + manifest.txt（sha256/工具/策略）；轮转留最新 `KEEP` 个 | `backup OK: <snap> (tool=… tls=… sha256=…)`；快照内 `integrity_check` 必须过 |
| `verify [snap]` | 快照三件套 | 无 | 双闸独立：manifest sha256 → `integrity_check`；tls=included 而 tgz 缺 → 硬失败 |
| `restore <snap>` | 过 verify 的快照 | `DB_PATH` + `TLS_DIR`（先对现状安全快照=回滚锚） | 网关在跑（TCP 探测通）→ 拒恢复 exit 1；否则回放 db+tls |
| `list` | 各快照 manifest | 无 | 快照清单表 |

**约束**：`BACKUP_ROOT` 缺省 `/var/backups/openshell-gateway`——**必须在 `/var/lib/openshell` 之外**（LESSONS #17：重装重置不连带抹备份）；快照目录 700、tls.tgz 600（密钥材料）。

**断裂后果**：tls 不成套 → 密文无钥不可恢复（LESSONS #17 原事故）；裸 cp 撕裂库 → 备份存在但恢复即坏；轮转 KEEP=0 → 快照即自删（M-13 注入即证）。
