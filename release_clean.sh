#!/usr/bin/env bash
# release_clean.sh — 发布专业化清理：开发态遗留物删除 + 设计文档过程性备注清除
#
# 用法:
#   bash release_clean.sh artifacts   # 删除开发态遗留（.agent/ archive/ 任务标记/CI/开发文档）
#   bash release_clean.sh remarks     # 清除 01-14 设计文档里的过程性备注（迁移史/评估报告编号等）
#   bash release_clean.sh check      # 门禁：遗留物=硬违例(退出码1)；备注残留=人工确认清单
#   bash release_clean.sh all        # artifacts + remarks
#   bash release_clean.sh -n artifacts  # dry-run：只列出将删除的文件
#
# 与 sanitize.sh 的分工: sanitize 清"敏感信息"（每次提交必跑）；
# 本脚本清"开发态内容"（同步新内容进发布工作区后、push 前跑一次）。

set -euo pipefail
cd "$(dirname "$0")"

DRYRUN=0
if [ "${1:-}" = "-n" ]; then DRYRUN=1; shift; fi
MODE="${1:-}"
[ -z "$MODE" ] && { sed -n '2,11p' "$0"; exit 2; }

# ============================================================
# 一、开发态遗留物清单（目录/文件名模式；随工作区演进可增删）
#    保护规则:
#    - 根目录 ./AGENTS.md 是本工作区现行宪法（AI 会话入口），不删
#    - AGENTS.md 只删子仓根部（深度2）的宪法；深层嵌套的（如 dsh-runtime
#      packages 文档、snapshots/ 测试夹具）是内容数据，不删
#    - 深度2 的 .git（子仓 git 指针文件，随 GitLab 同步反复回来；根 ./.git 是本仓，不删）
# ============================================================
# 目录名精确匹配（-name），避免 find Emacs 正则的管道符转义坑
ARTIFACT_DIR_NAMES='.agent .agents .zcode .claude .cursor .aider .trae archive'

scan_artifacts() {
  local dir_tests=""
  for d in $ARTIFACT_DIR_NAMES; do dir_tests+=" -name $d -o"; done
  dir_tests="${dir_tests% -o}"
    { find . -mindepth 2 -maxdepth 2 -name ".git" -print 2>/dev/null;
    find . -mindepth 1 -maxdepth 2 \
      \( -name .git -o -name node_modules -o -name .toolchain -o -name .githooks \) -prune -o \
      -type d \( $dir_tests \) -print 2>/dev/null;
    find . \
      \( -name .git -o -name node_modules -o -name .toolchain -o -name .githooks \) -prune -o \
      -type f \( -name "*_COMPLETE.md" -o -name "*.gitlab-ci.yml" -o -name "BUILD_INSTRUCTIONS.md" \
           -o -name "IMPLEMENTATION_SUMMARY.md" -o -name "MANUAL_TEST_GUIDE.md" \
           -o -name "fix-log-*.md" \) -print 2>/dev/null;
    find . -mindepth 2 -maxdepth 2 -name AGENTS.md -not -path "./.git/*" 2>/dev/null; } | sort -u
}

mode_artifacts() {
  local targets
  targets=$(scan_artifacts)
  if [ -z "$targets" ]; then
    echo "[artifacts] 无开发态遗留物"
    return 0
  fi
  if [ "$DRYRUN" = 1 ]; then
    echo "[artifacts] dry-run，将删除:"
    printf '%s\n' "$targets" | sed 's/^/       /'
    return 0
  fi
  printf '%s\n' "$targets" | while read -r t; do
    rm -rf "$t"; echo "[artifacts] 已删除 $t"
  done
  echo "[artifacts] 完成。注意修补引用（Makefile/README 里的 .agent、verify.sh、AGENTS.md 链接）"
}

