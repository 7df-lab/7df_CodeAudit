// maka-runtime-host 方言适配器（平台切片 7）：launch（descriptor.sandbox_image 沙箱）
// → execIn 驱动 headless `maka run -`（stdin=prompt）→ stdout=finalText。
// 依据: 主仓库 docs/designs/platform-slice7-agent-selection.md §5；S2 Maka 完整回合
// 实证（证据 .agent/evidence/maka-s2-20260919/——三根因配置已烘焙镜像 maka-cli:s3）；
// baton/docs/maka-integration.md §2（v0 驱动面=maka run；ACP=v1 收敛，接口同形）。
//
// 诚实边界：transport 登记值 acp-stdio（进程驱动语义位）；v0 实现=每回合一次
// maka run 进程（首回合无 --continue，后续 --continue 接续）；结果=text-final
// 通道（structured_submit=false——assignment 模板兜底段契约，parseAuditResult 接住）。
package sandbox

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// maka 驱动常量（S2 实证配方：cwd=/sandbox/project 上传位；HOME 对齐镜像烘焙 root）。
const (
	makaExecEnv     = "HOME=/home/node"
	makaProjectPath = "/workspace/project"
	// makaTurnBudgetDefaultS — 回合总预算缺省秒。依据: 主仓库
	// docs/designs/platform-slice7-agent-selection.md 附录 C4（"总预算 30m cfg 化"
	// ——突破单次 exec 600s 钳制（07 §8 EXEC_TIMEOUT_MAX），轮询间隔 20s 同条）。
	// 缺省 1800=30m；env 可调（CODEAUDIT_MAKA_TURN_BUDGET_S，非法值回落缺省）。
	makaTurnBudgetDefaultS = 1800
)

