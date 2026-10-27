# 代理池当前契约与实现基线

基线：提交 `71d8745`（安装与发布功能版本 `2.1.0`）。本次为静态代码核对；已有测试仅表示存在证据入口，不表示已重新执行或通过基础设施规模验收。阶段性增强见 [变更提案](../../changes/scale-proxy-pool/proposal.md)。

增量：`2.1.1` 的首批健康入口硬化与实际测试证据见 [P1 健康边界](../../changes/scale-proxy-pool/p1-health-boundaries.md)，不等同于 M1 或规模认证。

增量：`2.2.6` 的健康事件投递改造（HEALTH-002..005）见 [健康事件日志](../../changes/health-event-log/proposal.md)；它替换了"每订阅 4096 条 FIFO、溢出丢弃"的旧形态。

## 能力与事实源

| 层 | 权威能力及代码入口 | 当前边界 |
| --- | --- | --- |
| L0 节点事实 | `store`、`nodecodec`、`nodeloc`、`noderegion`、`nodetag`；`groupmember` / `nodeselect` / `nodeview` 统一规则与查询 | 地区证据与身份修订已落库；候选资格与健康/实际入池分开 |
| L1 健康平面 | `probe.Applier`；`monitor` 的 Scheduler、Registry、健康投影及 EventHub | 有持久化收据和投影重试；仍有内存兼容入口需做调用审计 |
| L2 分组策略 | `group.Runtime`、`PlanHealing`、`CapacitySelection`、`Healer` | 主路径已有；规模预算目前为代码常量，非完整配置产品 |
| L3 数据面 | `builder`、`boxmgr`、`outbound/pool` | 不可变快照及 PrepareUpdate/Commit；数据面失败是合法输入 |
| L4 控制面 | `monitor` API、`GroupPoolsPanel`、`GroupHealLog`、`store` 审计 | 已有策略校验和自愈日志；缺口聚合视图及策略变更留痕待补 |
| L5 平台化 | `app` 单进程装配、SQLite、安装与发布工作流 | 单机部署；不提供多实例一致性协议 |

## ADDED Requirement OUTBOUND-001：Clash SS obfs 运行时适配

SS 出站构建 MUST 将插件名 `obfs` 适配为 sing-box 内嵌的 `obfs-local`，并将其 `mode` / `host` 参数映射为 `obfs` / `obfs-host`。同时存在原生键和别名时 MUST 优先采用原生键；参数转义及重复值 MUST 保留语义。适配 MUST 仅发生在出站构建阶段，不修改持久化 URI、节点身份或 Clash 导出格式；原生 `obfs-local` 与其它插件配置 MUST 保持原样。

### Scenario: 订阅包含 TLS 混淆节点

- GIVEN Clash SS 节点配置 `plugin: obfs` 和 `plugin-opts: {mode: tls, host: cdn.example}`。
- WHEN 导入后冷启动、增量重载或全量重载。
- THEN 内嵌插件使用 TLS 混淆及指定 host，不因插件名报错，也不退回 HTTP 模式；节点身份与原始导出配置保持兼容。

代码：`internal/builder/shadowsocks_plugin.go`。测试：`TestClashObfsBuildsSupportedPlugin`、`TestShadowsocksPluginCompatibility`、`TestObfsPluginStartsAndReloads`。

## ADDED Requirement DIAGNOSTIC-002：解锁页仅匹配在线可用节点

解锁页分页、筛选统计、诊断分类和批量目标 MUST 使用同一在线可用基础范围：有 runtime tag 且 nodeview.Healthy 为真；清除筛选、保存筛选或显式选中 ID MUST NOT 绕过该范围。Healthy 沿用现有投影（已启用、完成首次检查、可用、非黑名单、非 provisional）。已选批量检测 MUST 在提交前与当前在线可用 ID 取交集；统计和任务匹配均基于各自读取的当前快照，不保证提交后健康状态不再变化。其它管理页面及未指定 healthy_only 的接口 MUST 保持原范围。

### Scenario: 混合健康节点与检测前状态变化

- GIVEN 已加载节点包含在线可用、待检查、不可用、黑名单及临时放行状态。
- WHEN 打开解锁页、清除/加载筛选或发起批量检测。
- THEN 列表、计数及目标仅包含在线可用节点；已选节点变为不可用时在提交前跳过，节点管理页仍能查看全部节点。

