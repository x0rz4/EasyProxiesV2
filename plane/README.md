# 架构、规格与实施计划

本目录服务于 EasyProxiesV2 的单机大型代理池建设：汇聚多来源节点，以统一身份、健康事实、成员规则和有界自愈提供稳定出口。产品层次为 L0 节点事实、L1 健康平面、L2 分组策略、L3 数据面、L4 运营控制面、L5 平台化。

> **与 `docs/` 的分工**：仓库根部的 [docs/architecture.md](../docs/architecture.md) 写"当前代码真实形态"，[docs/code-standards.md](../docs/code-standards.md) 是硬规则唯一体，[LESSONS.md](../LESSONS.md) 是复盘错题本。本目录（`plane/`）承载**运行时契约（specs/）、变更提案（changes/）与专题/历史记录**。同一约束只在一处定义：本目录引用 Requirement ID，不复述规则；历史文档的旧限制不覆盖当前规范。路由入口见 [AGENTS.md](../AGENTS.md)。

## 阅读入口

| 文档 | 用途与状态 |
| --- | --- |
| [前端合理性、安全与性能审计](changes/frontend-audit/plan.md) | 依赖、会话/缓存、按需加载、流式资源、表单与移动/键盘验证；实施中 |
| [池性能审计后续评估与修复](changes/pool-memory-throughput/followup.md) | 原五项后续建议、测量入口、内存/吞吐优化与逐项验收进度 |
| [池快照内存与选路吞吐审计](changes/pool-memory-throughput/plan.md) | 架构评估、连接引用收敛、固定模式线性回退及基准验收 |
| [解锁页在线可用范围](changes/unlock-healthy-scope/plan.md) | 列表、统计与检测目标统一限制在线可用，保留其它页面原范围 |
| [安装器中文与更新](changes/installer-chinese-update/plan.md) | 默认 /opt 子目录、中文帮助、显式更新及服务/容器状态保留 |
| [解锁页布局与计数修复](changes/unlock-page-cleanup/plan.md) | 整页滚动、移除重复统计、运行态节点计数及分页详情一致性 |
| [SS obfs 兼容修复](changes/ss-obfs-compatibility/plan.md) | Clash 混淆插件运行时适配、身份兼容与重载回归 |
| [Linux 服务安装优化](changes/linux-service-installation/plan.md) | systemd/OpenRC 注册、自启、离线管理及验收记录 |
| [控制面、刷新与退出链修复计划](changes/control-plane-scaling/plan.md) | 2026-10-26 后续七项修复、独立提交与验证记录 |
| [资源占用与稳定性修复计划](changes/resource-hardening/plan.md) | 2026-10-26 七项修复、独立提交顺序及验证记录 |
| [当前契约与实现基线](specs/proxy-pool/spec.md) | 以代码定位描述现有能力，列出 MUST 约束及其验证场景；不是规模验收报告 |
| [基础设施提案](changes/scale-proxy-pool/proposal.md) | 定位、范围、里程碑、规模假设和非目标 |
| [分层设计](changes/scale-proxy-pool/design.md) | 依赖边界、事件契约、现状差异与取舍 |
| [实施任务](changes/scale-proxy-pool/tasks.md) | 阶段 0–5、依赖、责任领域、验收产物 |
| [验证与规模门槛](changes/scale-proxy-pool/validation.md) | 回归矩阵、压测口径、指标和证据模板 |
| [写路径审计清单](write-path-audit.md) | 允许的写入者、旁路检索、待确认风险、评审检查 |
| [2026-10-23 稳定性审计](stability-audit-2026-10-23.md) | 订阅与重载局部修复、健康事件溢出复现、验证结果及独立改造建议 |
| [健康事件日志提案](changes/health-event-log/proposal.md) | 2.2.6 落地审计的独立改造建议：持久有序事件日志 + 游标投递 + 显式背压（已实现，契约见 design） |
| [容量自愈运营手册](group-healing.md) | 从 dry-run 到执行、缺口排查、关闭和回滚 |

