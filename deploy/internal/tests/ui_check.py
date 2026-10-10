#!/usr/bin/env python3
"""
ui_check — GUI 黑盒门禁单入口（manual-test-guide §2 口径；2026-09-07 收编
gui_streaming_check + ui_walkthrough_check 两脚本为一个文件两种模式）

模式一 · 全流程闭环（默认，无 --task）：
  登录 → 新建项目（UI 上传压缩包）→ toast 三连+自动跳任务详情 →
  运行期观察（执行日志卡/AI 交互日志卡渲染）+ 流式判据 v3 内联（自建任务的
  AI 阶段即观测窗——面板必须跟随后端实质产出直至终态）→ 终态（API 轮询口径，
  勿用页面文本判终态：AI 对话内容含"失败"字样会误判）→ 发现/融合视图 →
  风险详情深检（ADR-195 链路点选定位：蓝=链路行/黄=漏洞行；ADR-158 污点链
  SOURCE→SINK 或非 taint 诚实"不推测"；人工裁决回写：verdict 与 reasoning
  都须落库，R-30 锚）→ 报告深链 ?task= + 在线查看（新标签页渲染原始 JSON）→
  通知中心 → 仓库型(git clone)项目 UI 呈现核验。
  每步截图存证；核心断言任一失败 = 退出码 1。

模式二 · 挂载流式（--task <RUNNING任务>）：
  对一个 RUNNING 任务跑流式判据——快速验证流式链路（WS/网关推流/前端吸收）
  修复，不必整套创建流走 8~12 分钟。
  退出码：0=面板全程跟随后端产出；1=断裂（后端实质产出而面板 LagTolerance
  秒无变化）；2=inconclusive（窗内后端零产出：RuleScan 降级/工具静默期/未到
  AI 阶段——换 AI 活跃任务重跑或先核对沙箱链路）。

流式判据（v3，原 gui_streaming_check 内核，语义一字不改）：
  v2 双教训（2026-09-07 实证）：① 字节级比较假 FAIL——面板是人性化渲染文本，
  后端 cursor 是原始字节，天然 ±几十字节漂移（gw-d0760857 ±33B 仍被误杀）；
  ② 零产出空真 PASS——降级 RuleScan 时 0≥0 恒真（gw-024011e0 53s 假 PASS）。
  v3 进度耦合：面板"变化"（增减都算活，兼容 400 条窗口截断汰旧换新）重置时钟；
  后端自上次变化以来实质产出 ≥64B 且超 8s 面板无变化 → FAIL；终态后收敛采样
  （连续 2 拍稳定或再等一个容忍窗）；全程零产出 → inconclusive。

依赖：本机 playwright（chromium 已装）。
用法：python3 deploy/tests/ui_check.py [--base http://192.0.2.122:18088]
       python3 deploy/tests/ui_check.py --task gw-xxx [--timeout 300]
"""
import argparse
import json
import os
import re
import sys
import time
import urllib.request
import zipfile
from pathlib import Path

from playwright.sync_api import sync_playwright

BOX = "[data-testid='ai-interaction-log-box']"
LagTolerance = 8.0        # 后端实质产出后，允许面板跟随的秒数（50ms 合并窗+渲染余量）
MaterialProduction = 64   # 视为"实质产出"的后端新增字节（渲染层字节差噪声不计数）
TERMINAL = ("TASK_STATUS_COMPLETED", "TASK_STATUS_FAILED", "TASK_STATUS_TIMEOUT", "TASK_STATUS_DEAD", "TASK_STATUS_CANCELLED")

RESULTS = []


class _SkipDeepCheck(Exception):
    """深检在 AI 降级窗口内按 INCONCLUSIVE 豁免后跳过剩余断言（非失败）。"""


def _admin_token(args):
    body = json.dumps({"username": args.user, "password": args.password}).encode()
    api_base = args.base.rstrip("/").replace(":18088", ":18080")
    req = urllib.request.Request(api_base + "/v1/auth/login",
                                 data=body, headers={"Content-Type": "application/json"}, method="POST")
    return json.loads(urllib.request.urlopen(req, timeout=15).read() or b"{}").get("access_token", "")


def _task_degraded(page, args):
    """判定当前详情页任务是否处于 AI 降级（RuleScan 兜底）窗口——
    降级任务的发现无 AI reasoning hops，链路点选断言面不适用（R56/R57）。"""
    try:
        tid = page.url.split("/tasks/")[-1].split("?")[0].split("/")[0]
        if not tid.startswith("gw-"):
            return False
        tok = _admin_token(args)
        # 网关 decodeQuery 只认 JSON 对象分页形态（transcode.go decodeQuery 字段表）——
        # 标量 page_size 会被 DiscardUnknown 静默丢弃（默认首页 20 条，降级标记在 21+ 条时漏判）
        import urllib.parse as _up
        q = _up.quote('{"page_size":100}')
        req = urllib.request.Request(
            f"{args.base.rstrip('/')}/v1/findings?task_id={tid}&pagination={q}",
            headers={"Authorization": f"Bearer {tok}"})
        data = json.loads(urllib.request.urlopen(req, timeout=15).read() or b"{}")
        for f in (data.get("findings") or []):
            rid = f.get("source_rule_id") or ""
            rs = f.get("ai_reasoning") or ""
            if rid.startswith("rulescan-fallback:") or rs.startswith("[降级"):
                return True
    except Exception:
        pass
    return False


def check(name, ok, detail=""):
    RESULTS.append((name, bool(ok)))
    print(f"{'✓' if ok else '✗'} {name}{('  [' + detail + ']') if detail else ''}", flush=True)


def api(base, method, path, token=None, payload=None):
    req = urllib.request.Request(base + path, method=method)
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    data = json.dumps(payload).encode() if payload is not None else None
    if data:
        req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, data=data, timeout=15) as r:
        return json.loads(r.read().decode())


def login(page, base, user, password):
    page.goto(base, wait_until="domcontentloaded")
    page.locator("input").nth(0).fill(user)
    page.locator("input").nth(1).fill(password)
    page.get_by_role("button", name="登 录").click()
    # 登录后落总览'/'（App P4：登录后落总览页；旧口径'/'仅重定向 /projects 已失效）——判离登录页
    page.wait_for_url(lambda url: not url.rstrip("/").endswith("/login"), timeout=15_000)


