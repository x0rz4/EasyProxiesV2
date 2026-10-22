# CLAUDE.md

本仓库的工程约定由 **两份文件** 承载：一张路由表 + 一本错题本。不要在本文件里写规则。

- **[AGENTS.md](AGENTS.md)** —— 路由表。不写具体规则，只告诉你"干什么活之前先翻哪份文档"。所有"去哪看"的问题以它为准。
- **[LESSONS.md](LESSONS.md)** —— 错题本。agent 自己每晚复盘 commit 后写的日志：修完又返工的、压根不该合的、验证做得太糙的。每条带规则、原因、commit 证据；同一条教训在 3 个不同日子出现就转正进 [docs/code-standards.md](docs/code-standards.md)，成为逐条对照的硬规则。本仓库的代码由 agent 编写、评审、合并、发布，所以这本日志是写给下一个接手的 agent 自己的。

开工顺序：先按 AGENTS.md 路由表定位文档 → 写/审代码时逐条对照 docs/code-standards.md 与其 L 节审查清单 → 收工前扫一遍 LESSONS.md。