代码：`internal/nodeview/view.go`、`internal/httpapi/nodeview_handlers.go`、`frontend/src/components/UnlockPanel.tsx`。测试：`TestHealthyScopeAgreesAcrossQueryFacetsAndTargets`、`TestNodeViewHealthyScopeUsesCurrentRuntimeHealth`。

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

## Requirement HEALTH-002：健康测量与有序事件日志同事务

每条**被接受**的健康测量 MUST 在与事实、收据同一事务内追加一条不可变日志行（`health_events`，单调 `seq`，携带 NodeID、身份 token/revision、tag、ProbeID、成功/延迟/错误、CheckedAt）。被判为 `obsolete_identity` / `duplicate` / `older_probe` 的结果 MUST NOT 追加；诊断 origin MUST NOT 追加（DIAGNOSTIC-001）。

日志容量有硬上限；达到上限时 MUST 拒绝新的测量提交并显式报错，MUST NOT 静默丢弃旧事实或改由队列丢弃。

### Scenario: 消费者滞后导致日志满

- GIVEN 消费者长期不推进，日志已达容量上限。
- WHEN 新的健康测量提交。
- THEN 提交整体失败（`health event journal full`），事实、收据与日志都不变化；`GET /api/operations/health-events` 的 `rejected_measurements` 递增。

代码：`internal/probe/applier.go`、`internal/store/health_events.go`、`internal/store/migrations.go`（v24）。已有测试：`TestHealthMeasurementAppendsJournalRowInSameTransaction`、`TestHealthMeasurementRejectedWhenJournalFull`、`TestHealthEventJournalRejectsWhenFull`。

## Requirement HEALTH-003：投递按游标有序消费，通知只唤醒

订阅 MUST 从日志按 `seq` 升序读取，并只有在该批回调返回后才推进游标；内存通知只表示"可能有新行"，其合并、丢失或重复 MUST NOT 改变已提交事实的最终投递。被订阅过滤掉的事件 MUST 也推进游标（否则会永久卡住）。`SnapshotOnly` MUST 是可合并的刷新标志，MUST NOT 推进恢复/失败窗口，并在该订阅的日志补读之后投递。

### Scenario: 消费者阻塞期间积压超过旧队列上限

- GIVEN 回调被阻塞。
- WHEN 期间提交 5000 条健康测量（超过旧 FIFO 的 4096）。
- THEN 解除阻塞后 MUST 全部按序投递，`dropped=0`、`backlog=0`；分组恢复进度与逐条投递一致。

代码：`internal/monitor/health_event_hub.go`。已有测试：`TestHealthJournalDeliversBeyondLegacyQueueLimit`、`TestHealthEventSnapshotOnlyIsMergeable`。

## Requirement HEALTH-004：持久消费者从游标续读，重放幂等

需要跨重启生效的消费者 MUST 以稳定 key 注册（pool 为 `pool:<groupID>`/`pool:base`）；首次注册从日志尾部开始，已有游标则从游标续读。游标 MUST 在回调返回后持久化，持久化失败 MUST 阻止该消费者继续前进——不只是记录失败，而是停止读取新行，直到写入成功。原因是进程内 ProbeID 去重环只覆盖一个投递批次：带着未落盘的进度继续前进，崩溃重启后会重放已被挤出环的事件并二次推进恢复计数。同一测量被重复投递或重放时 MUST NOT 二次推进恢复计数或二次计入失败窗口。

游标 MUST 与该批事件产出的 `group_node_states` 在同一事务内提交。消费者回调因此 MUST 返回它造成的状态变更（`func(HealthResultEvent) []store.GroupNodeState`），只读观察者返回 `nil`。分成两次写入会让"已应用"与"应用结果"可能不一致：崩溃时游标领先，踢出记录丢失，重启后坏节点以健康身份复活。因此 consumer 的返回值里 MUST 包含跨分组传播的那些行（踢出/恢复是节点级事实）。

状态行指向的分组或节点已被删除（外键拒绝）MUST 视为该行无处可去：丢弃该行并继续推进游标，MUST NOT 当成可重试失败，否则一个已删除目标会永久卡住该消费者并拖住其它分组。