def snap_status(snap):
    return snap.get("status") or (snap.get("task") or {}).get("status") or ""


# ---------------- 流式判据 v3（两模式共用内核） ----------------

def run_stream_verdict(page, base, task, token, timeout_s, shot_dir=None, on_beat=None):
    """返回 (verdict, produced_total)——verdict ∈ pass/fail/inconclusive。
    on_beat(status, panel_bytes, beat_idx) 供全流程模式附加观察（每 2 拍一次回调，
    用于执行日志卡/AI 卡渲染检测，避免每拍读整页 body）。"""
    produced_total = 0        # 观察到的后端最大 cursor
    panel_prev = None         # 上一拍面板字节数（None=首拍）
    panel_last_change_t = None
    produced_since_change = 0 # 面板上次变化以来的后端累计新增字节
    verdict = "inconclusive"
    terminal_at = None        # 观察到终态的时刻（进入收敛采样）
    stable = 0                # 终态后面板连续未变拍数
    beat = 0
    start = time.time()
    while time.time() - start < timeout_s:
        now = time.time()
        snap = api(base, "GET", f"/v1/tasks/{task}/snapshot", token)
        cursor = int(snap.get("ai", {}).get("next_cursor") or 0)
        status = snap_status(snap)
        if page.query_selector(BOX):
            panel_bytes = len((page.eval_on_selector(BOX, "el => el.textContent") or "").encode("utf-8"))
        else:
            panel_bytes = 0

        if cursor > produced_total:
            produced_since_change += cursor - produced_total
            produced_total = cursor

        changed = panel_prev is not None and panel_bytes != panel_prev
        if changed or panel_prev is None:
            panel_last_change_t = now
            if changed:
                produced_since_change = 0
        panel_prev = panel_bytes

        note = ""
        if status in TERMINAL:
            if terminal_at is None:
                terminal_at = now
            note = f" terminal+{now - terminal_at:.0f}s stable={stable}"

        print(f"[{now-start:6.1f}s] task={status} backend_cursor={cursor} "
              f"panel_bytes={panel_bytes} pending={produced_since_change}{note}", flush=True)
        if shot_dir:
            shot_dir.mkdir(parents=True, exist_ok=True)
            page.screenshot(path=str(shot_dir / f"stream-{int(now-start):03d}.png"))
        if on_beat and beat % 2 == 0:
            on_beat(status, panel_bytes, beat)

        # 核心判据：后端实质产出而面板超过容忍窗无任何变化 → 断裂
        if (produced_since_change >= MaterialProduction
                and now - panel_last_change_t > LagTolerance):
            verdict = "fail"
            break

        if terminal_at is not None:
            stable = stable + 1 if not changed else 0
            # 终态收敛：面板已稳定，或终态后再给足一个容忍窗
            if stable >= 2 or now - terminal_at > LagTolerance:
                verdict = "inconclusive" if produced_total == 0 else "pass"
                break
        time.sleep(2)
        beat += 1
    return verdict, produced_total


# ---------------- 模式二：挂载流式 ----------------

def mode_attach(args):
    token = api(args.base, "POST", "/v1/auth/login",
                payload={"username": args.user, "password": args.password})["access_token"]
    # D1（计划档 2026-09-29）：任务形态分派——graph 分支前置恒 return，其后只剩
    # 平面任务（TURN 收束判据 → 既有流式判据 v3 零变化）。
    def snap():
        return api(args.base, "GET", f"/v1/tasks/{args.task}/snapshot", token)

    snap0 = snap()
    execution = ((snap0.get("task") or {}).get("config") or {}).get("execution") or ""
    if execution == "graph":
        with sync_playwright() as p:
            browser = p.chromium.launch()
            page = browser.new_page(viewport={"width": 1600, "height": 1000})
            login(page, args.base, args.user, args.password)
            rc = run_graph_checks(page, args.base, args, token)
            browser.close()
        return rc
    # TP20-T2 切换全路径（计划 §4）：首态 RUNNING 的平面任务先走六断言+二次切换
    # （任务被暂停/恢复/换笔——后续流式判据在接续流上继续采样）；非 RUNNING 跳过。
    if snap_status(snap0) == "TASK_STATUS_RUNNING":
        with sync_playwright() as p:
            browser = p.chromium.launch()
            page = browser.new_page(viewport={"width": 1600, "height": 1000})
            login(page, args.base, args.user, args.password)
            rc_sw = run_agent_switch_checks(page, args.base, args, token)
            browser.close()
        if rc_sw == 1:
            return 1
        snap0 = snap()  # 切换路径后的新态势（流式判据此后从当前位起评）
        if snap_status(snap0) in TERMINAL:
            print(f"NOTE: 切换路径后任务已终态（{snap_status(snap0)}）——流式判据跳过")
            return 0
    # 粒度分派（b4）：TURN 型（maka 等回合制 agent）无逐字增量——v3 流式
    # 判据的"面板停滞=断链"假设不适用（D1 批实证：maka 任务误报 FAIL）。
    # TURN 型改用收束判据：终态后面板有内容实体即通。
    # P3-1：粒度在条目建立后才可见（早挂载/键缺失/零值 UNSPECIFIED——protojson
    # EmitUnpopulated 显式下发零值枚举，三者都需重取确证）——先等信号。
    gran = (snap0.get("ai") or {}).get("granularity") or ""
    if not gran or gran == "AI_LOG_GRANULARITY_UNSPECIFIED":
        probe_deadline = time.time() + 90
        while time.time() < probe_deadline and (not gran or gran == "AI_LOG_GRANULARITY_UNSPECIFIED"):
            time.sleep(5)
            snap0 = snap()
            gran = (snap0.get("ai") or {}).get("granularity") or ""
            if snap_status(snap0) in TERMINAL:
                break
    if gran == "AI_LOG_GRANULARITY_TURN":
        with sync_playwright() as p:
            browser = p.chromium.launch()
            page = browser.new_page(viewport={"width": 1600, "height": 1000})
            login(page, args.base, args.user, args.password)
            page.goto(f"{args.base}/tasks/{args.task}", wait_until="domcontentloaded")
            page.wait_for_selector(BOX, timeout=15_000)
            deadline = time.time() + max(args.timeout, 60)
            box_text = ""
            st = ""
            ai_bytes = 0
            degraded = False
            while time.time() < deadline:
                box_text = page.locator(BOX).first.inner_text()
                body = snap()
                st = snap_status(body)
                ai_bytes = int((body.get("ai") or {}).get("total_bytes") or 0)
                # P2-1：后端确有产出 + 面板呈内容实体而非占位文案（方言中立判别：
                # 占位特征串"无逐字流"在场=面板仍为空态——dsh 方言标记 ✍/💭/── 第
                # 在 maka 方言面板不出现，标记集判别对 TURN 方言过严会烧满窗口）
                degraded = _task_degraded(page, args)
                if st in TERMINAL and (degraded or
                                       (ai_bytes > 0 and "无逐字流" not in box_text)):
                    break
                page.wait_for_timeout(3000)
            browser.close()
        # P2-1 终门与 break 条件同谓词（超时退出路径不得弱于提前退出——占位
        # 文案面板+后端有产出=推流断裂形态，终门漏检即假 PASS）
        marked = "无逐字流" not in box_text  # 占位特征不在场=面板有内容实体
        if st in TERMINAL and (degraded or (ai_bytes > 0 and marked)):
            print(f"PASS: TURN 粒度收束判据（终态={st}，后端 {ai_bytes}B，面板 {len(box_text)}B{'' if not degraded else '，降级豁免'}）")
            return 0
        print(f"FAIL: TURN 粒度收束判据未达（终态={st or '未到'}, 后端 {ai_bytes}B, 面板标记={marked}）")
        return 1
    with sync_playwright() as p:
        browser = p.chromium.launch()
        page = browser.new_page(viewport={"width": 1600, "height": 1000})
        login(page, args.base, args.user, args.password)
        page.goto(f"{args.base}/tasks/{args.task}", wait_until="domcontentloaded")
        page.wait_for_selector(BOX, timeout=15_000)
        verdict, produced = run_stream_verdict(
            page, args.base, args.task, token, args.timeout,
            shot_dir=Path(args.shots) if args.shots else None)
        browser.close()
    if verdict == "pass":
        print("PASS: 面板全程跟随后端产出直至终态（流式增量链路通）")
        return 0
    if verdict == "fail":
        print(f"FAIL: 后端实质产出（≥{MaterialProduction}B）后面板 {LagTolerance}s 无变化"
              f"——推流/吸收链断裂（backend_cursor={produced}）")
        return 1
    # 超时未终态与真零产出共用 return 2——2026-10-03 实录：切换接续流 300s 窗超时
    # （后端实产 101KB）被"零产出"文案误导排障方向。按 produced 分流话术。
    if produced > 0:
        print(f"INCONCLUSIVE: 采样窗超时未到终态（后端已产出 {produced}B，链路在产）"
              "——扩窗重跑（--timeout 增大）")
    else:
        print("INCONCLUSIVE: 采样窗内后端零产出（RuleScan 降级/工具执行静默期/未到 AI 阶段）"
              "——换 AI 活跃任务重跑或先核对沙箱链路")
    return 2



