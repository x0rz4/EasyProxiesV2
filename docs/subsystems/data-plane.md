# 子系统：数据面（选路 / 实例 / 自愈）

改动 `internal/outbound/pool`、`internal/group`、`internal/boxmgr`、`internal/monitor`、`internal/probe`、`internal/builder` 之前先读这份。行为契约的 MUST 见 [proxy-pool spec](../../plane/specs/proxy-pool/spec.md)。

## 包与文件

| 包 | 文件 | 职责 |
| --- | --- | --- |
| `outbound/pool` | `pool.go`、`shared_state.go` | 不可变快照上的选路、黑名单、失败记账、两阶段提交、共享状态 |
| `group` | `runtime.go`、`healer.go`、`healer_coordinator.go` | 成员状态机（ALIVE/SUSPECT/EVICTED）、滑动失败窗、踢出/恢复、自愈规划与容量选择 |
| `boxmgr` | `manager.go`、`healer.go`、`landing_location.go`、`probe_projection.go`、`reload_rebase.go` | sing-box 实例生命周期与"代"、三条重载路径、自愈执行端 |
| `monitor` | `node_registry.go`、`probe_scheduler.go`、`health_event_hub.go`、`health_applier.go`、`speedtest.go`、`tag_providers.go` | 运行态注册表、探测调度、健康事件 hub、健康投影 |
| `probe` | `applier.go` | 测量事件唯一应用链（收据/幂等/身份门禁） |
| `builder` | `builder.go` | config+store → sing-box `option.Options` |

## 关键不变量

1. **快照只读、选路无锁**：数据面读路径只做一次 `atomic.Pointer.Load()`；写侧走 `PrepareUpdate` → `Commit`，`PrepareUpdate` 失败必须 `Rollback` 并清理已创建成员。
2. **代际不可覆盖**：出站 tag 命名唯一在 `runtimetag`（`<16hex>@vN`）。同 tag 重复 `CreateMember` 会关掉旧 outbound、无法排空，所以变更节点必须换新 `@vN`。
3. **成员判定只有一份**：`groupmember.NewFilter().Allow()`（内部即 `nodeselect.Filter`）。builder 与 boxmgr 都走它，顺序契约固定：组未启用 → `excluded` 无条件排除 → `explicit` 无条件加入（绕过黑白名单）→ region 命中 + tag 黑/白名单（`any`/`all`）。未解析 tag ID 变成永不匹配哨兵（宁可缩小，不许放宽）。
4. **失败记账归属**：普通模式 → `sharedMemberState`（阈值→临时黑名单）；分组模式 → `group.RecordFailure`（滑动窗口→SUSPECT/EVICTED，按 NodeID **全局传播到所有分组**）。恢复同样全局；自动恢复参数是每分组一项配置。**调用方取消/超时不计入成员失败**（`dialFailureAttributable`）。
5. **锁边界是硬约束**：`boxmgr.mu` 只保护管理字段与 box 生命周期，`reloadMu` 串行化生命周期操作；**禁止持锁做网络拨号、解析订阅或等待 drain**。
6. **事件与快照的顺序**：`ReconcileSilent` 先迁移状态，**等匹配的 pool 快照可见后再 `NotifyEvents`**（护栏 6）。
7. **成员 outbound 不能声明静态依赖**：`CreateMember` 后 `Dependencies()` 非空会立即失败并回滚（否则 sing-box `dependByTag` 无法随快照更新）。

## 三条重载路径（boxmgr）

节点集合的唯一来源是 `runtimenodes.Load`（enabled 非订阅 + enabled 订阅可达，按 identity 去重）。冷启动 `Start` / 增量 `reloadNodesIncrementally` / 全量蓝绿 `reload` 三条路径都必须走它，不得各自过滤 `enabled`。

## 自愈链（HEAL）

`group.PlanHealing`/`CapacitySelection` → `boxmgr.ApplyHealingSelection`（重 Plan → 增量 PrepareUpdate/Commit → NotifyEvents）。promote → probe → 再 promote → refresh；每阶段重读政策与状态；**planned 审计失败必须阻断动作**。预算/冷却见 spec Requirement HEAL-002。运维手册见 [group-healing](../../plane/group-healing.md)（不重新实现 Healer）。

## 探测链（L1）

`monitor.beginHealthResult` → `probe.Applier`（收据/幂等/身份） → `ProjectHealthOutcome` → `health_applier` → 运行态 entry → `HealthResultEvent`。诊断 origin（`nodedetect`、`unlock`）**绝不发布健康事件**；`SnapshotOnly` 事件只要求重读快照，MUST NOT 推进恢复/失败窗口（spec HEALTH-001 / DIAGNOSTIC-001）。