# ============================================================
# 二、过程性备注清除（内嵌 Python，规则有序执行；UTF-8 原样读写）
# ============================================================
mode_remarks() {
  python - $([ "$DRYRUN" = 1 ] && echo --dry-run) <<'PYEOF'
import glob, io, re, sys

dry = '--dry-run' in sys.argv

# 目标: engine/ 下的编号设计文档 + README（11a 评估报告本身是报告实体，排除）
targets = sorted(
    glob.glob("engine/[0-9][0-9]_*.md") + glob.glob("engine/1??_*.md".replace("??", "a_")) +
    ["engine/README.md"])
targets = [t for t in targets if not t.startswith("engine/11a")]
targets = sorted(set(targets))

# (pattern, replacement) 有序规则; replacement=None 表示整行删除
RULES = [
    # --- 引言行改写（语义保留、过程叙事去除）---
    (r'^> 本文档修复 V1\.x .*$',
     None),
    (r'^> 与 V1\.x 差异：报告生成统一走 S9 Kafka 主路径（ADR-006）；模式B直调、模式D Kafka 的三条路径收敛为一条。$',
     '> 报告生成统一走 S9 Kafka 主路径（ADR-006）。'),
    (r'^修正说明：V1\.x 中模式A场景标注.*本矩阵已修正（⑪闭环）。$',
     '说明：五Agent 的 Vuln Detector 必然访问知识双层（至少 Skills 层）。'),
    (r'^> 本文档解决 V1\.x[^\n]*（矛盾④⑯）。终裁为 ',
     '> 知识层架构终裁为 '),
    (r'^> 本文档收敛 V1\.x 体系中 (?P<list>[^\n]*?)等全部数值冲突（[^）]*），任何文档与本表不一致时以本表为准。$',
     '> 本文档为 \\g<list>等非功能数值的唯一事实源，任何文档与本表不一致时以本表为准。'),
    (r'^> 本文档执行 V1\.x 未完成的合并回填：(?P<body>[^\n（]*?)（修复[^）]*）。$',
     '> \\g<body>。'),
    (r'^\*\*合并回填说明\*\*：V1\.x 矩阵中的[^\n]*，按 01 §4\.2 映射并入对应部署服务清单。$',
     '**服务映射说明**：proto 服务按 01 §4.2 映射并入对应部署服务清单。'),
    (r'^\*\*GNN 残留清除声明（N1）\*\*：.*$',
     '**GNN 决策（ADR-001）**：图结构分析能力由 CPG 污点追踪（符号方法）+ LLM 语义理解（神经方法）承担，'
     '补偿方案为 CPG 污点验证 + 规则引擎兜底，即本节实现。'),
    (r'^> \*\*S1 规则\*\*：数据结构唯一事实源为 `(?P<proto>[^`]+)`，本文档不内嵌 proto 定义[^\n]*$',
     '> **S1 规则**：数据结构唯一事实源为 `\\g<proto>`，本文档不内嵌 proto 定义。'),
    (r'^> 旧文档数据流图缺失[^\n]*$',
     None),
    (r'^> 旧文档[^\n]*$',
     None),
    (r'^\*\*归档区\*\*（archive/[^\n]*$',
     None),
    (r'^评审报告中的[^\n]*不作数。$',
     None),
    # --- 语义保留的特殊改写（先于通用规则）---
    (r'⚠️ V1\.0 仅实现 QuerySemanticSimilar 单查询；\*\*完整 Schema 于 M15 前交付，逾期则从申报书撤下该创新点表述\*\*',
     '⚠️ V1.0 仅实现 QuerySemanticSimilar 单查询，完整 Schema 列入后续里程碑交付'),
    (r'，废除"0\.15"写法（X3）。', '（禁止小数比率写法）。'),
    (r'（V1\.x 只有3值，缺模式D）', '（4 值枚举，含模式D）'),
    (r'（R7 定版 11 个）', '（11 个）'),
    (r'含评审争议双轨制，[RNXBC][0-9]+', '双轨制'),
    # --- 版本表/H1/标题的沿革括注 ---
    (r'（迁移整改重写，替代旧《[^》]*》，旧文档归档）', ''),
    (r'（迁移整改新建；[^）]*）', ''),
    (r'（定版基线，随迁移整改建立）', ''),
    (r'（随迁移整改建立）', ''),
    (r'（迁移整改[^）]*）：', ': '),
    (r'^(#{1,2} [^\n（]*?)（V1\.[0-9] 定版[—-][^）]*）\s*$', r'\1'),
    (r'^(#{1,2} [^\n（]*?)（V1\.[0-9] 定版）\s*$', r'\1'),
    (r'^(# [^\n（]*?)（V1\.[0-9]）\s*$', r'\1'),
    (r'（V2\.0 迁移整改完成版）', ''),
    (r'（V1\.[0-9X]+[：:][^）]*）', ''),
    (r'（V1\.[0-9]+重写）', ''),
    (r'<!-- 06 V1\.1（迁移整改）：环境变量拼写修正 OPENHELL→OPENSHELL；', '<!-- '),
    # --- 里程碑/交付/兑现类过程语 ---
    (r'（V1\.1 交付，M9 里程碑；落地 V1\.x 附录建议）', ''),
    (r'（兑现 V1\.x README 声明但未设计的能力）', ''),
    (r'（均为 V1\.x 真实发生过的事故）', ''),
    (r'（V1\.x §7\.1 的 page 字段作废）', ''),
    # --- 评估报告编号与矛盾/闭环标记 ---
    (r'（[RNXBC][0-9]{1,2}(/[A-Z][0-9a-z]*)* 收敛：', '（'),
    (r'（[RNXBC][0-9]{1,2}(/[A-Z][0-9a-z]*)* 收敛，', '（'),
    (r'（[RNXBC][0-9]{1,2}(/[A-Z][0-9a-z]*)* 收敛）', ''),
    (r'（修复 [^）]*）', ''),
    (r'——README 不再自行记录版本与整合状态（[^）]*）。', '。'),
    (r'迁移整改建立：收敛 [^|]* 全部数值冲突', '建立非功能数值基线'),
    (r'（[RNXBC][0-9]{1,2} 定版）', ''),
    (r'（[RNXBC][0-9]{1,2} 定版；[^）]*）', ''),
    (r'，[RNXBC][0-9]{1,2} 定版）', '）'),
    (r'（[RNXBC][0-9]{1,2} 定版 ）', '（'),
    (r'（[RNXBC][0-9]{1,2}（[:：]', '（'),  # 如 （N10：唯一来源…）
    (r'（[RNXBC][0-9]{1,2}[:：]', '（'),
    (r'（[RNXBC][0-9]{1,2} 摘要）', ''),
    (r'（[RNXBC][0-9]{1,2} 实现）', ''),
    (r'（[RNXBC][0-9]{1,2} 保持', '（保持'),
    (r'（[RNXBC][0-9]{1,2}）', ''),
    (r'（[RNXBC][0-9]{1,2}(/[A-Z][0-9a-z]*)+）', ''),
    (r'（ADR-([0-9]+) 教训）', r'（ADR-\1）'),
    (r'（矛盾[①-⑯Ⓐ-Ⓩ]+收敛?）', ''),
    (r'（修正版，修复⑪）', ''),
    (r'（修复⑪[^）]*）', ''),
    (r'（修复V1\.x[^）]*）', ''),
    (r'R3/N 补课：含 ', '含 '),
    (r'（含 ([^）]*) 定版）', r'（含 \1）'),
]

changed = []
for path in targets:
    try:
        raw = open(path, 'rb').read().decode('utf-8')
    except (UnicodeDecodeError, FileNotFoundError):
        continue
    # 统一行尾为 LF：同步自 GitLab 的文件常带 CRLF，\r 会破坏 $ 锚点规则
    raw = raw.replace('\r\n', '\n')
    text = raw
    for pat, rep in RULES:
        text = re.sub(pat, rep if rep is not None else '', text, flags=re.M)
    # 清理规则产生的连续空行（>2）与行尾空白
    text = re.sub(r'\n{3,}', '\n\n', text)
    text = re.sub(r'[ \t]+$', '', text, flags=re.M)
    if text != raw:
        changed.append(path)
        if not dry:
            open(path, 'wb').write(text.encode('utf-8'))

print('[remarks]%s处理 %d 个文档，%s %d 个:' % (
    ' dry-run，' if dry else ' ', len(targets), '将改动' if dry else '改动', len(changed)))
for p in changed:
    print('       ' + p)
if not changed:
    print('[remarks] 全部干净')

# ---- 第二阶段: 保留文档里的 .agent 死链修补 + 基建运维语中性化 ----
# engine/.agent 已被 artifacts 删除，保留文档中对它的引用改为发布副本的真实入口；
# pct exec 107 揭示生产宿主管理路径，中性化（经验：deploy/ 系文档含运维命令，需覆盖）
# 经验(2026-09-13)：docs/designs/ 设计工作稿被代码头注释与 e2e 用例按路径引用，
# 文件必须保留（不可入 artifacts 删除清单），但其 .agent 引用同样要修补——纳入本阶段。
REF_TARGETS = sorted(glob.glob("docs/*.md") + glob.glob("docs/designs/*.md") +
                     glob.glob("*/REGRESSIONS.md") +
                     glob.glob("deploy/*.md") + glob.glob("deploy/*/*.md"))
REF_RULES = [
    (r'`bash \.agent/verify\.sh`', '`make verify`'),
    (r'`\.agent/verify\.sh`', '`make verify`'),
    (r'bash \.agent/verify\.sh', 'make verify'),
    (r'、`\.agent/test-gates\.md`', ''),
    (r'、`\.agent/[^`*]+`', ''),
    (r'pct exec 107', 'pct exec <CTID>'),
]
ref_changed = []
for path in REF_TARGETS:
    try:
        raw = open(path, 'rb').read().decode('utf-8').replace('\r\n', '\n')
    except (UnicodeDecodeError, FileNotFoundError):
        continue
    text = raw
    for pat, rep in REF_RULES:
        text = re.sub(pat, rep, text)
    if text != raw:
        ref_changed.append(path)
        if not dry:
            open(path, 'wb').write(text.encode('utf-8'))
if ref_changed:
    print('[remarks] 死链修补 %s %d 个:' % ('将改动' if dry else '了', len(ref_changed)))
    for p in ref_changed:
        print('       ' + p)

# ---- 第三阶段: docs/designs 设计工作稿的开发过程叙事清除 ----
# 经验(2026-09-13, 同步批次沉淀): AI 会话产出的功能设计稿有四类过程叙事不入发布，
# 可按通用类正则沉淀（文件本体保留——代码注释/e2e 按路径引用它们）：
#   a) 日期+轮次定稿标记（YYYY-MM-DD N轮定/轮修订/轮补/与用户对齐/定稿行）
#   b) 【已实现|补齐|偏离记录|实现口径修订 日期】实现状态注记
#   c) .agent 工作流引用（claim 协调段/status.md 回写/evidence 归档）
#   d) "同 commit 演进/写码前逐条确认"类协作纪律语
DESIGN_TARGETS = sorted(glob.glob("docs/designs/*.md"))
DESIGN_RULES = [
    # a) 引言行与决策记录标题的定稿叙事
    (r'^> \d{4}-\d{2}-\d{2} 设计讨论定稿（本文=开发依据；实现期发现与本文冲突时，先改本文再写码）。$',
     '> 本文为功能设计文档；实现与本文冲突时先修订本文。'),
    (r'^> \d{4}-\d{2}-\d{2} [一二三四五六七八九十]+轮定稿。', '> '),
    (r'^> 与设计文档同 commit 演进；实现期发现验收标准不可达/不合理，先改本文再改码。$',
     '> 实现发现验收标准不可达/不合理时，先修订本文再改码。'),
    (r'（与本文同 commit 演进）', ''),
    (r'^## 0\. 决策记录（\d{4}-\d{2}-\d{2} 与用户对齐）$', '## 0. 关键设计决策'),
    # a) 表格/小节标题内嵌的日期轮次标记
    (r'（\d{4}-\d{2}-\d{2} (?:与用户对齐|[一二三四五六七八九十]+轮定)）', ''),
    (r'（\d{4}-\d{2}-\d{2} [一二三四五六七八九十]+轮修订）：', '：'),
    (r'，\d{4}-\d{2}-\d{2} [一二三四五六七八九十]+轮补）', '）'),
    (r'，\d{4}-\d{2}-\d{2} [一二三四五六七八九十]+轮修订：', '：'),
    (r'（[一二三四五六七八九十]+轮修订：', '（'),
    (r'[一二三四五六七八九十]+轮定 (D\d) 后再进一步', r'\1 后再进一步'),
    (r'（本轮新增，[^）]*）', ''),
    (r'（何时丢弃，[^）]*）', ''),
    (r'（§4\.10，本轮重点）', '（§4.10）'),
    (r'（设计的地基，实现前不必重查）', '（设计地基）'),
    (r'^## 9\. 实现期核对点（写码前逐条确认）$', '## 9. 实现核对点'),
    (r'实现期核对点 §10-', '§9-'),
    # b) 实现状态注记括号
    (r'^(\s*)\*\*【已实现 \d{4}-\d{2}-\d{2}】\*\* ', r'\1- '),
    (r'^(\s*)\*\*【补齐 \d{4}-\d{2}-\d{2}】\*\* ', r'\1'),
    (r'\*\*【偏离记录\+补齐 \d{4}-\d{2}-\d{2}】\*\*', '**实现偏离与补齐**'),
    (r'\*\*【实现口径修订 \d{4}-\d{2}-\d{2}】\*\*', '**实现口径**'),
    (r'随 A19 补课交付（vscode-plugin test/）', '见 vscode-plugin test/'),
    # c) .agent 工作流引用（verify.sh 死链规则已在第二阶段覆盖 `bash .agent/verify.sh`）
    (r'(?<![\w./`])verify\.sh', 'make verify'),
    (r'新 ADR（engine `\.agent` 惯例，本机）', '新 ADR'),
    (r'新 ADR 落 engine `\.agent`（增量扫描设计决策：D1-D6 摘要\+偏离处）；',
     '新 ADR 记录增量扫描设计决策（D1-D6 摘要+偏离处）；'),
    (r'证据归档 `\.agent/evidence/`（U8）', '证据归档本机（不入 git）'),
    # d) 协作纪律语 + 会话协调切片注
    (r'（须 web 会话可认领时）', ''),
    (r'（需 sim 栈，注意会话锁）', '（需模拟栈）'),
    (r'\(incremental-scan\.md\) v4）', '(incremental-scan.md)）'),
    # 文档尾部"纪律清单（开发阶段）"整节删除（claim/U2/status.md 全是会话工作流）
    (r'\n## 10\. 纪律清单（开发阶段）\n[\s\S]*$', '\n'),
]
design_changed = []
for path in DESIGN_TARGETS:
    try:
        raw = open(path, 'rb').read().decode('utf-8').replace('\r\n', '\n')
    except (UnicodeDecodeError, FileNotFoundError):
        continue
    text = raw
    for pat, rep in DESIGN_RULES:
        text = re.sub(pat, rep if rep is not None else '', text, flags=re.M)
    text = re.sub(r'\n{3,}', '\n\n', text)
    text = re.sub(r'[ \t]+$', '', text, flags=re.M)
    if text != raw:
        design_changed.append(path)
        if not dry:
            open(path, 'wb').write(text.encode('utf-8'))
if design_changed:
    print('[remarks] 设计稿叙事清除 %s %d 个:' % ('将改动' if dry else '了', len(design_changed)))
    for p in design_changed:
        print('       ' + p)
PYEOF
}