`group_node_states` 存在第二个异步写者（应用层合并持久化器）。每行 MUST 带单调 `revision`，写入 MUST 只在 `revision = 0` 或不小于已存值时生效，否则晚到的旧快照会覆盖新状态（例如把刚解除的踢出写回去）。`revision` MUST 随存储→配置→运行时往返并在启动时抬升全局时钟，否则重启后时钟归零会让所有新写入被当成过期。

### Scenario: 崩溃后重启

- GIVEN 事件已应用但游标未落盘，进程崩溃。
- WHEN 重启后从旧游标重放该批事件。
- THEN 已应用的 ProbeID 被去重，恢复计数不变；新事件照常推进。

### Scenario: 状态写入失败

- GIVEN 回调判定踢出。
- WHEN 提交时状态写入失败。
- THEN 游标 MUST NOT 前进，消费者停止投递并退避重试；恢复后两者一起落库。

代码：`internal/monitor/health_event_hub.go`、`internal/monitor/manager.go`（`SubscribeDurableHealthResults`）、`internal/group/runtime.go`（`SnapshotStates`）、`internal/store/health_events.go`（`AdvanceHealthCursorWithState`）、`internal/outbound/pool/pool.go`、`internal/app/app.go`（异步持久化器）。已有测试：`TestHealthEventCursorResumesAfterRestart`、`TestHealthEventFirstRegistrationStartsAtTail`、`TestHealthEventCloseKeepsJournalForNextStart`、`TestHealthEventCursorFailureStopsDelivery`、`TestHealthEventCursorAndStateCommitTogether`、`TestAdvanceHealthCursorWithStateIsAtomic`、`TestAdvanceHealthCursorSkipsStateForDeletedGroup`、`TestUpsertGroupNodeStateRejectsStaleRevision`、`TestGroupStatePersisterKeepsEveryMemberUnderBurst`、`TestHandleHealthResultReturnsDurableState`、`TestHealthReplayDoesNotAdvanceRecoveryTwice`、`TestHealthFailureReplayIsIgnored`。

## Requirement HEALTH-005：投递状态必须可观测

控制面 MUST 暴露日志容量、当前行数、`seq` 上界、积压、最老事件年龄、被拒测量次数，以及每个消费者的游标、已投递、空洞、读失败与游标失败次数。

清理 MUST 只删除"超过保留窗口**且**所有**当前注册**的持久消费者都已越过"的行；已不再注册的消费者（分组被删除、被禁用、实例停止）留下的游标行 MUST NOT 约束清理，它恢复注册时从自己的游标续读，中间被清理的行计为空洞。否则一个不活跃消费者会永久钉住最慢位置，日志被填满后**所有**分组的测量都被拒绝。

历史保留 MUST 服从容量约束，不能让已确认的历史永久占满日志并拒绝正常测量。当前常态已消费历史保留目标为 5 分钟、无注册消费者为 30 分钟，每分钟年龄清理；每秒检查容量，达到 75% 水位时将本次历史窗口缩短为 0，仅回收所有活跃消费者已持久确认的前缀。活跃消费者缺少游标 MUST 视为 0，不能视为已经消费至尾部。未消费积压与瞬间超过剩余空间的突发仍执行 8192 硬上限背压，MUST NOT 通过丢弃未确认记录接纳新测量。统计 MUST 报告压力阈值、成功压力清理次数与回收行数。

### Scenario: 消费者卡死

- GIVEN 某持久消费者回调永久阻塞。
- WHEN 该消费者游标不再前进。
- THEN 积压持续上升并可在统计接口看到；最老事件的保留条数不被清理掉，直到达到容量上限转为显式背压。

### Scenario: 消费跟得上但年轻历史达到容量水位

- GIVEN 所有活跃消费者已持久确认当前事件，日志达到 6144 行且年龄小于五分钟。
- WHEN 容量检查执行。
- THEN 已确认历史可以提前回收，新测量继续接纳；缺少游标或尚未确认的消费者仍阻止回收其债务。

### Scenario: 分组被删除后重新创建

- GIVEN 分组 9 的 pool 曾消费到 seq 3 后被删除，日志继续增长到 seq 200。
- WHEN 该分组的游标行仍在，消费者已不再注册。
- THEN 清理不受该行约束，活跃消费者的进度正常释放空间；分组重建后从 seq 3 续读并记一次空洞，不重放已清理的转移。

