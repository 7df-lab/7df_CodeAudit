#!/usr/bin/env bash
# gateway_db_backup.sh — gateway.db + tls 密钥树的最小备份/校验/恢复。
#
# 事故背书：一键全新树安装重置宿主
# /var/lib/openshell——gateway.db 重建 + tls 密钥换新，运行态注册的
# inference provider/route 全失；旧密文配旧密钥同亡，恢复无门。故快照
# 必须 db + tls 成套（缺 tls 的 db 快照=密文废纸）。
#
# 备份根目录刻意放在 /var/lib/openshell 之外（缺省 /var/backups/
# openshell-gateway）：网关侧重装/清空不连带抹掉备份。残留风险（如实）：
# 与 部署主机 同盘，部署主机 整机重建即失；离机副本属后续运维项，不在本脚本内。
#
# 一致性拷贝：优先 sqlite3 CLI（.backup 在线备份 API，写入中途也一致）；
# 无 CLI 则用 python3 标准库 sqlite3（backup() 同 API）。两者皆缺=硬失败，
# 绝不静默裸 cp 一个可能撕裂的库文件（宁 blocked 不假备份，U8）。
#
# Usage:
#   gateway_db_backup.sh backup [--quiet]   # 快照 + 完整性校验 + 轮转
#   gateway_db_backup.sh verify [snap-dir]  # 校验指定/最新快照（缺省最新）
#   gateway_db_backup.sh restore <snap-dir> # 恢复（网关须已停；先安全快照现状）
#   gateway_db_backup.sh list               # 列出快照与 manifest 摘要
#
# Environment（REMOTE 契约与 deploy.sh / gateway_lifecycle.sh 同款）:
#   REMOTE        命令前缀（缺省 "ssh <deploy-host> --"；空串 = 本机执行）
#   DB_PATH       缺省 /var/lib/openshell/gateway.db
#   TLS_DIR       缺省 /var/lib/openshell/tls（缺 tls 只告警降级，manifest 记 absent）
#   BACKUP_ROOT   缺省 /var/backups/openshell-gateway（/var/lib/openshell 之外）
#   KEEP          轮转保留快照数（缺省 14）
#   LIVENESS_HOST/_PORT  恢复前"网关已停"探测（缺省 127.0.0.1/8080）
#   SQLITE3_BIN / PYTHON3_BIN  工具路径覆盖（缺省 PATH 解析；测试注入点）
#
# 定时（<host-id> 侧安装，见 docs/prod-ops-2026-09-18-digest-pin-db-backup.md §4；
#   行内 REMOTE="" 与 bash 前缀不可省——缺省 REMOTE=远程执行 在 部署主机 内无 远程执行，裸跑必瞎）:
#   17 4 * * * REMOTE="" bash /opt/codeaudit/openshell-gateway/gateway_db_backup.sh backup --quiet
set -euo pipefail

REMOTE="${REMOTE-ssh <deploy-host> --}"   # `-` 非 `:-`：空串=本机执行契约（R1 同族）
DB_PATH="${DB_PATH:-/var/lib/openshell/gateway.db}"
TLS_DIR="${TLS_DIR:-/var/lib/openshell/tls}"
BACKUP_ROOT="${BACKUP_ROOT:-/var/backups/openshell-gateway}"
KEEP="${KEEP:-14}"
LIVENESS_HOST="${LIVENESS_HOST:-127.0.0.1}"
LIVENESS_PORT="${LIVENESS_PORT:-8080}"
SQLITE3_BIN="${SQLITE3_BIN:-$(command -v sqlite3 || true)}"
PYTHON3_BIN="${PYTHON3_BIN:-$(command -v python3 || true)}"

run_remote() { $REMOTE "$@"; }  # intentional word splitting (command prefix)
die() { echo "ERROR: $*" >&2; exit 1; }
log() { [ "${QUIET:-0}" = "1" ] || echo "$@"; }

snap_dirs() { # 全部快照目录名（snap-*），按名字排序即按时间排序（时间戳前缀）
    run_remote find "$BACKUP_ROOT" -maxdepth 1 -type d -name 'snap-*' \
        -printf '%f\n' 2>/dev/null | LC_ALL=C sort
}

new_snap_dir() { # 新快照目录（秒级名撞则加 -2/-3 后缀，保证幂等可连跑）
    # 时间戳只取一次（取两次可能跨秒，base 与 d 指向不同目录）
    local ts base d n=1
    ts="$(run_remote date +%Y%m%d-%H%M%S)"
    base="$BACKUP_ROOT/snap-$ts"; d="$base"
    while run_remote test -e "$d"; do n=$((n+1)); d="$base-$n"; done
    echo "$d"
}

