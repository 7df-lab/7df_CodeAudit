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
# ============================================================
# 目录名精确匹配（-name），避免 find Emacs 正则的管道符转义坑
ARTIFACT_DIR_NAMES='.agent .agents .zcode archive'

scan_artifacts() {
  local dir_tests=""
  for d in $ARTIFACT_DIR_NAMES; do dir_tests+=" -name $d -o"; done
  dir_tests="${dir_tests% -o}"
    { find . -mindepth 1 -maxdepth 2 \
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