# ---------------- 切换 Agent 全路径断言族（TP20-T2，伞仓计划 §4） ----------------

def _task_agent(snap):
    return ((snap.get("task") or {}).get("agent")) or ""


def _logs_text(snap):
    return "\n".join((l or {}).get("message") or "" for l in ((snap.get("logs") or {}).get("logs") or []))


def _box_text(page):
    try:
        return page.locator(BOX).first.inner_text()
    except Exception:
        return ""


def run_agent_switch_checks(page, base, args, token):
    """TP20-T2 切换 Agent 全路径（六断言①-⑥ + SW0 Modal 契约联测 + PAUSED 态二次切换②④）。

    前提：attached 任务 RUNNING 且 /v1/agents 存在第二 agent。流程：
    GUI「暂停并切换 Agent」→ ①PAUSED ②专行 ③AI marker ④agent 标签 →
    PAUSED 态二次切换（②④ 再证）→ GUI 恢复 → ⑤同一流连续+新回合「任务下发」→
    ⑥findings 计数不回退。最终 agent=二次切换目标（缺省回归——续跑面不依赖
    第二 agent 镜像健康，方言内部行为归 e2e_agent_cases E 族）。返回 0/1/None（跳过）。
    """
    tid = args.task
    def snapd():
        return api(base, "GET", f"/v1/tasks/{tid}/snapshot", token)
    agents = (api(base, "GET", "/v1/agents", token) or {}).get("agents") or []
    cur = _task_agent(snapd()) or next((a["id"] for a in agents if a.get("is_default")), "")
    others = [a["id"] for a in agents if a["id"] != cur]
    if not others:
        print("NOTE: /v1/agents 无第二 agent——切换全路径跳过（清单=单条目部署）")
        return None
    target, back = others[0], cur  # 二次切换回归原 agent（缺省档），续跑面已知健康
    page.goto(f"{base}/tasks/{tid}", wait_until="domcontentloaded")
    page.wait_for_selector(BOX, timeout=15_000)

    def findings_count():
        # R135（缺损批 A6）：pagination.total 字段不存在于 PaginationResponse（只有
        # has_next/next_cursor）——原读法恒 0（SW6 空转的测量面根因）。页游走计数。
        n, cursor = 0, ""
        for _ in range(20):  # 20×100=2000 条护栏
            url = f"/v1/findings?task_id={tid}&page_size=100" + (f"&cursor={cursor}" if cursor else "")
            d = api(base, "GET", url, token) or {}
            n += len(d.get("findings") or [])
            pg = d.get("pagination") or {}
            cursor = pg.get("next_cursor") or ""
            if not pg.get("has_next"):
                break
        return n

    deadline = time.time() + max(getattr(args, "switch_timeout", 300), 120)  # A1：切换段独立预算
    f0, ai0 = findings_count(), int((snapd().get("ai") or {}).get("total_bytes") or 0)

    # —— 第一次切换（RUNNING 态：暂停并切换）——
    page.get_by_role("button", name="暂停并切换 Agent").click()
    page.wait_for_selector(".ant-modal", timeout=10_000)
    # 当前项禁用+「当前」徽标（Modal 契约联测）
    check("SW0 当前项禁用+「当前」徽标", "当前" in page.locator(".ant-modal").inner_text()
          and page.locator(f".ant-modal label:has-text('{cur}')").first.get_attribute("class").find("disabled") >= 0,
          f"cur={cur}")
    page.locator(f".ant-modal .ant-radio-wrapper:has-text('{target}')").first.click()
    page.get_by_role("button", name="确认切换").click()
    page.wait_for_selector(".ant-modal", state="detached", timeout=10_000)

    st = ""
    while time.time() < deadline and st != "TASK_STATUS_PAUSED":
        page.wait_for_timeout(3000)
        st = snap_status(snapd())
    check("SW1 切换后任务态 PAUSED", st == "TASK_STATUS_PAUSED", f"status={st}")

    snap1 = snapd()
    check("SW2 执行日志出现切换专行", "切换 Agent" in _logs_text(snap1)
          and target in _logs_text(snap1), _logs_text(snap1)[-160:])
    marker_at = None
    while time.time() < deadline and marker_at is None:  # marker=冻结回调写（回合边界，滞后于状态位）
        page.wait_for_timeout(3000)
        bt = _box_text(page)
        if "── 切换 Agent" in bt and target in bt:
            marker_at = bt.find("── 切换 Agent")
    check("SW3 AI 时间线出现切换 marker", marker_at is not None,
          (_box_text(page)[max(0, (marker_at or 0) - 20):][:80]) if marker_at is not None else "窗口内未见 marker")
    check("SW4 header agent 标签变更", _task_agent(snapd()) == target,
          f"task.agent={_task_agent(snapd())!r} want={target!r}")

    # —— PAUSED 态二次切换（②④ 再证：同款 GUI 通道走 SetTaskAgent）——
    page.get_by_role("button", name="切换 Agent").click()
    page.wait_for_selector(".ant-modal", timeout=10_000)
    page.locator(f".ant-modal .ant-radio-wrapper:has-text('{back}')").first.click()
    page.get_by_role("button", name="确认切换").click()
    page.wait_for_selector(".ant-modal", state="detached", timeout=10_000)
    snap2, logs2 = None, ""
    while time.time() < deadline:
        page.wait_for_timeout(2000)
        snap2 = snapd()
        logs2 = _logs_text(snap2)
        if "切换 Agent" in logs2 and _task_agent(snap2) == back:
            break
    check("SW5 PAUSED 二次切换专行+标签（②④）",
          "切换 Agent" in logs2 and _task_agent(snap2 or {}) == back,
          f"agent={_task_agent(snap2 or {})!r} want={back!r}")

    # —— 恢复与接续（⑤⑥）——
    page.get_by_role("button", name="恢复任务").click(timeout=10_000)
    dispatch_before = _box_text(page).count("任务下发")
    ai_at_pause = int((snapd().get("ai") or {}).get("total_bytes") or 0)
    ok5 = False
    while time.time() < deadline:
        page.wait_for_timeout(4000)
        snapd_ = snapd()
        bt = _box_text(page)
        ai_now = int((snapd_.get("ai") or {}).get("total_bytes") or 0)
        if bt.count("任务下发") > dispatch_before and ai_now >= ai_at_pause:  # 同一流（游标单调不减）+新回合任务下发
            ok5 = True
            st = snap_status(snapd_)
            break
        if snap_status(snapd_) in TERMINAL:
            st = snap_status(snapd_)
            break
    check("SW5+ resume 后同一流连续+新回合「任务下发」", ok5, f"dispatch={dispatch_before}→{_box_text(page).count('任务下发')} status={snap_status(snapd())}")
    f1 = findings_count()
    if snap_status(snapd()) not in TERMINAL:  # 未终态：等终窗收 ⑥（终态=findings 定格）
        while time.time() < deadline and snap_status(snapd()) not in TERMINAL:
            page.wait_for_timeout(5000)
    f2 = findings_count()
    # 缺损批 A6（2026-10-02）：空转防呆——f0 恒 0 时「不回退」无牙（首切换时点
    # findings 尚未落库是常态）。联动执行日志「沙箱产出 N 项发现」：N>0 则终值
    # 必须 ≥1（有产出必在场，切换/接续不得丢）；全零如实注记空转形态。
    produced = sum(int(m) for m in __import__("re").findall(r"沙箱产出 (\d+) 项发现", _logs_text(snapd())))
    sw6_ok = f2 >= f0 and f1 >= f0 and (produced == 0 or f2 >= 1)
    note = f"切换前={f0} 二次切换后={f1} 现值={f2} 沙箱产出累计={produced}" + ("" if produced else "（空转形态：切换窗内零产出——如需有牙形态用带发现任务先产出再切）")
    check("SW6 已提交 findings 计数不回退（产出联动）", sw6_ok, note)
    sw = [ok for name, ok in RESULTS if name.startswith("SW")]
    return 0 if all(sw) else 1