// makaTurnBudgetS — 回合总预算（cfg 化：env 覆盖 > 缺省 30m；附录 C4）。
func makaTurnBudgetS() int {
	if v := os.Getenv("CODEAUDIT_MAKA_TURN_BUDGET_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return makaTurnBudgetDefaultS
}

// newMakaSession — open：launch（镜像经 effectiveImage=descriptor 优先）→ 上传项目
// → 健康面（maka --version）。bridge 不启动（非 sse-bridge 通道）。
func newMakaSession(ctx context.Context, r *ManagerRunner, t Task) (turnSession, error) {
	ls, err := r.launch(ctx, t.TaskID)
	if err != nil {
		return nil, err
	}
	if t.WorkspaceDir != "" {
		if err := r.uploadProject(ctx, ls, t.WorkspaceDir); err != nil {
			ls.teardown()
			return nil, fmt.Errorf("upload project: %w", err)
		}
	}
	if _, err := r.execIn(ctx, ls.url, ls.token, ls.ref.ID,
		"HOME=/home/node maka --version", 60); err != nil {
		ls.teardown()
		return nil, fmt.Errorf("maka healthcheck（cli-help）: %w", err)
	}
	// 复批 II（附录 C1）：Maka 复用 DSH 前端显示——humanLog 出口喂 .ai.log→WS
	// StreamAIInteractionLog；行协议对齐 dsh（══ 会话/── 轮/emoji 前缀——
	// parseTimeline EntryKind 分色兼容）。v1=回合粒度整段；逐字流=ACP 收敛批。
	r.humanLog("══ Maka 会话开始（maka-cli 沙箱）══\n")
	return &makaSession{ls: ls, r: r, taskID: t.TaskID, tasklog: t.TasklogDir}, nil
}

// makaSession — headless maka run 回合会话。
type makaSession struct {
	ls      *liveSession // 沙箱资源（url/token/ref/teardown 复用；bridge 通道未启动）
	r       *ManagerRunner
	turns   int    // 已发回合数（--continue 接续判据）
	failed  bool   // 上一回合 --continue 失败标记（兜底：下回合裸跑）
	taskID  string // 档案归属键（RuntimeEventLog 导出落盘 <tasklogDir>/<taskID>/）
	tasklog string // tasklog 根（沙箱回收前导出的落盘位；空=禁用投影导出）
	lastExp []byte // 最近一次导出的 RuntimeEventLog JSONL（teardown 前缓存）
}

// makaOutputContract — text-final 输出契约头（模型依从性补强：assignment 模板
// 兜底段存在但 glm 依从性漂移——sim gw-11ebc0 实证自然语言收敛无围栏 JSON。
// 前置强调=无工具提交通道方言的必要约束，与 dsh 的工具 schema 约束同位）。
const makaOutputContract = "重要：你的最终回复必须以一个 ```json 围栏代码块结尾。findings 数组每项必须给出 file_path（项目内相对路径字符串）与 start_line（行号整数），例如：{\"findings\":[{\"title\":\"...\",\"file_path\":\"app.py\",\"start_line\":7,\"confidence\":0.9}]}；没有发现时为空数组。围栏 JSON 之前的分析文字可以保留。\n\n"

// turn — `maka run -`（stdin=prompt）驱动单回合；stdout=finalText。
// 首回合裸跑（无既有任务可接续）；后续 --continue（同 cwd 兼容任务续）；
// --continue 失败兜底=裸跑重试一次（会话接续非硬依赖——设计 §8 风险对策）。
func (s *makaSession) turn(ctx context.Context, prompt string) (string, []ToolCall, error) {
	base := fmt.Sprintf(
		"HOME=/home/node maka run - --cwd %s --connection codeaudit-inference --yolo --timeout %d --max-steps 24",
		makaProjectPath, makaTurnBudgetS())
	if s.turns > 0 && !s.failed {
		base += " --continue"
	}
	s.r.humanLog(fmt.Sprintf("── 回合 %d ──\n📋 [任务下发]（%d 字节）\n", s.turns+1, len(prompt)))
	out, err := s.execStdin(ctx, base, makaOutputContract+prompt)
	if err != nil && s.turns > 0 && !s.failed {
		// --continue 接续失败（任务不存在/不兼容）：裸跑兜底一次并标记
		s.failed = true
		s.r.event("warn", "maka --continue 接续失败，裸跑兜底: %v", err)
		out, err = s.execStdin(ctx, strings.Replace(base, " --continue", "", 1), prompt)
	}
	if err != nil {
		return "", nil, err
	}
	s.turns++
	s.failed = false
	final := strings.TrimSpace(out)
	s.r.humanLog(fmt.Sprintf("💬 [Maka 回复]（%d 字节）\n%s\n", len(final), final))
	s.exportRuntimeEvents() // Maka 适配器（2026-09-22）：teardown 前抓 RuntimeEventLog
	return final, nil, nil  // text-final：无工具调用面
}

// makaExportScript — RuntimeEventLog 导出（runtime.sqlite → JSONL stdout）。
// 行形状与 baton maka 适配器输入=S2 样本信封（event_id/…/payload_json/committed_at）。
// python3+sqlite3 沙箱镜像内置；导出失败=投影禁用诚实降级，不影响回合产物。
const makaExportScript = `import sqlite3, json
db = sqlite3.connect('file:/home/node/.config/Maka/workspaces/default/runtime.sqlite?mode=ro', uri=True)
cols = 'event_id, session_id, invocation_id, run_id, turn_id, event_seq, event_kind, payload_json, committed_at'
rows = db.execute('SELECT ' + cols + ' FROM runtime_events ORDER BY committed_at, event_seq').fetchall()
for r in rows:
    print(json.dumps(dict(zip([c.strip() for c in cols.split(',')], r))))`

// exportRuntimeEvents — 沙箱回收前抓 RuntimeEventLog（"the log is the runtime"
// 落 runtime.sqlite——2026-09-22 sim 实锚，无裸 JSONL 文件）。写到
// <tasklog>/<taskID>/maka-events.jsonl（baton project -maka 的输入；tasklog 空=
// 禁用）。降级纪律同投影钩子：失败仅日志，不影响回合产物。
func (s *makaSession) exportRuntimeEvents() {
	if s.tasklog == "" || s.taskID == "" {
		return
	}
	res, err := s.r.execIn(context.Background(), s.ls.url, s.ls.token, s.ls.ref.ID,
		"HOME=/home/node python3 -c "+shQuote(makaExportScript), 60)
	if err != nil {
		s.r.event("warn", "[task %s] RuntimeEventLog 导出失败（投影禁用，诚实降级）: %v", s.taskID, err)
		return
	}
	stdout, _ := res["stdout"].(string)
	if !strings.Contains(stdout, `"event_kind"`) {
		s.r.event("warn", "[task %s] RuntimeEventLog 导出为空——无事件可投影", s.taskID)
		return
	}
	s.lastExp = []byte(stdout)
	dir := filepath.Join(s.tasklog, s.taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.r.event("warn", "[task %s] tasklog 目录创建失败: %v", s.taskID, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "maka-events.jsonl"), s.lastExp, 0o644); err != nil {
		s.r.event("warn", "[task %s] maka-events.jsonl 落盘失败: %v", s.taskID, err)
		return
	}
	s.r.event("info", "[task %s] RuntimeEventLog 已导出（maka-events.jsonl %d 字节）——baton -maka 投影输入就绪", s.taskID, len(s.lastExp))
}

// execStdin — 长回合两段式（复批 II C4）：manager 单次 exec 上限 600s（R27）——
// maka run 改后台启动（nohup+完成标记）→20s 轮询标记（单轮探测 <600s 安全）→
// 完成后读输出文件。总预算=makaTurnBudgetS（缺省 30m，env 可调——附录 C4 cfg 化；
// 不再受单次 exec 钳制）。非 maka run
// 短脚本（健康面/诊断）直走 execOnce。
func (s *makaSession) execStdin(ctx context.Context, script, stdin string) (string, error) {
	if !strings.Contains(script, "maka run") {
		return s.execOnce(ctx, script, stdin, 60)
	}
	const outPath, donePath, stdinPath = "/tmp/maka-out.txt", "/tmp/maka-done", "/tmp/maka-stdin"
	// stdin 经文件注入（后台进程不接 exec stdin；maka run - 从 stdinPath 读）
	if stdin != "" {
		w := fmt.Sprintf("base64 -d > %s <<'B64EOF'\n%s\nB64EOF", stdinPath, base64.StdEncoding.EncodeToString([]byte(stdin)))
		if _, err := s.execOnce(ctx, w, "", 60); err != nil {
			return "", err
		}
	}
	bg := fmt.Sprintf("rm -f %s %s; ( /bin/bash -c %s < %s > %s 2>&1; echo DONE > %s ) & echo started",
		donePath, outPath, shQuote(script), stdinPath, outPath, donePath)
	if _, err := s.execOnce(ctx, bg, "", 90); err != nil {
		return "", err
	}
	budgetS := makaTurnBudgetS()
	deadline := time.Now().Add(time.Duration(budgetS) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(20 * time.Second):
		}
		res, err := s.execOnce(ctx, "cat "+donePath+" 2>/dev/null || true", "", 30)
		if err == nil && strings.Contains(res, "DONE") {
			out, rerr := s.execOnce(ctx, "cat "+outPath+"; echo EXITMARK$?", "", 120)
			if rerr != nil {
				return "", rerr
			}
			return s.parseRunOutput(out)
		}
	}
	return "", fmt.Errorf("maka run 超时（%ds 轮询预算耗尽）", budgetS)
}