pick_tool() { # stdout: sqlite3|python3|""（远端可用性探测，每次实跑时重新判定）
    if [ -n "$SQLITE3_BIN" ] && run_remote "$SQLITE3_BIN" --version >/dev/null 2>&1; then
        echo sqlite3
    elif [ -n "$PYTHON3_BIN" ] && run_remote "$PYTHON3_BIN" -c 'import sqlite3' >/dev/null 2>&1; then
        echo python3
    else
        echo ""
    fi
}

snapshot_db() { # snapshot_db <tool> <dest-db-path> —— 一致性拷贝（在线备份 API）
    local tool="$1" dest="$2"
    case "$tool" in
        sqlite3)
            # .backup 点命令参数按 SQL 单引号字面量引用（dest 路径含空格亦安全）
            run_remote "$SQLITE3_BIN" "$DB_PATH" ".backup '$dest'" ;;
        python3)
            run_remote "$PYTHON3_BIN" - "$DB_PATH" "$dest" <<'PY'
import sqlite3, sys
src = sqlite3.connect(sys.argv[1])
dst = sqlite3.connect(sys.argv[2])
with dst:
    src.backup(dst)
src.close(); dst.close()
PY
            ;;
        *) die "no consistent-copy tool (sqlite3 CLI / python3 sqlite3) available; refusing plain cp" ;;
    esac
}

check_db_integrity() { # check_db_integrity <tool> <db-path> → 一致则静默 return 0
    # 工具对撕裂库的非零退出必须归一到 die（直接让 set -e 杀脚本会丢
    # "integrity_check failed" 语义，验证方只能看到工具自己的报错）
    local tool="$1" db="$2" out
    case "$tool" in
        sqlite3) out=$(run_remote "$SQLITE3_BIN" "$db" "PRAGMA integrity_check;" 2>/dev/null) || out="(tool error)" ;;
        python3)
            out=$(run_remote "$PYTHON3_BIN" - "$db" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
rows = c.execute("PRAGMA integrity_check").fetchall()
print("\n".join(r[0] for r in rows))
c.close()
PY
            ) || out="(tool error)"
            ;;
        *) die "integrity check needs sqlite3 CLI or python3" ;;
    esac
    [ "$out" = "ok" ] || die "integrity_check failed for $db: $out"
}

cmd_backup() {
    local tool snap dest sum tls_state manifest
    tool="$(pick_tool)"
    [ -n "$tool" ] || die "neither sqlite3 nor python3 available on target — cannot take a consistent snapshot (install sqlite3, or python3 with stdlib sqlite3)"
    run_remote test -f "$DB_PATH" || die "gateway db not found: $DB_PATH"
    snap="$(new_snap_dir)"
    run_remote mkdir -p "$snap"
    run_remote chmod 700 "$snap"
    dest="$snap/gateway.db"

    snapshot_db "$tool" "$dest"
    check_db_integrity "$tool" "$dest"

    tls_state="absent"
    if run_remote test -d "$TLS_DIR"; then
        run_remote tar -C "$(dirname "$TLS_DIR")" -czf "$snap/tls.tgz" "$(basename "$TLS_DIR")" \
            || die "tar of $TLS_DIR failed (snapshot kept at $snap for inspection)"
        run_remote chmod 600 "$snap/tls.tgz"
        tls_state="included"
    else
        echo "WARNING: $TLS_DIR absent — snapshot carries db only; ciphertext without keys is unrestorable (LESSONS #17)" >&2
    fi

    sum="$(run_remote sha256sum "$dest" | awk '{print $1}')"
    manifest="$snap/manifest.txt"
    run_remote bash -s "$snap" "$sum" "$tls_state" "$tool" "$KEEP" "$DB_PATH" "$TLS_DIR" <<'EOF'
set -euo pipefail
{
    echo "created=$(date -Is)"
    echo "tool=$4"
    echo "db_path=$6"
    echo "tls_dir=$7"
    echo "tls=$3"
    echo "db_sha256=$2"
    echo "keep_policy=$5"
} > "$1/manifest.txt"
EOF
    log "backup OK: $snap (tool=$tool tls=$tls_state sha256=$sum)"

    # 轮转：名字序=时间序，保留最新 KEEP 个。ROTATE_PROTECT（restore 设置）=
    # 恢复源免轮转——安全快照的轮转不得销毁本轮恢复正要读取的快照
    # 
    local total old
    total="$(snap_dirs | wc -l)"
    if [ "$total" -gt "$KEEP" ]; then
        snap_dirs | head -n $((total - KEEP)) | while IFS= read -r old; do
            [ "$old" = "${ROTATE_PROTECT:-}" ] && continue
            run_remote rm -rf "$BACKUP_ROOT/$old"
            log "rotated out: $old"
        done
    fi
}

