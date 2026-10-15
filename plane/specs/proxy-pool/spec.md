# 代理池当前契约与实现基线

基线：提交 `71d8745`（安装与发布功能版本 `2.1.0`）。本次为静态代码核对；已有测试仅表示存在证据入口，不表示已重新执行或通过基础设施规模验收。阶段性增强见 [变更提案](../../changes/scale-proxy-pool/proposal.md)。

增量：`2.1.1` 的首批健康入口硬化与实际测试证据见 [P1 健康边界](../../changes/scale-proxy-pool/p1-health-boundaries.md)，不等同于 M1 或规模认证。

## 能力与事实源

| 层 | 权威能力及代码入口 | 当前边界 |
| --- | --- | --- |
| L0 节点事实 | `store`、`nodecodec`、`nodeloc`、`noderegion`、`nodetag`；`groupmember` / `nodeselect` / `nodeview` 统一规则与查询 | 地区证据与身份修订已落库；候选资格与健康/实际入池分开 |
| L1 健康平面 | `probe.Applier`；`monitor` 的 Scheduler、Registry、健康投影及 EventHub | 有持久化收据和投影重试；仍有内存兼容入口需做调用审计 |
| L2 分组策略 | `group.Runtime`、`PlanHealing`、`CapacitySelection`、`Healer` | 主路径已有；规模预算目前为代码常量，非完整配置产品 |
| L3 数据面 | `builder`、`boxmgr`、`outbound/pool` | 不可变快照及 PrepareUpdate/Commit；数据面失败是合法输入 |
| L4 控制面 | `monitor` API、`GroupPoolsPanel`、`GroupHealLog`、`store` 审计 | 已有策略校验和自愈日志；缺口聚合视图及策略变更留痕待补 |
| L5 平台化 | `app` 单进程装配、SQLite、安装与发布工作流 | 单机部署；不提供多实例一致性协议 |

## Requirement IDENTITY-001：结果必须绑定身份与运行时代际

持久化测量 MUST 在联网前记录身份 token/revision、ProbeID 和 StartedAt；结果应用 MUST 拒绝旧身份、重复或乱序结果。NodeID、身份 revision 与 runtime tag generation 不是同一概念，不能互相替代。显示名不作为身份。

### Scenario: 身份 A → B → A 期间旧结果返回

- GIVEN 同一 NodeID 的身份已发生两次替换，最初 A 的探测仍在途。
- WHEN 最初 A 的结果被提交。
- THEN 旧 revision MUST 被拒绝，不能改变新身份的健康、地区或恢复计数。

代码：`internal/probe/applier.go`、`internal/store/sqlite_location.go`、`internal/monitor/node_registry.go`、`internal/monitor/health_applier.go`。已有测试：`TestReceiptRejectsReplayOldAndReplacedIdentity`、`TestHealthResultCannotCrossIdentityReplacement`。

## Requirement HEALTH-001：持久化健康测量只有一条应用链

生产装配 MUST 使用 `HealthProbe + Health → Applier → receipt → ProjectHealthOutcome → runtime entry → HealthResultEvent`；事务失败不得发布测量成功。运行时投影重试 MUST 幂等，历史收据不能在进程重启后重复推进恢复。无 Store 的内存模式是兼容/测试路径，不是生产双写许可。

`HealthResultEvent` 当前携带 Tag、NodeID、Success、Latency、Error、CheckedAt 与 SnapshotOnly；SnapshotOnly 只要求重新读取当前快照，MUST NOT 作为一次新的健康成功/失败推进恢复或故障窗口。

持久化运行态无探测目标时不得用兼容方法制造成功收据，仍按 optimistic/strict 决定待测节点临时选路。单节点探测的调用方取消不得形成健康事实；探测自身预算超时不得接受 callback 的迟到成功。

### Scenario: 收据提交成功但投影失败

- GIVEN 测量与收据已在同一事务提交。
- WHEN 投影失败并在随后重试。
- THEN 挂起收据保留；重复投影不重复计数；确认只能确认相应收据。

代码：`internal/probe/applier.go`、`internal/monitor/health_applier.go`、`internal/boxmgr/probe_projection.go`。已有测试：`TestProjectionRetryAndConditionalAcknowledgment`、`TestPersistentHealthProjectionIsIdempotent`。

## Requirement DIAGNOSTIC-001：诊断不产生路由健康成功

测速、延迟诊断、质量和解锁结果 MUST 受 origin/kind 和单 payload 校验约束；可以写自身诊断事实、触发标签或成员规则更新，MUST NOT 伪造健康成功、清除 EVICTED 或推进恢复计数。真实数据面失败和显式人工恢复是不同的受控输入，不归入诊断。

### Scenario: 已剔除节点诊断成功

- GIVEN 节点处于 EVICTED，恢复成功次数尚未达到阈值。
- WHEN 手动测速、解锁或质量检测成功。
- THEN 诊断可展示成功，节点仍保持原剔除状态与恢复计数；只有合法健康复检能推进恢复。

代码：`internal/probe/applier.go`、`internal/monitor/probe_applier.go`、`internal/monitor/nodecheck.go`。已有测试：`TestManualResultCannotPublishHealth`、`TestNodeCheckLatencyDoesNotMutateHealth`；完整剔除×各诊断类型矩阵待补。

## Requirement MEMBERSHIP-001：候选资格先于运行态容量选择

候选集 MUST 通过共用成员规则产生：excluded 优先、explicit 按既定规则、再地区/标签/筛选语义；容量选择和运行证据 MUST NOT 扩大候选集。地区只接受出口证据，失败不能把已知地区无条件清空。

### Scenario: 健康备用节点不符合规则

