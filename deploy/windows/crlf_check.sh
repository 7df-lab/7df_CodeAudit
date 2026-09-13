#!/bin/sh
# deploy/windows/crlf_check.sh — Windows 引导的 CRLF 哨兵/修复（bootstrap.ps1 经 Git Bash 调用）
#
# 设计要点：
#   - 哨兵范围 = 伞仓 + 全部子仓（--recursive）的跟踪 *.sh 与 Dockerfile*——部署链
#     真正执行/消费的是这几十个文件；只查 production-deploy.sh 单文件探不到子仓被
#     用户全局 autocrlf re-smudge 的形态（bootstrap 先 submodule update 后才检查的
#     时序下尤其如此）；
#   - 修复 = core.autocrlf false 落盘（伞仓+子仓）+ checkout-index -a -f 强制重检出
#     （按 index 内容物化、无 smudge）。会丢弃未提交改动——脏树拦截在调用方
#     （bootstrap.ps1 修复分支先行 git status --porcelain 检查）；
#   - 全程 POSIX sh；CR 字符经环境变量传入子命令，规避三层嵌套引号。
#
# 用法:
#   crlf_check.sh <repo-posix-path>              哨兵：命中则输出文件清单并退出 1；干净退出 0
#   crlf_check.sh --repair <repo-posix-path>     修复：无条件归一化，成功退出 0（失败非 0）
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
    git config core.autocrlf false
    git checkout-index -a -f
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