# ---------------- 图任务断言族（D1：并入主干，原 evidence 专用脚本 batch5-gui-de.py 移植+b4 扩面） ----------------

def run_graph_checks(page, base, args, token):
    """图任务 GUI 断言 G1-G5（终态任务最佳——通道内容完整；运行中亦可跑，判据用轮询）。
    granularity 判据自校准：期望值取自 REST 契约元数据（b4 AILogGranularity），
    GUI 文案与之一致才 PASS——单脚本对 token/turn 双粒度样本通用。"""
    tid = args.task
    page.goto(f"{base}/tasks/{tid}", wait_until="domcontentloaded")
    page.wait_for_timeout(2500)

    def snap_auth():
        return api(base, "GET", f"/v1/tasks/{tid}/snapshot", token)

    def graph():
        return api(base, "GET", f"/v1/tasks/{tid}/graph", token)

    # G1 图卡渲染：画布挂载+图 API 节点非空（终态任务立即成立）
    g = graph()
    nodes = g.get("nodes") or []
    deadline = time.time() + 60
    while not nodes and time.time() < deadline:
        page.wait_for_timeout(3000)
        g = graph()
        nodes = g.get("nodes") or []
    canvas = page.locator("[data-testid='g6-container'] canvas")
    g1_ok = canvas.count() > 0 and len(nodes) > 0
    check("G1 图卡渲染（画布挂载+节点非空）", g1_ok, f"nodes={len(nodes)}")
    if not g1_ok:  # P3-3：画布未挂载时后续断言依赖 bounding_box——短路防 TypeError 炸脚本
        print("GRAPH-CHECKS: 0/1 pass（G1 失败短路）")
        return 1

    # G2 弹层+通道时间线：G6 节点绘制在 canvas 内（无 DOM 锚）——网格扫描点击
    # 直到命中节点开弹层（LR 布局行距/视口变换不定，单点中心不可靠）
    pop = page.locator("[data-testid='graph-node-popup']")
    g6 = page.locator("[data-testid='g6-container']")  # 惰性 locator，持有等价于重建
    body = ""
    pop_ok = False
    box = g6.first.bounding_box()
    # 2D 网格（LR 布局多列多行——X 单线扫描漏列）
    grid = [(fx, fy) for fy in (0.5, 0.32, 0.68, 0.4, 0.6) for fx in (0.5, 0.25, 0.75, 0.12, 0.88, 0.37, 0.63)]
    for fx, fy in grid:
        g6.first.click(
            position={"x": box["width"] * fx, "y": box["height"] * fy})
        page.wait_for_timeout(800)
        if pop.count() == 1:
            pop_ok = True
            body = pop.first.inner_text()
            break
        # 点空白处关弹层（P2-3）——继续扫描
    check("G2 节点弹层挂载+通道时间线非空", pop_ok and len(body) > 40,
          body[:60].replace("\n", " ") if body else "popup 未挂载")

    # G3 granularity 文案自校准（b4 GUI 级验收核心）：REST 契约元数据 vs 弹层文案
    snap = snap_auth()
    ai_meta = (snap.get("ai") or {})
    gran = ai_meta.get("granularity") or ""
    chans = ai_meta.get("channels") or []
    if not gran and chans:
        gran = chans[0].get("granularity") or ""
    expect_turn = gran == "AI_LOG_GRANULARITY_TURN"
    has_banner = "整回合返回结论" in body
    # 契约未声明（UNSPECIFIED/盘回落旧档）=GUI 回落 agent 判别——此时 banner 可能
    # 出现（turn 型 agent 名判别）也可能不出现，判据降级为"不误报 token 断言"
    if gran in ("AI_LOG_GRANULARITY_TOKEN", "AI_LOG_GRANULARITY_TURN"):
        check("G3 granularity 契约↔GUI 文案一致", has_banner == expect_turn,
              f"granularity={gran} banner={has_banner}")
    else:
        check("G3 granularity 未声明（回落 agent 判别，不做一致性判死）", True,
              f"granularity={gran or 'absent'}")
    page.keyboard.press("Escape")
    page.wait_for_timeout(400)

    # G4 分区汇总 Modal
    page.get_by_role("button", name="分区汇总").first.click()
    page.wait_for_timeout(1200)
    modal = page.locator(".ant-modal:visible")
    mbody = modal.first.inner_text() if modal.count() else ""
    check("G4 分区汇总 Modal 可开+内容非空", modal.count() > 0 and len(mbody) > 30,
          mbody[:50].replace("\n", " "))
    page.keyboard.press("Escape")
    page.wait_for_timeout(400)

    # G5 下载完整日志（Q10 面）：点击必须发生在 expect_download 上下文内
    # （Tooltip 包裹下 role 定位偶发不稳——失败回落 has-text+force）
    dl_ok = False
    dl_name = ""
    dl_label = "下载完整日志"  # 三处选择器共用——字面量漂移=回落机制失效
    try:
        page.get_by_role("button", name=dl_label).first.scroll_into_view_if_needed(timeout=8_000)
        try:
            with page.expect_download(timeout=20_000) as dl:
                page.get_by_role("button", name=dl_label).first.click(timeout=8_000)
        except Exception:
            with page.expect_download(timeout=20_000) as dl:
                page.locator(f"button:has-text('{dl_label}')").first.click(force=True)
        d = dl.value
        dl_name = d.suggested_filename
        d.cancel()
        dl_ok = len(dl_name) > 0
    except Exception as e:
        dl_ok = False
        dl_name = str(e)[:60]
    check("G5 下载完整日志（触发下载事件）", dl_ok, dl_name)

    if args.shots:
        sd = Path(args.shots)
        sd.mkdir(parents=True, exist_ok=True)
        page.screenshot(path=str(sd / "graph-checks-final.png"))
    failed = [n for n, okk in RESULTS if not okk]
    print(f"GRAPH-CHECKS: {len(RESULTS) - len(failed)}/{len(RESULTS)} pass")
    return 0 if not failed else 1