verify_snapshot() { # verify_snapshot <snap-name> → 快照目录名（非全路径）
    local snap="$1" dir="$BACKUP_ROOT/$snap" tool sum have tls
    run_remote test -d "$dir" || die "snapshot not found: $dir"
    run_remote test -f "$dir/manifest.txt" || die "manifest missing in $dir"
    run_remote test -f "$dir/gateway.db" || die "gateway.db missing in $dir"
    tool="$(pick_tool)"
    [ -n "$tool" ] || die "neither sqlite3 nor python3 available — cannot verify"
    sum="$(run_remote awk -F= '$1=="db_sha256"{print $2}' "$dir/manifest.txt")"
    [ -n "$sum" ] || die "manifest has no db_sha256 in $dir"
    have="$(run_remote sha256sum "$dir/gateway.db" | awk '{print $1}')"
    [ "$have" = "$sum" ] || die "sha256 mismatch in $dir (recorded=$sum actual=$have)"
    check_db_integrity "$tool" "$dir/gateway.db"
    tls="$(run_remote awk -F= '$1=="tls"{print $2}' "$dir/manifest.txt")"
    if [ "$tls" = "included" ]; then
        run_remote test -f "$dir/tls.tgz" || die "manifest says tls=included but tls.tgz missing in $dir"
    else
        echo "WARNING: snapshot $snap has no tls bundle (unrestorable ciphertext)" >&2
    fi
}

cmd_verify() {
    local snap="${1:-}"
    if [ -z "$snap" ]; then
        snap="$(snap_dirs | tail -1)"
        [ -n "$snap" ] || die "no snapshots under $BACKUP_ROOT"
        echo "verifying latest: $snap"
    fi
    verify_snapshot "$snap"
    echo "verify OK: $snap (sha256 + integrity_check + tls bundle)"
}

cmd_restore() { # 恢复前置：快照先过校验；网关必须已停（活着改 db = 撕裂自找）
    local snap="${1:-}"
    [ -n "$snap" ] || die "usage: $0 restore <snap-dir-name|path>"
    # 允许传全路径或目录名两种写法；统一归一为目录名
    snap="${snap##*/}"
    verify_snapshot "$snap"

    if run_remote timeout 5 bash -c 'exec 3<>/dev/tcp/$1/$2' _ "$LIVENESS_HOST" "$LIVENESS_PORT" >/dev/null 2>&1; then
        die "gateway still accepting connections on $LIVENESS_HOST:$LIVENESS_PORT — stop it first (gateway_lifecycle.sh stop); restore into a live db corrupts state"
    fi
    run_remote mkdir -p "$(dirname "$DB_PATH")" "$(dirname "$TLS_DIR")"

    # 现状先安全快照（回滚锚），再落盘恢复。ROTATE_PROTECT 钉住恢复源：
    # 安全快照触发的轮转不得删除本轮恢复的读取对象
    echo "safety-snapshotting current state before restore ..."
    ROTATE_PROTECT="$snap" QUIET=0 cmd_backup
    local dir="$BACKUP_ROOT/$snap"
    run_remote cp "$dir/gateway.db" "$DB_PATH"
    run_remote chmod 600 "$DB_PATH"
    if run_remote test -f "$dir/tls.tgz"; then
        run_remote tar -C "$(dirname "$TLS_DIR")" -xzf "$dir/tls.tgz"
    fi
    echo "restore OK from $dir -> db=$DB_PATH tls=$TLS_DIR"
    echo "next: gateway_lifecycle.sh start && verify (production: human-approved deploy.sh flow)"
}

cmd_list() {
    local s created tool tls
    run_remote mkdir -p "$BACKUP_ROOT"
    local snaps; snaps="$(snap_dirs)"
    [ -n "$snaps" ] || { echo "(no snapshots under $BACKUP_ROOT)"; return 0; }
    printf '%-24s %-28s %-8s %s\n' SNAP CREATED TOOL TLS
    for s in $snaps; do
        created="$(run_remote awk -F= '$1=="created"{print $2}' "$BACKUP_ROOT/$s/manifest.txt" 2>/dev/null || echo '?')"
        tool="$(run_remote awk -F= '$1=="tool"{print $2}' "$BACKUP_ROOT/$s/manifest.txt" 2>/dev/null || echo '?')"
        tls="$(run_remote awk -F= '$1=="tls"{print $2}' "$BACKUP_ROOT/$s/manifest.txt" 2>/dev/null || echo '?')"
        printf '%-24s %-28s %-8s %s\n' "$s" "$created" "$tool" "$tls"
    done
}

cmd="${1:-}"
case "$cmd" in
    backup)  shift; if [ "${1:-}" = "--quiet" ]; then QUIET=1; fi; cmd_backup ;;
    verify)  shift; cmd_verify "${1:-}" ;;
    restore) shift; cmd_restore "${1:-}" ;;
    list)    cmd_list ;;
    *) sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