代码：`internal/monitor/health_event_hub.go`（`stats`、`durableConsumers`）、`internal/monitor/manager.go`（`pruneHealthJournal`）、`internal/httpapi/server_health_events.go`、`internal/store/health_events.go`（`PruneHealthEvents`）。已有测试：`TestPruneHealthEventsKeepsSlowestConsumerBacklog`、`TestPruneHealthEventsIgnoresInactiveConsumerCursor`、`TestHealthEventJournalSustainsConfiguredProbeRate`、`TestHealthEventStatsReportsJournalBacklog`。

## Requirement DIAGNOSTIC-001：诊断不产生路由健康成功

测速、延迟诊断、质量和解锁结果 MUST 受 origin/kind 和单 payload 校验约束；可以写自身诊断事实、触发标签或成员规则更新，MUST NOT 伪造健康成功、清除 EVICTED 或推进恢复计数。真实数据面失败和显式人工恢复是不同的受控输入，不归入诊断。

### Scenario: 已剔除节点诊断成功

- GIVEN 节点处于 EVICTED，恢复成功次数尚未达到阈值。
- WHEN 手动测速、解锁或质量检测成功。
- THEN 诊断可展示成功，节点仍保持原剔除状态与恢复计数；只有合法健康复检能推进恢复。

代码：`internal/probe/applier.go`、`internal/httpapi/probe_applier.go`、`internal/httpapi/nodecheck.go`。已有测试：`TestManualResultCannotPublishHealth`、`TestNodeCheckLatencyDoesNotMutateHealth`；完整剔除×各诊断类型矩阵待补。

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

## ADDED Requirement RUNTIME-002：连接仅保留选中运行时引用

TCP/UDP 选路和未完成的拨号重试 MUST 持有同一快照，防止候选 outbound 在拨号过程中被关闭。连接建立后 MUST 在释放快照前取得选中 memberRuntime 的 operation 引用；已建立连接 MUST NOT 因自身生命周期继续钉住整份旧拓扑。关闭（含重复关闭）MUST 只释放一次该连接的 operation 和活动计数；半关闭能力与错误归因保持原有语义。

### Scenario: 热更新发生在拨号中，随后连接长期存活

- GIVEN 拨号已加载旧快照且尚未返回。
- WHEN 发布新拓扑并退休旧成员，之后原拨号成功建立连接。
- THEN 拨号返回前旧候选仍受保护；返回后无关旧成员可独立排空，选中成员必须等待自身连接关闭，不能被提前关闭。

代码：`internal/outbound/pool/pool.go`。测试：`TestConnectionPinsOnlySelectedRuntime`（TCP/UDP）、`TestRetiredMemberWaitsForLoadedSnapshotReader`、`TestDialContextKeepsLoadedSnapshotAcrossRetry`。

## Requirement AUDIT-001：控制面必须诚实表达观测和审计边界

当前认证查询为 `GET /api/groups/{groupID}/healing-events?limit=50`；事件状态为 dry_run/planned/succeeded/failed/deferred/alert。每组限制 500 条和 30 天，5min 内相同状态及动作详情去重；清理发生在成功追加新事件时，不是独立定时清理。审计查询 MUST NOT 被当成无损动作计数器或无限期证据库。

### Scenario: 一组长期不再产生事件

- GIVEN 组停止写入新自愈事件。
- WHEN 已经过了 30 天。
- THEN 不得承诺后台已清理旧记录；独立清理属于阶段 4 计划。

代码：`internal/store/sqlite_healer.go`、`internal/httpapi/healer_handlers.go`、`frontend/src/components/GroupHealLog.tsx`。

## 现状未满足的规模能力

健康事件采用持久日志与游标，游标和分组状态同事务提交（HEALTH-002..005）；多进程共用同一数据库不受保证。自愈每轮全组读取一次，其后只重查目标组；预算和熔断未完整配置化。除健康投递外仍缺统一指标及确定的规模报告。统一拓扑事件、多实例权威状态、完整操作审计、备份恢复演练均不能声明已交付。

## Requirement CONTROL-001：节点分页保持一致快照与有界详情读取