# ---------------- 模式一：全流程闭环 ----------------

def mode_walkthrough(args):
    shots = Path(args.shots)
    shots.mkdir(parents=True, exist_ok=True)

    def snap(n):
        name = f"{n:02d}.png" if isinstance(n, int) else f"{n}.png"
        page.screenshot(path=str(shots / name))

    sample = Path("/tmp/ui-sample.zip")
    with zipfile.ZipFile(sample, "w") as z:
        z.writestr("app.py", (
            'import sqlite3\n'
            'def get_user(uid):\n'
            '    conn = sqlite3.connect("app.db")\n'
            '    cur = conn.cursor()\n'
            '    cur.execute("SELECT * FROM users WHERE id = \\"%s\\"" % uid)\n'
            '    return cur.fetchone()\n'
            'API_TOKEN = "hunter2-hardcoded-secret"\n'
        ))
        z.writestr("utils.py", (
            'import os\n'
            'def rm(p):\n'
            '    os.system("rm -rf " + p)\n'
            'PASSWORD = "admin12345"\n'
        ))

    with sync_playwright() as p:
        browser = p.chromium.launch()
        page = browser.new_page(viewport={"width": 1600, "height": 1000})
        page.set_default_timeout(20_000)

        # ---- 1. 登录 ----
        login(page, args.base, args.user, args.password)
        check("登录成功并跳转项目页", True)
        snap(1)

        # ---- 2. 新建项目（UI 上传）----
        # 登录后落总览（App P4）：总览的"新建项目"按钮跳 /projects?new=1（项目页暂不
        # 消费该参数自动开弹窗）——GUI 门禁需到项目页点 PageHeader 的按钮开 Modal
        page.goto(args.base + "/projects", wait_until="domcontentloaded")
        page.get_by_role("button", name="新建项目").click()
        dlg = page.locator(".ant-modal:visible, [role='dialog']").first
        dlg.wait_for(state="visible")
        name = f"ui-全流程-{time.strftime('%H%M%S')}"
        inputs = dlg.locator("input:visible")
        inputs.nth(0).fill(name)  # 名称(0)/仓库地址(1)/默认分支(2)——记忆实证顺序
        snap(2)
        with page.expect_file_chooser() as fc:
            dlg.locator("[class*='dropzone'], [class*='upload']").first.click()
        fc.value.set_files(str(sample))
        try:
            page.get_by_text("上传成功", exact=False).first.wait_for(timeout=20_000)
            check("压缩包上传成功（toast：上传成功——项目将以该压缩包为源码）", True)
        except Exception:
            check("压缩包上传成功（toast：上传成功——项目将以该压缩包为源码）", False)
        snap(3)
        page.locator("button").filter(has_text="确").last.click()

        # ---- 3. toast 三连 + 自动跳任务详情 ----
        try:
            page.wait_for_url("**/tasks/gw-**", timeout=20_000)
            check("创建后自动跳转任务详情页（/tasks/gw-*）", True)
        except Exception:
            check("创建后自动跳转任务详情页（/tasks/gw-*）", False, page.url)
        snap(4)
        tid = page.url.rstrip("/").split("/")[-1]

        # ---- 4. 运行期观察 + 流式判据 v3 内联（自建任务的 AI 阶段即观测窗）----
        page.wait_for_selector(f"{BOX}, .ant-tabs", timeout=20_000)
        body = page.locator("body").inner_text()
        check("任务详情呈现（含任务号 gw- 头）", "gw-" in body)

        token = api(args.base, "POST", "/v1/auth/login",
                    payload={"username": args.user, "password": args.password})["access_token"]
        exec_ok = ai_seen = False

        def on_beat(status, panel_bytes, beat):
            nonlocal exec_ok, ai_seen
            if not exec_ok:
                b = page.locator("body").inner_text()
                if "沙箱创建" in b or "状态流转" in b:
                    exec_ok = True
            if not ai_seen and panel_bytes > 0:
                ai_seen = True
                snap(5)

        # 模式C AI 阶段实测 6m48s~11min+（DSH 自纠轮数随 LLM 输出浮动），窗口须兜住慢轮
        verdict, produced = run_stream_verdict(
            page, args.base, tid, token, timeout_s=1200, shot_dir=None, on_beat=on_beat)
        check("运行期流式判据 v3（面板跟随后端实质产出直至终态）", verdict == "pass",
              f"verdict={verdict}, backend_cursor={produced}")
        check("执行日志卡渲染（沙箱创建/状态流转行可见）", exec_ok)
        check("AI 交互日志卡渲染（面板出现且有内容）", ai_seen)

        # ---- 5. 终态 ----
        status = snap_status(api(args.base, "GET", f"/v1/tasks/{tid}/snapshot", token))
        check("任务到达终态 COMPLETED（snapshot API 口径）", status == "TASK_STATUS_COMPLETED",
              f"status={status}")
        page.reload(wait_until="domcontentloaded")
        page.wait_for_timeout(3000)
        snap(6)
        body = page.locator("body").inner_text()
        check("终态徽标中文口径（已完成）", "已完成" in body)
        if page.query_selector(BOX):
            panel_text = page.eval_on_selector(BOX, "el => el.textContent") or ""
            final_bytes = len(panel_text.encode("utf-8"))
            # 终态健康（2026-09-17 钉）：面板"非空且收束"不等于 AI 链路活——DSH
            # 回合失败时面板被 API Error 文案填满同样"非空"（sim401 事故实录，
            # LESSONS #17）。降级三标记任一命中即红：API Error（DSH 回合反馈）、
            # 降级为内置规则引擎/RuleScan 兜底（engine WARN 行，渲染进执行/AI 日志卡）。
            degraded = ("API Error" in panel_text
                        or "降级为内置规则引擎" in body or "RuleScan 兜底" in body)
            check("AI 交互终态健康（无 API Error/RuleScan 降级文案）", not degraded,
                  f"{final_bytes}B" + ("——AI 链路断裂:网关 provider/inference route 未注册?" if degraded else ""))
            check("AI 交互日志终态非空且收束", final_bytes > 0 and "收束" in body,
                  f"{final_bytes}B")
        check("阶段时间线四阶段呈现", all(k in body for k in ("SAST 扫描", "AI 推理", "结果融合", "报告生成")))

        # ---- 6. 发现页签 + 融合视图 + 风险详情深检（终态后才渲染）----
        try:
            page.get_by_role("tab", name="发现", exact=False).first.click()
            page.wait_for_timeout(1500)
            body = page.locator("body").inner_text()
            check("发现页签渲染发现行（风险详情按钮）", body.count("风险详情") >= 1,
                  f"风险详情按钮×{body.count('风险详情')}")
            snap(7)
            # ---- 6a 增量双视图（ADR-225 F21）：来源筛选三态在位并可切换 ----
            # 本用例任务为全量扫描（无继承行）——切"继承"断言空态文案不崩即算过；
            # 继承行渲染面的行为锁在 vitest（FindingsPage 来源筛选）与 e2e 11。
            # SKIP_6A=1：二分排障开关（定位 6a 与深检失败的因果关系时禁用本块）。
            def _run_6a():
                try:
                    combos = page.locator(".ant-select")
                    combos.nth(2).click()  # 过滤栏下拉序：结论[0]/严重程度[1]/来源[2]
                    page.wait_for_timeout(600)
                    opts = page.locator(".ant-select-dropdown:visible .ant-select-item-option")
                    labels = [opts.nth(i).inner_text() for i in range(min(opts.count(), 5))]
                    check("来源筛选三态选项（全部/新发现/继承）",
                          all(k in labels for k in ("全部", "新发现", "继承")), ",".join(labels))
                    opts_filter = page.locator(
                        ".ant-select-dropdown:visible .ant-select-item-option", has_text="继承")
                    opts_filter.first.click()
                    page.wait_for_timeout(800)
                    body2 = page.locator("body").inner_text()
                    check("来源=继承 视图切换不崩（空态或继承行如实呈现）",
                          ("暂无发现" in body2) or ("继承" in body2))
                    combos.nth(2).click()
                    page.locator(".ant-select-dropdown:visible .ant-select-item-option",
                                 has_text="全部").first.click()
                    page.wait_for_timeout(600)
                except Exception as e:
                    check("来源筛选三态选项（全部/新发现/继承）", False, str(e)[:80])
            try:
                page.get_by_role("tab", name="融合视图", exact=False).first.click()
                page.wait_for_timeout(1200)
                check("融合视图页签可切换渲染", len(page.locator("body").inner_text()) > 500)
                snap(8)
                page.get_by_role("tab", name="发现", exact=False).first.click()
            except Exception as e:
                check("融合视图页签可切换渲染", False, str(e)[:80])
            try:
                page.get_by_role("button", name="风险详情").first.click()
                page.wait_for_timeout(1200)
                body = page.locator("body").inner_text()
                check("风险详情抽屉打开（源码/Sink/结论区）",
                      any(k in body for k in ("源码", "Sink", "结论", "CWE")))
                snap(9)
                page.keyboard.press("Escape")
            except Exception as e:
                check("风险详情抽屉打开", False, str(e)[:80])
            # ---- 6.5 风险详情深检：链路点选定位 + 污点链 + 裁决回写（R-30 后口径）----
            # 注意：步骤 6 已展开第 0 行（其按钮已变"收起"），此处 .first 命中的是
            # 下一行按钮→两行同时展开；所有断言必须作用域到"新展开行"（.last），
            # 全局 querySelector 会打到第 0 行的同名组件（2026-09-07 双展开实测踩坑）。
            try:
                # 发现行顺序随融合结果浮动（SAST 行无链路数据=诚实降级行）——有界游走
                # 展开至首个带 hops 的 AI 行再断言链路点选；全部行无 hops 才算失败
                # （2026-09-10 复跑实证：DOM 序把 opengrep 行排前致 hops=0 假红）。
                exp = None
                for row_try in range(5):
                    page.get_by_role("button", name="风险详情").first.click()
                    cand = page.locator("tr.ant-table-expanded-row:visible").last
                    cand.wait_for(state="visible", timeout=10_000)
                    page.wait_for_timeout(1200)  # source-file 拉取
                    if cand.locator("[data-testid^='chain-hop-']").count() >= 1:
                        exp = cand
                        exp_idx = page.locator("tr.ant-table-expanded-row:visible").count() - 1
                        break
                if exp is None:
                    if _task_degraded(page, args):
                        check("风险详情深检（链路点选/污点链/裁决回写）", True,
                              "INCONCLUSIVE 豁免：任务处于 AI 降级窗口（RuleScan 兜底无 hops，降级可感知已在任务层断言）")
                        raise _SkipDeepCheck()
                    raise RuntimeError("游走 5 行均无 chain-hop（全任务无 AI 链路行——检查融合产出）")
                body = exp.inner_text()
                m = re.search(r"已居中定位到第 (\d+) 行（(链路引用|漏洞位置)）", body)
                check("代码上下文：源码视图默认居中到漏洞位置", bool(m) and m.group(2) == "漏洞位置",
                      f"caption={m.groups() if m else None}")
                hops = exp.locator("[data-testid^='chain-hop-']")
                n_hops = hops.count()
                check("AI 结论链路渲染（R-30 修复后 reasoning 落库→hops 可点选）", n_hops >= 1,
                      f"hops={n_hops}")
                if n_hops >= 1:
                  try:
                    pinned = page.locator("tr.ant-table-expanded-row:visible").nth(exp_idx)
                    hop_line, hop_i = None, None
                    for i in range(n_hops):
                        t = hops.nth(i).inner_text()
                        hm = re.search(r":(\d+)", t)
                        if hm and (not m or int(hm.group(1)) != int(m.group(1))):
                            hop_line = int(hm.group(1)); hop_i = i
                            if "汇" in t:
                                break
                    if hop_i is not None:
                        hops.nth(hop_i).click()
                        # 切文件需重拉 source-file：等目标行渲染进本行视图再断言
                        try:
                            pinned.locator(f"[data-line='{hop_line}']").first.wait_for(timeout=10_000)
                        except Exception:
                            pass
                        body = pinned.inner_text()
                        m2 = re.search(r"已居中定位到第 (\d+) 行（链路引用）", body)
                        st = pinned.locator("[data-testid='source-viewer']").evaluate(
                            """(v, line) => { const r = v.querySelector(`[data-line='${line}']`);
                                if (!r) return null; const cs = getComputedStyle(r);
                                return {boxShadow: cs.boxShadow, top: r.offsetTop, scrollTop: v.scrollTop, vh: v.clientHeight}; }""",
                            hop_line)
                        centered = st and 0 <= st["top"] - st["scrollTop"] <= st["vh"]
                        check("点选链路 hop：切换为「链路引用」定位且行号一致",
                              bool(m2) and int(m2.group(1)) == hop_line, f"caption={m2.groups() if m2 else None}")
                        # 高亮色 = web src/theme/evidence.ts EVIDENCE.link（2026-09-13 P1
                        # 设计整改换装 #58A6FF=rgb(88,166,255)；旧值 64,169,255 已废）——
                        # 再换主题须同步此处字面量
                        check("链路行蓝色高亮且居中（自动滚动）",
                              bool(st) and "88, 166, 255" in st["boxShadow"] and centered)
                  except Exception as e:
                    check("点选链路 hop：切换为「链路引用」定位且行号一致", False,
                          f"hop 点选块异常（定位器失稳/源文件切换）：{str(e)[:90]}")
                taint = "污点传播链路" in body and "SOURCE" in body and "SINK" in body
                honest = "数据流链路仅由污点类规则扫描产出，该发现未携带链路数据" in body
                check("污点链路/链路缺席说明（taint 规则渲染 SOURCE→SINK；非 taint 显一行说明）", taint or honest,
                      f"taint={taint}, honest={honest}")
                snap("6a")
                # 人工裁决回写（R-30②：verdict 与 reasoning 必须都落库）
                card = exp.locator(".ant-card", has_text="人工裁决").last
                card.scroll_into_view_if_needed()
                card.locator(".ant-select").first.click()
                page.locator(".ant-select-dropdown:visible .ant-select-item-option", has_text="误报").first.click()
                card.locator("textarea").fill("自动化验收：人工裁决链路回归——结论与理由需一并落库")
                card.get_by_role("button", name="提交裁决").click()
                try:
                    page.locator(".ant-message", has_text="结论已回写").first.wait_for(timeout=10_000)
                    ok_toast = True
                except Exception:
                    ok_toast = False
                check("提交裁决：toast「结论已回写」", ok_toast)
                page.wait_for_timeout(2500)
                body = exp.inner_text()
                check("裁决回写生效且理由原文展示（R-30②）",
                      ("写入方：人工" in body) and ("人工裁决链路回归" in body))
                snap("6b")
            except _SkipDeepCheck:
                pass  # 已按降级豁免记账
            except Exception as e:
                if _task_degraded(page, args):
                    check("风险详情深检（链路点选/污点链/裁决回写）", True,
                          f"INCONCLUSIVE 豁免：AI 降级窗口 + 定位竞争（{str(e)[:60]}）")
                else:
                    check("风险详情深检（链路点选/污点链/裁决回写）", False, str(e)[:100])
            # 6a 在深检之后执行（实证定位：先跑 6a 的下拉/过滤交互残留会令深检的
            # 行游走点击超时——禁用 6a 复跑即绿，二分归因 2026-09-10）
            if not os.environ.get("SKIP_6A"):
                _run_6a()
        except Exception as e:
            check("发现页签渲染发现行", False, str(e)[:80])

        # ---- 7. 报告深链 + 在线查看（新标签页渲染原始 JSON）----
        try:
            page.goto(f"{args.base}/reports?task={tid}", wait_until="domcontentloaded")
            page.wait_for_timeout(1500)
            rbody = page.locator("body").inner_text()
            check("报告深链定位本任务报告（?task= 过滤，绕开首页分页）",
                  tid in rbody and ("在线查看" in rbody or "下 载" in rbody))
            snap(10)
            with page.context.expect_page() as pop:
                page.get_by_role("button", name="在线查看", exact=False).first.click()
            view = pop.value
            view.wait_for_load_state("domcontentloaded")
            # 2026-09-12 报告窗口改 sandboxed iframe+blob 通道（B4 待办收尾）——正文不
            # 再直写在宿主 body；断言升级：iframe 在位 + blob 正文真实含报告内容
            # （读 iframe.contentWindow 不受 sandbox 阻——同源 about:blank 宿主注入）。
            try:
                vbody = view.locator("body").inner_text()
                direct = tid in vbody or "findings" in vbody
            except Exception:
                direct = False
            blob_text = ""
            try:
                blob_text = view.evaluate(
                    """() => { const f = document.querySelector('iframe[sandbox]'); if (!f) return '';
                        return f.src.startsWith('blob:') ? 'BLOB_IFRAME' : ''; }""")
            except Exception:
                pass
            iframe_ok = False
            if blob_text == "BLOB_IFRAME":
                iframe_ok = view.evaluate(
                    """() => fetch(document.querySelector('iframe[sandbox]').src)
                        .then(r => r.text()).then(t => t.includes('findings') || t.includes('task_id') ? 'OK' : t.slice(0,80))""") == "OK"
            check("在线查看完整报告（新标签页原始 JSON）", direct or iframe_ok,
                  "direct" if direct else ("iframe+blob 正文含报告内容" if iframe_ok else "窗口正文不含报告内容"))
            view.screenshot(path=str(shots / "11-report-view.png"))
            view.close()
        except Exception as e:
            check("在线查看完整报告", False, str(e)[:80])

        # ---- 8. 通知中心（侧边菜单项导航，非铃铛图标）----
        try:
            page.get_by_role("menuitem", name="通知", exact=False).first.click()
            page.wait_for_timeout(1500)
            body = page.locator("body").inner_text()
            check("通知中心渲染且有条目", "任务" in body and ("未读" in body or "已完成" in body))
            snap(12)
        except Exception as e:
            check("通知中心渲染", False, str(e)[:80])

        # ---- 9. 仓库型(git)项目 UI 呈现（首页 10 条分页——测试项目累积后目标
        #      可能掉到第 2 页，有界翻页游走而非只搜首页）----
        try:
            page.goto(f"{args.base}/projects", wait_until="domcontentloaded")
            page.wait_for_timeout(1500)
            found = False
            for _ in range(6):
                if page.get_by_text(args.repo_project, exact=False).count() > 0:
                    page.get_by_text(args.repo_project, exact=False).first.click()
                    found = True
                    break
                nxt = page.locator(".ant-pagination-next:not(.ant-pagination-disabled)")
                if nxt.count() == 0:
                    break
                nxt.first.click()
                page.wait_for_timeout(1200)
            if not found:
                raise RuntimeError("翻完 6 页未见目标项目（分页游走上限）")
            page.wait_for_timeout(1500)
            body = page.locator("body").inner_text()
            ok = ("已完成" in body) or ("gw-" in body)
            check(f"仓库型项目「{args.repo_project}」UI 可见且任务可达", ok)
            snap(13)
        except Exception as e:
            check("仓库型项目 UI 呈现", False, str(e)[:80])

        browser.close()

    fails = [n for n, ok in RESULTS if not ok]
    print(f"\n==== UI 全流程结果: {len(RESULTS) - len(fails)}/{len(RESULTS)} 通过 ====")
    if fails:
        print("失败项:")
        for n in fails:
            print("  -", n)
        return 1
    return 0


