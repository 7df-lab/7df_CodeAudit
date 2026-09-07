# 事故复盘（postmortem）

[English](README.md) | 中文

事故复盘记录的是：一个 bug 出现在了不该出现的地方（真实用户、已合并的 PR（Pull Request）、已发布的版本），值得关注的是*为什么我们的流程放过了它*，而不仅仅是那一行修复。

事故复盘不是 [Agent Note](../../.agents/notes/README.zh.md)（Agent Note 记录一个经过深思熟虑的设计决策及其被否决的替代方案，或提出未来工作）。它是一份回顾性的失败记录：什么坏了、机制是什么、为什么每道安全网都没拦住、以及为此新增了哪些具体防护措施，以确保同类 bug 下次出现时会明确报错。

当一个 bug 满足以下条件时，请撰写事故复盘：**隐蔽**（机制不显而易见，即使是细心的工程师也得费力重新推导）、**系统性**（逃逸的原因是测试、工具、约定的缺口，而非一次性的笔误）、**重新发现的代价高**（它消耗了真实的调试时间，且下次还会如此）。请链接该事故复盘所推动建立的防护措施（测试、AGENTS.md 规则、ADR）。

事故收尾时还要把它钉住：每篇事故复盘都要在[回归账本](../../scripts/regression-corpus.manifest.json)中登记条目，写明其 bug 类别，以及当该类别复发时会失败的无密钥、永久运行的测试（或门禁脚本）——`verify-regression-corpus` 会拒绝没有条目的复盘、测试已被改名或删除的钉住项，以及所属测试文件已不在其声称的测试层中的钉住项。`tsx scripts/verify-regression-corpus.ts --run` 可在本地执行可运行的钉住项。

每篇事故复盘以一段**执行摘要**开头：一个简短段落，让忙碌的读者在三十秒内吸收要点——什么坏了、用直白的话说根因是什么、为什么逃逸了、可长期沿用的教训是什么——然后才是后续的详细「概述、时间线、根因、防护措施」各节。

| # | 标题 |
|---|---|
| [0001](0001-acp-default-export-drops-inject.zh.md) | ACP（Agent Client Protocol）服务器在连接时崩溃：`export default` 丢失了插件的 `inject` |
| [0002](0002-js-expression-disabled-filesystem-tools.zh.md) | 文件系统快照工具被一个字面量 `!!js` 对象永久禁用 |
| [0003](0003-web-agent-gui-feedback-loop.zh.md) | Web agent（智能体）验证了替代服务器，而非承载其会话的 GUI |
| [0004](0004-landlock-partial-notice-misclassified-child-failures.zh.md) | Landlock 部分强制执行通知导致子进程失败被误归类 |