## 文档层次与变更工作流

参考 [OpenSpec](https://github.com/Fission-AI/OpenSpec) 的规格驱动思路，在现有 `plane/` 内组织，不要求安装 CLI，也不声称本目录通过了 OpenSpec CLI 校验。

```text
plane/
  specs/proxy-pool/spec.md             当前契约与代码证据
  changes/scale-proxy-pool/
    proposal.md                       为什么做、范围和非目标
    design.md                         如何分层及设计取舍
    tasks.md                          待办、依赖和里程碑
    validation.md                     验收方法及尚缺的证据
  changes/health-event-log/
    proposal.md                       健康事件无损投递：为什么、范围、回滚
    design.md                         HEALTH-002..007 契约、日志/游标/背压设计
    tasks.md                          进度与遗留
  write-path-audit.md                  维护中的审计清单
  group-healing.md                     运维手册
  *.md                                专题设计或历史记录
```

1. 新变更先写 proposal，再在 design 中区分现有行为和拟新增行为。新增/修改/删除的规格使用 `ADDED` / `MODIFIED` / `REMOVED` 标记，并引用稳定的 Requirement ID。
2. 用 `Requirement` 和 `Scenario` 描述行为，场景使用 GIVEN / WHEN / THEN；MUST 表示实现和评审必须满足，SHOULD 表示可解释的推荐项。
3. tasks 中只有实际完成且带证据的任务才勾选；“已有某函数/测试”不等于本阶段已验收。
4. 实现与验证结束后，把通过的变更合入 `specs/` 的当前契约，再记录归档状态；未落地的未来设计留在 changes 中。
5. 同一约束只在 specs 中定义；设计、任务和手册引用 ID，不各自发明一份语义。历史文档的旧限制不能覆盖当前用户指令。

当前变更状态：**In progress，已开始 P1 首批健康入口硬化，M1–M4 尚未完成验收**。首批变更与验证记录见 [P1 健康边界](changes/scale-proxy-pool/p1-health-boundaries.md)；规模假设仍需实测冻结。

2.2.6 的健康事件日志变更（[proposal](changes/health-event-log/proposal.md) / [design](changes/health-event-log/design.md) / [tasks](changes/health-event-log/tasks.md)）：**已实现并合入 specs HEALTH-002..005**，遗留项（崩溃窗口内一批事件可能重复应用）记在 tasks.md。

## 专题文档索引

| 领域 | 文档 | 阅读提示 |
| --- | --- | --- |
| L0 事实与规则 | [节点视图重构计划](nodeview-refactor-plan.md)、[节点标签](node-tagging-system.md)、[分组规则页](group-screening-rules-page.md) | 前者保留实现历史，当前契约以 specs 与代码为准 |
| L1 探测 | [健康探测运行时](health-probe-runtime-architecture.md) | Scheduler / Registry / EventHub 现状与新规模化边界 |
| L2 容量 | [分组容量自愈](group-healing.md) | 已实现主路径及操作手册，不重新实现 Healer |
| L3 生命周期 | [增量热重载](incremental-hot-reload.md)、[多端口与混合模式](Multi-port-Hybrid.md) | 关注 PrepareUpdate/Commit、代际和连接排空 |
| L4 控制面 | [monitor 领域分层提案](monitor-server-domain-refactor.md) | **PR1（§11）、PR2（§13）、Phase 1（§14）、Phase 2 的 Auth/Session（§15）与 Settings（§16）均已完成**；其余领域 Service 与 HTTP 生命周期迁到 `app.Run` 仍是提案 |
| 性能专题 | [并发与内存](architecture-concurrency-memory-optimization.md)、[依赖与性能](dependency-performance-optimization.md) | 作为方案背景，实施前重新核对代码 |
| 历史与证据 | [节点视图验收记录](nodeview-validation.md)、[早期计划](panle1.md) | 记录当时环境和结果，不代表当前 CI 或规模认证 |

文档变更评审清单见 [写路径审计清单](write-path-audit.md#评审清单)。