# ============================================================
# 三、门禁
# ============================================================
mode_check() {
  local rc=0
  echo "== 遗留物检查（硬违例）=="
  local arts
  arts=$(scan_artifacts)
  if [ -n "$arts" ]; then
    echo "!! 发现开发态遗留物:"
    printf '%s\n' "$arts" | sed 's/^/     /'
    rc=1
  else
    echo "   通过"
  fi
  echo "== 过程性备注残留（人工确认；命中不阻断）=="
  local resid
  resid=$(grep -rnIE "迁移整改|V1\.x 教训|（[RNXBC][0-9]{1,2}）|矛盾[①-⑯]|修复⑪|评估报告 R[0-9]|旧文档|（V1\.[0-9] 定版）|落地 V1\.x|兑现 V1\.x" \
      --include="*.md" --exclude-dir=.git --exclude-dir=archive \
      --exclude-dir=.agent --exclude-dir=.agents --exclude-dir=.zcode . 2>/dev/null \
    | grep -vE "^\./README\.md:.*(LESSONS|playbooks|AGENTS)" || true)
  if [ -n "$resid" ]; then
    printf '%s\n' "$resid" | head -30 | sed 's/^/   ? /'
  else
    echo "   无残留"
  fi
  [ $rc -eq 0 ] && echo "release_clean: check 通过（备注残留请人工过目）" || echo "release_clean: check 未通过" >&2
  exit $rc
}

case "$MODE" in
  artifacts) mode_artifacts ;;
  remarks)   mode_remarks ;;
  check)     mode_check ;;
  all)       mode_artifacts; mode_remarks ;;
  *) sed -n '2,11p' "$0"; exit 2 ;;
esac
