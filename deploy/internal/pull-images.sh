#!/usr/bin/env bash
# 多源镜像预拉 —— 部署链的镜像可用性兜底。
#
# 背景：目标 docker 宿主的 registry-mirrors 配置
# 不可信——部分 mirror 已死、部分镜像站按命名空间白名单放行（daocloud 拒
# bitnami/*），daemon 自动回退直连 Docker Hub 又常被网络策略阻断，compose up
# 在拉取环节失败。本脚本对清单里的每个镜像做幂等预拉：
#   已有 → 跳过；直拉 → 失败则逐 mirror 显式拉取并 retag 回原名。
# 非 docker.io 镜像（ghcr.io/... 等）mirror 前缀规则不同，只直拉不试 mirror。
#
# 用法（经 sandbox-deploy.sh 分发或独立手跑）：
#   REMOTE="ssh <deploy-host> --" deploy/pull-images.sh "img1:tag,img2:tag,..."
#   deploy/pull-images.sh "img1:tag"            # REMOTE 缺省同部署链
#   MIRRORS="https://a,https://b" ...           # 覆盖 mirror 列表
#
# Environment: REMOTE / MIRRORS（逗号分隔，可含协议前缀）
set -uo pipefail

# 用 `-` 而非 `:-`：REMOTE=""（空串）是显式的"本机执行"契约，不得触发缺省
# 
REMOTE="${REMOTE-ssh <deploy-host> --}"
# 实测存活的公共源：
#   daocloud   library/* 快而稳，命名空间镜像按白名单放行（minio 通、bitnami 拒）
#   1panel.live 全量代理，bitnami/* 实测通
#   dockerproxy.net 部分代理，兜底
MIRRORS="${MIRRORS:-docker.m.daocloud.io,docker.1panel.live,dockerproxy.net}"

run_remote() { $REMOTE "$@"; }  # intentional word splitting (command prefix)

# 拉取超时包装：公共镜像源会"半速爬行/停滞"——
# 实测 1panel 传 bitnami/kafka:3.7 挂 40min+ 无进展也无失败，docker pull 自身
# 无超时参数，停滞源会挂死整条部署链。GNU timeout（coreutils；Git Bash/Ubuntu
# 均自带）+PULL_TIMEOUT（缺省 900s）兜底；宿主缺 timeout 则裸跑（原行为）。
# 注意不经 run_remote 函数（timeout 只能包可执行命令）：$REMOTE 不加引号展开
# 与 run_remote 同一 word-split 契约，REMOTE="" 时空展开等价直跑。
maybe_timeout() {
    if command -v timeout >/dev/null 2>&1; then
        timeout "${PULL_TIMEOUT:-900}" "$@"
    else
        "$@"
    fi
}

fail=0
IFS=',' read -ra imgs <<< "$(printf '%s' "$1" | tr -d ' ')"
for img in "${imgs[@]:-}"; do
    [ -n "$img" ] || continue
    if run_remote docker image inspect "$img" >/dev/null 2>&1; then
        echo "have:   $img"
        continue
    fi
    if maybe_timeout $REMOTE docker pull --quiet "$img" >/dev/null 2>&1; then
        echo "pulled: $img"
        continue
    fi
    case "$img" in
        ghcr.io/*|ghcr.io:*|quay.io/*|registry.k8s.io/*)
            echo "FAIL:   $img（非 docker.io 镜像，无 mirror 可试）" >&2
            fail=1
            continue
            ;;
    esac
    ok=""
    for attempt in 1 2; do   # 第二轮兜底：mirror 突发限流/daemon 网络爬坡（dind 实测）
        [ "$attempt" = "2" ] && sleep 10
        for m in ${MIRRORS//,/ }; do
            ns=""; case "$img" in */*) ;; *) ns="library/" ;; esac   # 官方镜像命名空间
            ref="$m/$ns$img"; [ "${m#https://}" != "$m" ] && ref="${m#https://}/$ns$img"
            if maybe_timeout $REMOTE docker pull --quiet "$ref" >/dev/null 2>&1; then
                run_remote docker tag "$ref" "$img" && run_remote docker rmi "$ref" >/dev/null 2>&1 || true
                echo "pulled: $img (via $m)"
                ok=1
                break
            fi
        done
        [ -n "$ok" ] && break
    done
    [ -n "$ok" ] || { echo "FAIL:   $img（直拉与全部 mirror 均失败）" >&2; fail=1; }
done
exit $fail
