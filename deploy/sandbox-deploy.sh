#!/usr/bin/env bash
# 伞仓统一部署入口 —— 读 deploy/sandbox-deploy.toml，把命令分发给各项目的 deploy.sh。
# 清单里的 dir 相对伞仓根；各项目 deploy.sh 的路径默认值均为脚本相对，
# 不经本分发器也可独立手跑。
#
# 各项目 deploy.sh 的统一契约：
#   deploy   幂等部署（差量下发 + 生效 + 健康验证）
#   check    漂移检查，不改任何东西
#   status   容器/服务状态 + 健康
#   start|stop|restart
#   logs [N]
#
# 用法（伞仓根执行）：
#   deploy/sandbox-deploy.sh plan                 # 清单：项目/目标/依赖
#   deploy/sandbox-deploy.sh status               # 全项目健康摘要
#   deploy/sandbox-deploy.sh check                # 全项目漂移检查
#   deploy/sandbox-deploy.sh pull                 # 全项目上游镜像预拉（多源兜底）
#   deploy/sandbox-deploy.sh all                  # 按 sandbox-deploy.toml 顺序全量部署（enabled）
#   deploy/sandbox-deploy.sh <name>               # 部署单项（先补齐未就绪依赖）
#   deploy/sandbox-deploy.sh <name> <action>      # action ∈ deploy|check|status|start|stop|restart|logs [N]
set -euo pipefail
cd "$(dirname "$0")/.."              # 清单 dir 相对伞仓根

TOML="${TOML:-deploy/sandbox-deploy.toml}"
ROOT="$PWD"   # 伞仓根（头部已 cd）；dispatch 在子 shell 里 cd 项目目录后仍要用

# ---- 解析 deploy.toml：每项目输出一行 "字段=值|字段=值|..." -----------------

load_projects() {
    awk '
        /^\[\[project\]\]/ { if (n > 0) print rec; n++; rec=""; next }
        n > 0 && /^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*[[:space:]]*=/ {
            line = $0
            sub(/^[[:space:]]+/, "", line)
            key = line;  sub(/[[:space:]]*=.*/, "", key)
            val = line;  sub(/^[^=]*=[[:space:]]*/, "", val)
            gsub(/^"|"$/, "", val)
            rec = rec (rec == "" ? "" : "|") key "=" val
        }
        END { if (n > 0) print rec }
    ' "$TOML"
}

get() {  # get <record> <field> [default]
    local val=""
    IFS='|' read -ra pairs <<< "$1"
    for p in "${pairs[@]}"; do
        [ "${p%%=*}" = "$2" ] && val="${p#*=}"
    done
    echo "${val:-${3:-}}"
}

# 依赖是否已登记在 enabled 列表里（未登记的依赖视为外部已就绪）。

dispatch() {  # dispatch <record> <action> [args...]
    local rec="$1" action="$2"; shift 2
    local name dir target vmid remote image context images
    name=$(get "$rec" name); dir=$(get "$rec" dir)
    target=$(get "$rec" target)
    if [ "$target" = "pending" ]; then
        echo "── $name: 未接入（sandbox-deploy.toml enabled=false），跳过 $action"
        return 0
    fi
    vmid=$(get "$rec" vmid 107)
    remote=$(get "$rec" remote "")
    image=$(get "$rec" image "")
    context=$(get "$rec" context "")
    images=$(get "$rec" images "")
    echo "── $name: $action ($target → vmid $vmid)"
    (
        cd "$dir"
        export REMOTE="pct exec $vmid --" DEPLOY_DIR="$remote" \
               VMID="$vmid" IMAGE="$image" CONTEXT="$context"
        # deploy 前镜像预拉（多源兜底，幂等；清单无 images 字段则跳过）
        if [ -n "$images" ]; then
            if [ "$action" = "deploy" ] || [ "$action" = "pull" ]; then
                bash "$ROOT/deploy/pull-images.sh" "$images" || exit 1
            fi
        fi
        [ "$action" = "pull" ] && exit 0
        ./deploy.sh "$action" "$@"
    )
}

deploy_with_deps() {  # deploy_with_deps <record> <seen...>
    local rec="$1"; shift
    local name dep dep_rec found
    name=$(get "$rec" name)
    IFS=',' read -ra deps <<< "$(get "$rec" depends | tr -d '[] ')"
    for dep in "${deps[@]:-}"; do
        if [ -z "$dep" ]; then continue; fi
        found=0
        for seen in "$@"; do [ "$seen" = "$dep" ] && found=1; done
        if [ "$found" = "1" ]; then continue; fi
        dep_rec=""
        while IFS= read -r r; do
            if [ "$(get "$r" name)" = "$dep" ]; then dep_rec="$r"; fi
        done < <(load_projects)
        if [ -n "$dep_rec" ]; then
            deploy_with_deps "$dep_rec" "$dep" "$@"
        fi
    done
    dispatch "$rec" deploy
}

cmd="${1:-plan}"
[ $# -gt 0 ] && shift

case "$cmd" in
    plan)
        printf '%-32s %-14s %-6s %-8s %s\n' PROJECT TARGET VMID ENABLED DEPENDS
        load_projects | while read -r rec; do
            printf '%-32s %-14s %-6s %-8s %s\n' \
                "$(get "$rec" name)" "$(get "$rec" target)" \
                "$(get "$rec" vmid -)" "$(get "$rec" enabled true)" \
                "$(get "$rec" depends -)"
        done
        ;;
    all)
        # deploy.toml 顺序即拓扑序，依赖总在依赖者之前，直接顺序执行。
        # dispatch 一律 stdin=/dev/null：内层命令（docker exec -i、tar 等）
        # 会吞读循环的清单流，静默吃掉后续项目记录。
        while read -r rec; do
            [ "$(get "$rec" enabled true)" = "true" ] || continue
            dispatch "$rec" deploy < /dev/null
        done < <(load_projects)
        ;;
    pull)
        # 全项目镜像预拉（幂等；compose up 前的可用性兜底，all 也会经 dispatch 自动跑）
        fail=0
        while read -r rec; do
            [ "$(get "$rec" enabled true)" = "true" ] || continue
            dispatch "$rec" pull < /dev/null || fail=1
        done < <(load_projects)
        exit $fail
        ;;
    status|check)
        fail=0
        # check 顺带跑部署接线审计（服务间地址/共享卷/档位——2026-09-05 接线缺陷防线）
        if [ "$cmd" = "check" ]; then
            python3 "$ROOT/deploy/check-wiring.py" || fail=1
        fi
        want="${1:-}"    # 可选：只看单个项目（deploy/sandbox-deploy.sh check <name>）
        while read -r rec; do
            [ "$(get "$rec" enabled true)" = "true" ] || continue
            if [ -n "$want" ] && [ "$(get "$rec" name)" != "$want" ]; then continue; fi
            dispatch "$rec" "$cmd" < /dev/null || fail=1
        done < <(load_projects)
        exit $fail
        ;;
    "")
        echo "usage: $0 <plan|status|check|pull|all|<project> [action]>" >&2
        exit 2
        ;;
    *)
        rec=""
        while IFS= read -r r; do
            if [ "$(get "$r" name)" = "$cmd" ]; then rec="$r"; fi
        done < <(load_projects)
        if [ -z "$rec" ]; then
            echo "unknown project: $cmd（见 $0 plan）" >&2
            exit 2
        fi
        action="${1:-deploy}"
        shift || true
        if [ "$action" = "deploy" ]; then
            deploy_with_deps "$rec" "$(get "$rec" name)"
        else
            dispatch "$rec" "$action" "$@"
        fi
        ;;
esac