// execOnce — 单次 manager exec（stdin_b64；timeout 钳 580<600 上限）。
func (s *makaSession) execOnce(ctx context.Context, script, stdin string, timeoutS int) (string, error) {
	if timeoutS > 580 {
		timeoutS = 580 // manager exec 上限 600s（R27 EXEC_TIMEOUT_MAX；07 §8）取 580 余量
	}
	res, err := s.r.call(ctx, "POST", s.ls.url+"/api/v1/sandboxes/exec", s.ls.token, map[string]any{
		"sandbox_id":      s.ls.ref.ID,
		"command":         []string{"/bin/bash", "-c", script},
		"timeout_seconds": timeoutS,
		"env":             map[string]any{"HOME": "/home/node"},
		"stdin_b64":       base64.StdEncoding.EncodeToString([]byte(stdin)),
	})
	if err != nil {
		return "", err
	}
	if code, _ := res["exit_code"].(float64); int(code) != 0 {
		stderr, _ := res["stderr"].(string)
		stdout, _ := res["stdout"].(string)
		return "", fmt.Errorf("maka exec exit %d: %s%s", int(code), tail(stdout, 200), tail(stderr, 300))
	}
	out, _ := res["stdout"].(string)
	return out, nil
}

// parseRunOutput — 输出文件读取拆分（正文 + EXITMARK 退出码）。
func (s *makaSession) parseRunOutput(raw string) (string, error) {
	idx := strings.LastIndex(raw, "EXITMARK")
	if idx < 0 {
		return strings.TrimSpace(raw), nil
	}
	body, exitStr := raw[:idx], strings.TrimSpace(raw[idx+8:])
	if exitStr != "0" {
		return "", fmt.Errorf("maka run exit %s: %s", exitStr, tail(body, 400))
	}
	return strings.TrimSpace(body), nil
}

// shQuote — 单引号安全包裹（脚本内嵌为 bash -c 参数）。
func shQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// close — 沙箱回收（teardown 恒回收纪律同 dsh）。幂等（teardown 自带闸）。
func (s *makaSession) close() { s.ls.teardown() }

// name — 会话标识（沙箱 name，同 dsh 口径）。
func (s *makaSession) name() string {
	if s.ls.ref.Name != "" {
		return s.ls.ref.Name
	}
	return s.ls.ref.ID
}

// turnsContribution — 每回合独立进程无跨回合轮号面（0 即无偏移，R98 语义）。
func (s *makaSession) turnsContribution() float64 { return 0 }

// errorTail — 失败诊断（尽力而为）：maka state root 日志尾。
func (s *makaSession) errorTail(ctx context.Context) string {
	dctx, cancel := context.WithTimeout(ctx, 15e9)
	defer cancel()
	res, err := s.execStdin(dctx, "tail -c 800 /home/node/.config/Maka/workspaces/default/diagnostic.log 2>/dev/null || true", "")
	if err != nil || res == "" {
		return ""
	}
	return tail(res, 800)
}