- GIVEN 节点健康但已被显式排除或不匹配当前规则。
- WHEN 自愈发现容量缺口。
- THEN 该节点不得被提升；预览、运行态和下一次规划采用一致候选语义。

代码：`internal/groupmember`、`internal/nodeselect`、`internal/nodeview`、`internal/boxmgr/healer.go`、`internal/nodeloc`。

## Requirement CAPACITY-001：容量开关和健康定义保持兼容

`target_size=0` MUST 关闭容量管理。`target_size>0 && healing_enabled=false` MUST 仅规划与审计，当前实现不将流量限制到活动槽位。只有显式启用执行才切换容量选择。策略 MUST 满足 `0 ≤ min_healthy ≤ target_size ≤ 100000`，refill 无重复且顺序固定；可省略某动作。

健康计数 MUST 同时满足 ALIVE、Available、初检完成、非 Provisional、无黑名单，以及 `0 ≤ now - HealthCheckedAt ≤ max(60s, 2 × group interval)`。`Healthy` 是观测的活动健康数，`Projected` 是规划后假设选中的健康数，不能用后者宣称动作已成功。

### Scenario: 新组配置容量但未启用执行

- GIVEN 组填写目标容量，执行开关保持关闭。
- WHEN Healer 运行。
- THEN 只允许 dry_run 审计，不额外复检、不刷新订阅、不修改活动成员；正常周期探测仍工作。

代码：`internal/group/healer.go`、`internal/boxmgr/healer.go`。已有测试：`TestPlanHealingPromotesFreshStandbyWithoutMutatingEvidence`、`TestHealerDryRunDoesNotConsumeBudgetsOrPerformActions`。

## Requirement HEAL-001：自愈先审计、重读并二次校验

动作顺序 MUST 保持 promote → probe → 再次 promote → refresh；异步请求不得假设成功。每阶段重读政策与状态，执行端重新校验授权、资格和计划。planned 审计失败 MUST 阻止动作；动作完成后的审计失败不能被描述为动作已回滚。当前审计有去重，不是每次尝试唯一的完整追踪系统。

### Scenario: 规划后关闭执行或成员规则变化

- GIVEN 某动作已规划但尚未进入执行端。
- WHEN 当前政策关闭执行或候选集合改变。
- THEN 执行端 MUST 拒绝过期动作。已开始的网络请求不保证瞬间取消，后续结果仍经过身份和代际检查。

代码：`internal/group/healer_coordinator.go`、`internal/boxmgr/healer.go`、`internal/app/healer.go`。已有测试：`TestHealerRefreshCircuitAndAuditFailure`、`TestHealerFailedPromotionFallsBackAndAlerts`。

## Requirement HEAL-002：预算、冷却和恢复阈值不能被绕过

当前默认：每轮 2min、tick 30s、复检 20/min 全局、同节点间隔 1min；订阅 2/min 全局、同订阅至少 5min，连续失败 3 次熔断 30min。全局指同一 Healer 进程，预算采用分钟计数窗口，不是跨实例或严格滑动窗口。复检 MUST 遵守组恢复冷却与连续成功阈值。

### Scenario: 多组共享同一订阅或坏节点

- GIVEN 多组同时出现缺口并引用相同节点或订阅。
- WHEN 同一个 Healer 调谐。
- THEN 共享去重与预算，轮换组起点；不因组数增加放大资源请求。

代码：`internal/group/healer_coordinator.go`、`internal/app/healer.go`、`internal/subscription/manager.go`。已有测试：`TestHealerSharesDedupAndBudgetAcrossGroups`、`TestHealerRechecksRecoveryBeforeRefreshing`。

## Requirement RUNTIME-001：状态迁移、快照发布、通知顺序固定

成员和容量变化 MUST 使用已有增量发布边界，先 ReconcileSilent 与 PrepareUpdate/Commit，快照可见后再 NotifyEvents；不得为了提升备用节点重启 base box/listener。备用 MUST 保留探测和恢复状态，已有连接不得仅因降为备用被关闭。真实流量失败可以进入 Runtime；成功诊断不能成为恢复依据。

### Scenario: 活动槽位变更时存在长连接

- GIVEN 某成员正在传输且因容量策略转为 standby。
- WHEN 发布新活动选择。
- THEN 新选路使用新快照，原连接继续；组状态迁移与通知顺序可解释。

代码：`internal/group/runtime.go`、`internal/outbound/pool`、`internal/boxmgr/healer.go`。已有测试：`TestCapacitySelectionPreservesSlotsAndExplicitEmpty`、`TestStandbyRecoveryPreservesStreakAcrossCapacityChange`、`TestHealingPromotesStandbyWithoutRestartingBoxes`。

## Requirement AUDIT-001：控制面必须诚实表达观测和审计边界

当前认证查询为 `GET /api/groups/{groupID}/healing-events?limit=50`；事件状态为 dry_run/planned/succeeded/failed/deferred/alert。每组限制 500 条和 30 天，5min 内相同状态及动作详情去重；清理发生在成功追加新事件时，不是独立定时清理。审计查询 MUST NOT 被当成无损动作计数器或无限期证据库。

### Scenario: 一组长期不再产生事件

- GIVEN 组停止写入新自愈事件。
- WHEN 已经过了 30 天。
- THEN 不得承诺后台已清理旧记录；独立清理属于阶段 4 计划。

代码：`internal/store/sqlite_healer.go`、`internal/monitor/healer_handlers.go`、`frontend/src/components/GroupHealLog.tsx`。

## 现状未满足的规模能力

健康 EventHub 每订阅为无界 FIFO；`HealingInputs` 多次全量读取；预算和熔断未完整配置化；缺少统一指标及确定的规模报告。WAL、单写连接和四读连接已经存在。统一拓扑事件、多实例权威状态、完整操作审计、备份恢复演练均不能声明已交付。
