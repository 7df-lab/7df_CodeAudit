# Post-mortems

English | [中文](README.zh.md)

Incident write-ups: a bug reached a place it shouldn't have (a real user, a merged PR, a release), and the interesting part is *why our process let it through*, not just the one-line fix.

A post-mortem is NOT an [Agent Note](../../.agents/notes/README.md) (which records a deliberate design decision and its rejected alternatives, or proposes future work). It is a backward-looking record of a failure: what broke, the mechanism, why every safety net missed it, and the concrete guardrails added so the same class of bug fails loudly next time.

Write one when a bug is **subtle** (the mechanism is non-obvious and a careful engineer would re-derive it the hard way), **systemic** (the reason it escaped is a gap in tests/tooling/conventions, not a one-off typo), and **costly to rediscover** (it cost real debugging time, and would cost it again). Link the guardrails (tests, AGENTS.md rules, ADRs) the post-mortem motivated.

Closing an incident also pins it: every post-mortem gets an entry in the [regression corpus](../../scripts/regression-corpus.manifest.json) naming its bug class and the keyless, permanently running test (or gate script) that fails when the class reintroduces — `verify-regression-corpus` rejects a post-mortem without an entry, a pin whose test was renamed or removed, and a pin whose file no longer lives in the lane it claims. `tsx scripts/verify-regression-corpus.ts --run` executes the runnable pins locally.

Every post-mortem opens with an **Executive summary**: one short paragraph a busy reader can absorb in thirty seconds — what broke, the root cause in plain terms, why it escaped, and the durable lesson — before the detailed Summary / Timeline / Root cause / Guardrails sections that follow.

| # | Title |
|---|---|
| [0001](0001-acp-default-export-drops-inject.md) | ACP server crashed on connect: `export default` dropped the plugin's `inject` |
| [0002](0002-js-expression-disabled-filesystem-tools.md) | Filesystem snapshot tools were permanently disabled by a literal `!!js` object |
| [0003](0003-web-agent-gui-feedback-loop.md) | Web agent validated a replacement server instead of the GUI hosting its session |
| [0004](0004-landlock-partial-notice-misclassified-child-failures.md) | Landlock partial-enforcement notice misclassified child failures |