节点视图 MUST 在读连接池的一致性事务中加载节点、标签和事实，不占用唯一写连接。分页页面 MUST 只加载当前页 ID 的详情/结果；跨页选择保留 ID。详情接口的空 ID 列表 MUST 返回空，非法 ID MUST 拒绝，不能降级为全量读取；原有认证和订阅可见性保持不变。

### Scenario: 查询时写入、空页和跨页选择

- GIVEN WAL 读快照正在生成，用户在多页选择了节点。
- WHEN 写事务提交、页面切换或请求空 ID 列表。
- THEN 写入可继续，当前查询仍看到原快照；跨页操作使用已选 ID，空列表不读取全体节点。

代码：`internal/store/sqlite.go`、`internal/httpapi/nodeview_handlers.go`、`internal/httpapi/node_ids.go`、`frontend/src/components/ManagePanel.tsx`、`frontend/src/components/UnlockPanel.tsx`。测试：`TestSQLiteReadTransactionSnapshotAllowsWriter`、`TestPagedResultReadsNeverWidenIDs`、`TestListManagedNodes`。

## Requirement CONTROL-002：检测 SSE 终态可达且连接可退出

初始任务快照与订阅 MUST 原子建立；任务结束时终态 MUST 不因进度队列已满而丢失。首帧保持 task 兼容格式，已完成任务补发 done 后退出。写入/flush MUST 有期限，服务关闭或请求取消后解除订阅。

### Scenario: 初始 Flush 时任务完成

- GIVEN 初始快照已读取，首次 Flush 尚未返回。
- WHEN 任务完成或慢读者填满事件队列。
- THEN 仍能读取终态；写失败或服务关闭会结束连接。

代码：`internal/httpapi/nodecheck.go`、`internal/httpapi/nodecheck_api.go`。测试：`TestNodeCheckTerminalSurvivesFullSubscriber`、`TestNodeCheckStreamLifecycle`、`TestNodeCheckTaskEventsKeepsInitialEventShape`。

## ADDED Requirement PERFORMANCE-001：性能采样显式启用且有界

profile MUST 默认关闭，并同时受管理会话、非空管理密码与真实 loopback 对端约束，不信任转发头。只开放 heap/allocs/goroutine/cpu 二进制采样；CPU 时长 MUST 在 1–30 秒内，默认 10 秒，HEAD 不得启动采样。进程内最多一个 profile，请求取消或 Server 关闭 MUST 停止 CPU 采集，响应写入 MUST 有期限。

### Scenario: 本机与远程请求同时采样

- GIVEN 开启了性能采样，管理密码与会话有效。
- WHEN 远程请求伪造 X-Forwarded-For，或第二个本机请求在采样期间到达。
- THEN 远程请求被拒绝，并发请求返回 429；已进行的采样在取消/关闭后释放进程级占位。

资源接口 MUST 只返回聚合数量，不能暴露节点凭据；RSS 不可用时明确标记，Go heap、Go 管理内存估计与 RSS 不得混同。分组统计是跨 slot 的观测，不承诺与并发重载原子一致。

代码：`internal/httpapi/server_resources.go`、`internal/boxmgr/resources.go`。测试：`TestProfileAccessAndParameters`、`TestCPUProfileCancellationShutdownAndConcurrency`、`TestProfileReadableByGoTool`、`TestProfileOptInAndHead`、`TestResourceSnapshotReportsMemoryAndNoCredentials`、`TestRuntimeResourcesCountsOverlapAndExcludesStoppedSlots`。

## Requirement LIFECYCLE-001：应用退出共用预算并保持清理顺序

Run 退出 MUST 先取消生产者，所有清理共享三十秒预算，超时 MUST 返回错误交给进程入口结束进程。最终统计和状态写入 MUST 接收预算 context；不可取消的第三方关闭函数只能留在同一条清理链内等待，MUST NOT 为跳过它而提前关闭其后的资源。

### Scenario: 第三方关闭函数阻塞

- GIVEN 关闭链仍有一个未返回的第三方函数。
- WHEN 退出预算耗尽。
- THEN Run 返回超时错误；清理链仍按顺序执行，不提前关闭链尾数据库。

代码：`internal/app/shutdown.go`、`internal/app/app.go`、`internal/nodetag/queue.go`。测试：`TestShutdownBudgetPreservesResourceOrder`、`TestQueueShutdownCancelsActiveRecompute`。

