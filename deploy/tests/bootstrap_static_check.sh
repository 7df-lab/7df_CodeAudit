#!/usr/bin/env bash
# deploy/tests/bootstrap_static_check.sh — bootstrap.ps1 传输层静态门禁
# （2026-09-13 审计 A1-A5 根治批：脚本文本禁走 PS 命令行）
#
# 断言：
#   ① 无残留 `bash -lc '`——脚本文本经命令行直传 = PS5.1 内嵌双引号不转义 ×
#      bash.exe MSVCRT 剥引号切参的碎裂根因，必须全部走临时文件/stdin 传输函数；
#   ② 全部 bash 语法形态的下发脚本字面量：bash -n 语法绿 + 纯 ASCII + LF；
#   ③ To-PosixPath 盘根/盘符相对/正常路径三态正则行为（Python 模拟）。
#
# 用法: bash deploy/tests/bootstrap_static_check.sh   —— 需 bash、grep、python3
set -u
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
BOOT="$ROOT/deploy/windows/bootstrap.ps1"
[ -f "$BOOT" ] || { echo "bootstrap.ps1 不存在: $BOOT" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 不可用" >&2; exit 2; }

PASS=0; FAIL=0
ok()  { echo "  ✓ $1"; PASS=$((PASS+1)); }
bad() { echo "  ✗ $1"; FAIL=$((FAIL+1)); }

echo "== ① 无 bash -lc 脚本直传残留 =="
if grep -n "bash -lc '" "$BOOT" >/dev/null 2>&1; then
  bad "①残留 bash -lc 直传："
  grep -n "bash -lc '" "$BOOT" | sed 's/^/    /'
else
  ok "①无残留（全部经 Invoke-GitBashSh/Invoke-WslSh 下发）"
fi

echo "== ② 下发脚本字面量：语法/ASCII/LF =="
# 抽取 PS 单引号字面量（'' 转义还原），筛出 bash 语法形态者逐一 bash -n
python3 - "$BOOT" <<'PYEOF'
import re, subprocess, sys, tempfile, os
boot = open(sys.argv[1], 'rb').read()
text = boot.decode('utf-8-sig')
# PS 单引号字面量：'...' 内 '' 为转义单引号
literals = []
for m in re.finditer(r"'((?:[^']|'')*)'", text):
    lit = m.group(1).replace("''", "'")
    line = text.count('\n', 0, m.start()) + 1
    literals.append((line, lit))
# bash 语法形态判定：含命令分隔/重定向/常见命令词头，排除纯 PS 词汇（正则、路径、消息）
bashish = re.compile(r'(?:;|\&\&|\|\||\b(?:cd|sh|bash|printf|mkdir|command|test|exec|git)\b|>[^>]|2>&1)')
fails = 0
checked = 0
for line, lit in literals:
    if not bashish.search(lit):
        continue
    checked += 1
    issues = []
    if not all(ord(c) < 128 for c in lit):
        issues.append('含非 ASCII 字符')
    if '\r' in lit:
        issues.append('含 CR')
    # PS 端 $caPre + '; ...' 拼接的右半段单独存在时以 ';' 起头——加 ': '（bash
    # 空命令）前缀还原拼接后形态再验语法，其余断言（ASCII/LF）照查原串
    check_text = (': ' + lit) if lit.lstrip().startswith(';') else lit
    with tempfile.NamedTemporaryFile('w', suffix='.sh', delete=False, newline='') as f:
        f.write(check_text + '\n')
        tf = f.name
    r = subprocess.run(['bash', '-n', tf], capture_output=True, text=True)
    os.unlink(tf)
    if r.returncode != 0:
        issues.append('bash -n 语法错: ' + r.stderr.strip().splitlines()[-1] if r.stderr.strip() else 'bash -n 语法错')
    if issues:
        fails += 1
        print(f"    ✗ 行 {line}: {'; '.join(issues)}")
        print(f"      字面量: {lit[:120]}")
if fails == 0:
    print(f"  ✓ ②{checked} 个 bash 形态字面量全部语法绿/ASCII/LF")
    sys.exit(0)
else:
    print(f"  ✗ ②{checked} 个中 {fails} 个字面量不合格")
    sys.exit(1)
PYEOF
if [ $? -eq 0 ]; then ok "②字面量门禁通过"; else bad "②字面量门禁未通过"; fi
# 抽取器自证：字面量总数必须达到下限，防止正则失效导致空转绿
cnt=$(python3 - "$BOOT" <<'PYEOF'
import re, sys
text = open(sys.argv[1], encoding='utf-8-sig').read()
n = 0
for m in re.finditer(r"'((?:[^']|'')*)'", text):
    lit = m.group(1).replace("''", "'")
    if re.search(r'(?:;|\&\&|\|\||\b(?:cd|sh|bash|printf|mkdir|command|test|exec|git)\b|>[^>]|2>&1)', lit):
        n += 1
print(n)
PYEOF
)
[ "${cnt:-0}" -ge 8 ] && ok "②抽取自证：bash 形态字面量 ${cnt} 个（≥8）" || bad "②抽取自证失败：仅 ${cnt:-0} 个（<8），抽取器可能失效"

echo "== ③ To-PosixPath 三态正则模拟 =="
python3 - <<'PYEOF'
import re, sys
def topath(p):
    p = p.rstrip('\\')
    if re.match(r'^[A-Za-z]:$', p): return 'DIE:盘根'
    if re.match(r'^[A-Za-z]:[^\\/]', p): return 'DIE:盘符相对'
    m = re.match(r'^([A-Za-z]):[\\/](.*)$', p)
    if m: return '/' + m.group(1).lower() + '/' + m.group(2).replace('\\', '/')
    return p.replace('\\', '/')
cases = [
    (r'C:\Users\me\codeaudit', '/c/Users/me/codeaudit', '常规路径'),
    (r'C:/Users/me/codeaudit', '/c/Users/me/codeaudit', '正斜杠路径'),
    ('C:tools', 'DIE:盘符相对', '盘符相对路径拒绝'),
    ('C:', 'DIE:盘根', '裸盘符拒绝'),
    ('C:\\', 'DIE:盘根', '盘根拒绝'),
    ('~/codeaudit-umbrella', '~/codeaudit-umbrella', '~ 路径原样'),
]
fails = 0
for src, want, desc in cases:
    got = topath(src)
    if got == want:
        print(f"  ✓ {desc}: {src!r} -> {got!r}")
    else:
        print(f"  ✗ {desc}: {src!r} 期望 {want!r} 实际 {got!r}")
        fails += 1
sys.exit(1 if fails else 0)
PYEOF
if [ $? -eq 0 ]; then ok "③To-PosixPath 模拟通过"; else bad "③To-PosixPath 模拟未通过"; fi

echo ""
echo "================ bootstrap_static_check 结果 ================"
echo "通过=$PASS 失败=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
exit 0
