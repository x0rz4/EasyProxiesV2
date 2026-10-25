# 子系统：数据面（选路 / 实例 / 自愈）

改动 `internal/outbound/pool`、`internal/group`、`internal/boxmgr`、`internal/monitor`、`internal/probe`、`internal/builder` 之前先读这份。行为契约的 MUST 见 [proxy-pool spec](../../plane/specs/proxy-pool/spec.md)。

## 包与文件

| 包 | 文件 | 职责 |
| --- | --- | --- |
| `outbound/pool` | `pool.go`、`shared_state.go` | 不可变快照上的选路、黑名单、失败记账、两阶段提交、共享状态 |
| `group` | `runtime.go`、`healer.go`、`healer_coordinator.go` | 成员状态机（ALIVE/SUSPECT/EVICTED）、滑动失败窗、踢出/恢复、自愈规划与容量选择 |
| `boxmgr` | `manager.go`、`healer.go`、`landing_location.go`、`probe_projection.go`、`reload_rebase.go` | sing-box 实例生命周期与"代"、三条重载路径、自愈执行端 |
| `monitor` | `node_registry.go`、`probe_scheduler.go`、`health_event_hub.go`、`health_applier.go`、`speedtest.go`、`tag_providers.go` | 运行态注册表、探测调度、健康事件游标投递、健康投影 |
| `probe` | `applier.go` | 测量事件唯一应用链（收据/幂等/身份门禁/健康日志行） |
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

不响应取消的探测调用在实际返回前始终保留占位与 detached 计数，不按时间遗忘；同一个运行时调用不能重入。调度器另有 512 个真实未退出调用的总上限，覆盖运行时代际更替；触顶显式拒绝新调用。卡死的 Go 调用无法被强杀，须由底层连接退出或替换运行时恢复。

`monitor.beginHealthResult` → `probe.Applier`（收据/幂等/身份/**日志行**） → `ProjectHealthOutcome` → `health_applier` → 运行态 entry → `HealthResultEvent`（**只作唤醒**） → 消费者按游标读 `health_events`。诊断 origin（`nodedetect`、`unlock`）**绝不发布健康事件**；`SnapshotOnly` 是可合并的刷新标志，只要求重读快照，MUST NOT 推进恢复/失败窗口（spec HEALTH-001/002/003、DIAGNOSTIC-001）。

当前投递形态（2.2.6 起）：被接受的健康测量在**测量事务内**追加一条 `health_events` 行（`seq` 单调），订阅从日志按游标有序消费。pool 是持久消费者（key `pool:<groupID>`/`pool:base`），重启后从游标续读，重复投递按 ProbeID 幂等（`groupMemberRuntime.appliedProbes`，环深 64 = 一个投递批次）。日志容量 8192，满时**拒绝新测量并报错**（显式背压）。

清理边界有两处容易搞反，改之前先读 spec HEALTH-005：

- 只有**当前注册**的持久消费者约束清理。分组被删除/禁用/实例停止后留下的游标行不参与计算——否则它会把最慢位置永久钉住，日志填满后所有分组的测量都被拒绝。
- 已消费的行按 5 分钟窗口释放（无注册消费者时 30 分钟）。稳态大小 = `速率 × 窗口`；沿用 30 分钟时长，约 137 个节点按 60s 探测就会在消费者完全跟上的情况下触顶。

游标持久化失败会**停止**该消费者继续读取（不是只记一次失败），因为去重环只覆盖一批，带着未落盘进度前进会在重启后二次推进恢复计数。积压/最老年龄/游标见 `GET /api/operations/health-events`。关闭只取消等待，未投递的行保留给下次启动；已开始的回调可在 close 返回后完成。无 store 的内存模式仍走有界直投（1024），是兼容/测试路径，不是生产契约。契约与边界见 spec HEALTH-002..005 与 [健康事件日志设计](../../plane/changes/health-event-log/design.md)。

游标与它造成的状态**同事务**提交：消费者回调返回 `[]store.GroupNodeState`，hub 用 `AdvanceHealthCursorWithState` 一次写入游标 + `group_node_states`。分成两次写，崩溃就可能在游标领先的状态下丢掉一条踢出记录，重启后坏节点以健康身份复活。回调要带上**跨分组传播**的行（踢出/恢复是节点级事实），所以 `SnapshotStates` 扫所有分组。指向已删除分组/节点的行直接丢弃并继续推进游标——那种行无处可去，重试会永久卡住消费者。

`group_node_states` 还有第二个写者：`internal/app` 的异步合并持久化器（当前节点与非健康转换）。两者都写"转换发生时的快照"，所以每行带单调 `revision`，写入只在 `revision = 0` 或不小于已存值时生效，否则晚到的旧快照会把刚解除的踢出写回去。`revision` 随 store→config→runtime 往返，启动时用它抬升全局时钟。