## Requirement SUBSCRIPTION-001：并发抓取与未提交结果同时有界

批量订阅刷新 MUST 保持订阅顺序提交，最多持有三个未提交抓取结果；已可提交的前项不能等待全体抓取完成。版本门禁、失败/304 窄写与最后一次运行时对账 MUST 保留，取消 MUST 回收所有流水线 worker。

### Scenario: 首项或后项提供者缓慢

- GIVEN 一批订阅中某项尚未返回。
- WHEN 后续抓取完成或调用方取消。
- THEN 首项缓慢形成背压，后项缓慢不阻止前项提交；取消后所有抓取退出。

代码：`internal/subscription/manager.go`。测试：`TestRefreshPipelineBackpressureAndEarlyCommit`、`TestRefreshDueFetchesConcurrently`。

## Requirement AUTH-001：使用元数据与认证吞吐解耦

API Key 使用记录 MUST 合并同一 Key 的最新观测，使用单一消费者、节流与失败重试，不为每次请求创建 goroutine。关闭 MUST 在预算内尝试保存最后观测；换库 MUST 隔离旧 ID，旧时间不能覆盖较新时间。队列上限触发时 MUST 显式记录元数据丢弃，不能影响认证结果。

### Scenario: 同一 Key 突发访问和短暂写入失败

- GIVEN 同一 Key 在一分钟内访问多次，数据库暂时写失败。
- WHEN 新观测抵达、失败恢复或应用退出。
- THEN 最新时间保留待重试，写入受节流约束，退出尝试最终写入。

代码：`internal/controlplane/auth_usage.go`、`internal/store/sqlite.go`。测试：`TestAPIKeyUseCoalescesAndFlushesLatest`、`TestAPIKeyUseRetriesWithoutDroppingNewObservation`、`TestAPIKeyUseShutdownBudgetAndRebinding`、`TestAPIKeyUsageQueueIsBounded`。

## Requirement DEPLOY-001：Linux 服务注册和自启可独立管理

二进制默认安装 MUST 注册当前 Linux 服务管理器的服务、启用开机自启并启动；已有本地程序的 register 和各服务管理动作 MUST 不依赖 Release 下载。默认 install 的 no-start MUST 保持仅文件安装语义。禁用自启不停止当前进程；注销服务 MUST 保留程序、配置和数据库。非法环境或服务归属冲突 MUST 拒绝操作，启动/自启验证失败不得记录安装成功。

### Scenario: 已有程序补注册、关闭自启和注销

- GIVEN 目录里有可执行程序、配置与数据库，Linux 使用 systemd 或 OpenRC。
- WHEN 离线注册，随后禁用自启或注销服务。
- THEN 注册可重复执行；禁用只改开机行为，注销停止服务但保留已有文件；下载和无关服务不受影响。

代码：`install.sh`。测试：`tests/test_install.py`（离线注册、systemd/OpenRC 生命周期、归属保护、失败与数据保留）。部署说明：`docs/deployment.md`。测试中的服务管理器是模拟，不代替真实服务器重启验收。

## ADDED Requirement DEPLOY-002：中文安装与保留状态的更新

安装器默认帮助与提示 MUST 使用中文，默认安装目录 MUST 为 `/opt/easy-proxies`，并允许 `--dir` 覆盖。`--action update` 和 `--update` MUST 等效且要求已有安装；更新 MUST 保留配置、节点文件和数据库。默认二进制更新 MUST 保留服务文件、自启状态及运行/停止状态，原先运行时才重启；Docker 更新 MUST 校验容器归属及三个标准挂载源，保留重启策略和运行/停止状态。下载、校验或拉取失败 MUST 不替换旧程序/容器，更新失败 MUST 不写入新的成功版本记录。

### Scenario: 已停止且禁用了自启的服务更新

- GIVEN 已有本地程序和配置，服务已停止且禁用了开机自启。
- WHEN 使用 update 更新版本。
- THEN 程序替换后服务仍停止且自启仍关闭；配置和数据保留。使用 no-start 时仅准备文件/镜像，现有进程/容器不被重启或替换。

代码及测试：`install.sh`、`tests/test_install.py`。操作语义、版本记录与失败边界见 `docs/deployment.md`。