def main():
    ap = argparse.ArgumentParser(description="GUI 黑盒门禁单入口（默认全流程；--task 挂载流式）")
    ap.add_argument("--base", default="http://192.0.2.122:18088")
    ap.add_argument("--user", default="admin")
    ap.add_argument("--password", default="admin")
    ap.add_argument("--task", default="", help="挂载模式：对一个 RUNNING 任务只跑流式判据")
    ap.add_argument("--timeout", type=int, default=300, help="挂载模式总采样窗上限秒")
    # 缺损批 A1（2026-10-02）：切换段与流式判据窗解耦——原共用 --timeout 使 SW6 的
    # 等终态与 v3 判据窗互相挤压（字面 PASS 只在竞态落对时出现，两次 INCONCLUSIVE
    # 实录）；切换段独立上限（默认 300s：SW1-SW5 需 120-180s+余量），到点未终态即
    # 转判据（判据窗仍用 --timeout 全额，从切换完成时起算）。
    ap.add_argument("--switch-timeout", type=int, default=300, help="切换段（SW0-SW6）时长上限秒；到点未终态转流式判据")
    ap.add_argument("--repo-project", default="repo-git-全链", help="仓库型项目名（UI 呈现核验）")
    ap.add_argument("--shots", default="", help="截图目录")
    args = ap.parse_args()

    if args.task:
        return mode_attach(args)
    args.shots = args.shots or ".agent/evidence/ui-walkthrough"
    return mode_walkthrough(args)


if __name__ == "__main__":
    sys.exit(main())
