#!/bin/sh
# deploy/windows/crlf_check.sh — Windows 引导的 CRLF 哨兵/修复（两壳共用：
# bootstrap.ps1 经 Git Bash 或 WSL 内 bash 调用）
#
# 设计要点：
#   - 哨兵范围 = 伞仓 + 全部子仓（--recursive）的跟踪 *.sh 与 Dockerfile*——部署链
#     真正执行/消费的是这几十个文件；只查 production-deploy.sh 单文件探不到子仓被
#     用户全局 autocrlf re-smudge 的形态；
#   - 修复 = core.autocrlf false 落盘（伞仓+子仓）+ checkout-index -a -f 强制重检出
#     （按 index 内容物化、无 smudge）。修复守卫内建（2026-09-13 二批，原为调用方
#     约定）：先判别脏树性质——与行尾无关的内容变更（工作树/暂存区双检 + 子仓逐个
#     同检，git diff --ignore-cr-at-eol --quiet）→ 退出 3 拒修防丢数据；纯行尾脏
#     （re-smudge 形态）→ 放行自动归一，checkout-index 只抹 CR 不丢任何内容。
#     判别在 autocrlf 落盘之前/之后均成立（有无 clean filter 两态都已核）；
#   - 全程 POSIX sh；CR 字符经环境变量传入子命令，规避三层嵌套引号；
#   - git 需 ≥2.16（--ignore-cr-at-eol；Git for Windows / Ubuntu-22.04 均满足）。
#
# 用法:
#   crlf_check.sh <repo-posix-path>              哨兵：命中则输出文件清单并退出 1；干净退出 0
#   crlf_check.sh --repair <repo-posix-path>     修复：成功退出 0；真实内容脏拒绝修复退出 3
#   其他/缺参                                     用法错误，退出 2
set -u
CR=$(printf '\r')
export CR
repair=0
if [ "${1:-}" = "--repair" ]; then
    repair=1
    shift
fi
[ -n "${1:-}" ] || { echo "usage: $0 [--repair] <repo-posix-path>" >&2; exit 2; }
cd "$1" || exit 2
command -v git >/dev/null 2>&1 || { echo "git 不可用" >&2; exit 2; }

if [ "$repair" -eq 1 ]; then
    # 修复守卫（机制内建）：未提交改动若含与行尾无关的内容变更 → 退出 3 拒修。
    # 工作树(--diff)与暂存区(--cached)双检；子仓逐个同检。仅 CR 差异（re-smudge）
    # 或仅未跟踪文件不拦——checkout-index 只重写跟踪文件、只抹 CR，两者均不受损。
    # 根仓 --diff 须 --ignore-submodules=dirty：子仓工作树脏会把 gitlink 报成
    # modified 且 --ignore-cr-at-eol 对 gitlink 不生效（合成仓 S4 实证）——子仓
    # 内部脏由下方 foreach 逐个判别，两侧正好互补；gitlink 提交变更仍照报拒修。
    if ! git diff --ignore-cr-at-eol --ignore-submodules=dirty --quiet || \
       ! git diff --cached --ignore-cr-at-eol --quiet; then
        echo "存在与行尾无关的未提交改动——拒绝修复（checkout-index 会丢弃它们）。请 stash 后重跑；勿直接 commit（autocrlf=false 下会把 CRLF 写进仓库）。" >&2
        exit 3
    fi
    subreal=$(git submodule --quiet foreach --recursive \
        'git diff --ignore-cr-at-eol --quiet >/dev/null && git diff --cached --ignore-cr-at-eol --quiet >/dev/null || echo REALDIRTY; exit 0')
    case "$subreal" in *REALDIRTY*)
        echo "子仓存在与行尾无关的未提交改动——拒绝修复（checkout-index 会丢弃它们）。请对各子仓 stash 后重跑。" >&2
        exit 3
    ;;
    esac
    git config core.autocrlf false || exit 1
    git checkout-index -a -f || exit 1
    git submodule --quiet foreach --recursive \
        'git config core.autocrlf false && git checkout-index -a -f' || exit 1
    exit 0
fi

hits=$(git ls-files -- '*.sh' '*Dockerfile*' | xargs -r grep -l "$CR" 2>/dev/null)
# 内层管道以 exit 0 收尾：子仓干净时 grep 无命中退出 1，会被 foreach 当作
# 子命令失败（致命错误 run_command 非零 + 污染输出）；哨兵语义只看输出清单，
# 子仓 git 本身坏掉的形态由后续部署链响亮失败兜底
subs=$(git submodule --quiet foreach --recursive \
    'git ls-files -- "*.sh" "*Dockerfile*" | xargs -r grep -l "$CR" 2>/dev/null; exit 0' |
    sed "s|^|  [sub] |")
if [ -n "$hits" ] || [ -n "$subs" ]; then
    [ -n "$hits" ] && printf '%s\n' "$hits"
    [ -n "$subs" ] && printf '%s\n' "$subs"
    exit 1
fi
exit 0
