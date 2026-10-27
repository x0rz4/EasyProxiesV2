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
| `builder` | `builder.go`、`shadowsocks_plugin.go` | config+store → sing-box `option.Options`；SS 混淆插件运行时适配 |

## 关键不变量

SS 插件在 `shadowsocksRuntimePlugin` 适配 Clash 与 sing-box 的格式差异。适配仅作用于构建出的出站，不回写 URI、Spec、身份或导出配置；行为契约见 OUTBOUND-001。sing-box 内嵌实现插件，无需安装外部 obfs 可执行文件。

1. **快照只读、选路无锁**：数据面读路径只做一次 `atomic.Pointer.Load()`；写侧走 `PrepareUpdate` → `Commit`，`PrepareUpdate` 失败必须 `Rollback` 并清理已创建成员。
2. **代际不可覆盖**：出站 tag 命名唯一在 `runtimetag`（`<16hex>@vN`）。同 tag 重复 `CreateMember` 会关掉旧 outbound、无法排空，所以变更节点必须换新 `@vN`。
3. **成员判定只有一份**：`groupmember.NewFilter().Allow()`（内部即 `nodeselect.Filter`）。builder 与 boxmgr 都走它，顺序契约固定：组未启用 → `excluded` 无条件排除 → `explicit` 无条件加入（绕过黑白名单）→ region 命中 + tag 黑/白名单（`any`/`all`）。未解析 tag ID 变成永不匹配哨兵（宁可缩小，不许放宽）。
4. **失败记账归属**：普通模式 → `sharedMemberState`（阈值→临时黑名单）；分组模式 → `group.RecordFailure`（滑动窗口→SUSPECT/EVICTED，按 NodeID **全局传播到所有分组**）。恢复同样全局；自动恢复参数是每分组一项配置。**调用方取消/超时不计入成员失败**（`dialFailureAttributable`）。
5. **锁边界是硬约束**：`boxmgr.mu` 只保护管理字段与 box 生命周期，`reloadMu` 串行化生命周期操作；**禁止持锁做网络拨号、解析订阅或等待 drain**。
6. **事件与快照的顺序**：`ReconcileSilent` 先迁移状态，**等匹配的 pool 快照可见后再 `NotifyEvents`**（护栏 6）。
7. **成员 outbound 不能声明静态依赖**：`CreateMember` 后 `Dependencies()` 非空会立即失败并回滚（否则 sing-box `dependByTag` 无法随快照更新）。

## 选路与连接引用

每次发布独立的只读拓扑与索引；同一快照内，候选内容和顺序相同的 TCP/UDP/全体视图共用切片和索引，空协议视图不分配索引。按延迟排序前复制网络列表，避免改写拓扑顺序；不同版本不共享这些容器。monitor 的每节点 timeline 在首次实际事件时才分配，保留原有 20 条上限和迁移历史。

`internal/outbound/pool/pool.go` 中 TCP/UDP 在选路和全部拨号重试期间持有同一份快照。连接建立后，包装器先增加选中 `memberRuntime.operations`，函数返回时释放快照；关闭连接只释放该成员的 operation 和活动连接计数，重复关闭不会重复递减。这样长连接保护自己的 outbound，同时允许无关退休成员排空，旧快照的整份拓扑和索引也不再因该连接保留。拨号未返回时仍保护全部重试候选，不能提前释放快照。契约见 RUNTIME-002。

固定模式失效回退沿拓扑循环查找下一个候选，复用快照的 TCP/UDP/全体候选索引，最坏 O(N)、零额外分配。控制面传入任意子集或重排候选时临时构建索引，O(N+K)，保留拓扑优先和返回候选下标的语义。`balance` 仍逐连接扫描候选求精确最小活动数。

基础池 `balance` 在遇到首个 0 连接成员时提前结束，保持最少连接和平局时拓扑优先的规则，全部繁忙时仍 O(N)。显式 `pool.mode: balance_p2c` 每连接从同一 TCP/UDP 候选快照抽取两个不同成员，选择观测连接数较少者，平局使用第一个随机候选，O(1)、零选路分配；只有一个候选时直接使用它。随机数使用 Go 的并发安全 math/rand/v2 顶层函数，不争用池级随机状态。此模式不保证全池最少连接，未更改默认值、已有 balance 语义、分组 fixed/lowest_latency/random 白名单、健康与重试边界。见 RUNTIME-003。

## 三条重载路径（boxmgr）

池的 TCP 连接包装只在底层实际支持时暴露 `CloseRead` / `CloseWrite`。缺少写半关闭能力时由 sing-box 走完整关闭，仍通过 tracked 连接释放计数；真实半关闭不释放活动连接。不能靠忽略半关闭错误替代关闭。

节点集合的唯一来源是 `runtimenodes.Load`（enabled 非订阅 + enabled 订阅可达，按 identity 去重）。冷启动 `Start` / 增量 `reloadNodesIncrementally` / 全量蓝绿 `reload` 三条路径都必须走它，不得各自过滤 `enabled`。

`groupAppliedFingerprint` 以 canonical JSON 编码运行配置形态后计算 SHA-256，仅保留 64 字符摘要；成员先走统一谓词，再纳入原 YAML 指纹包含的 Name/URI/Port/Username/Password。ID、地区、标签和身份缓存不直接参与连接形态，空 refill 与 nil 等价；监听、策略、健康/恢复和 healing 配置仍参与比较。各 box 仍独立拥有 outbound，sharedMemberState 不承担跨 box 的关闭归属。

## 自愈链（HEAL）

`internal/boxmgr/resources.go` 实现 `controlplane.RuntimeResourceReader`，统计运行中的 base/分组实例、成员重叠倍数与退休成员数；只输出数量，不构建配置或获取 reloadMu。入口与指标边界见 [性能观测](../performance.md)。

`group.PlanHealing`/`CapacitySelection` → `boxmgr.ApplyHealingSelection`（重 Plan → 增量 PrepareUpdate/Commit → NotifyEvents）。promote → probe → 再 promote → refresh；每阶段重读政策与状态；**planned 审计失败必须阻断动作**。预算/冷却见 spec Requirement HEAL-002。运维手册见 [group-healing](../../plane/group-healing.md)（不重新实现 Healer）。

## 探测链（L1）

不响应取消的探测调用在实际返回前始终保留占位与 detached 计数，不按时间遗忘；同一个运行时调用不能重入。调度器另有 512 个真实未退出调用的总上限，覆盖运行时代际更替；触顶显式拒绝新调用。卡死的 Go 调用无法被强杀，须由底层连接退出或替换运行时恢复。

`monitor.beginHealthResult` → `probe.Applier`（收据/幂等/身份/**日志行**） → `ProjectHealthOutcome` → `health_applier` → 运行态 entry → `HealthResultEvent`（**只作唤醒**） → 消费者按游标读 `health_events`。诊断 origin（`nodedetect`、`unlock`）**绝不发布健康事件**；`SnapshotOnly` 是可合并的刷新标志，只要求重读快照，MUST NOT 推进恢复/失败窗口（spec HEALTH-001/002/003、DIAGNOSTIC-001）。

当前投递形态（2.2.6 起）：被接受的健康测量在**测量事务内**追加一条 `health_events` 行（`seq` 单调），订阅从日志按游标有序消费。pool 是持久消费者（key `pool:<groupID>`/`pool:base`），重启后从游标续读，重复投递按 ProbeID 幂等（`groupMemberRuntime.appliedProbes`，环深 64 = 一个投递批次）。日志容量 8192，满时**拒绝新测量并报错**（显式背压）。

清理边界有两处容易搞反，改之前先读 spec HEALTH-005：

- 只有**当前注册**的持久消费者约束清理。分组被删除/禁用/实例停止后留下的游标行不参与计算——否则它会把最慢位置永久钉住，日志填满后所有分组的测量都被拒绝。
- 常态按 5 分钟历史窗口释放（无注册消费者时 30 分钟），每分钟年龄清理；每秒额外检查容量，到达 75% 水位（6144 行）时提前回收已持久确认的前缀。历史保留是容量允许时的目标。真正未消费的积压仍受 8192 硬上限和显式背压约束，突发超过剩余容量也仍可被拒绝。活跃消费者缺少持久游标时按 0 处理，不能默认已读到尾部。

游标持久化失败会**停止**该消费者继续读取（不是只记一次失败），因为去重环只覆盖一批，带着未落盘进度前进会在重启后二次推进恢复计数。积压/最老年龄/游标见 `GET /api/operations/health-events`。关闭只取消等待，未投递的行保留给下次启动；已开始的回调可在 close 返回后完成。无 store 的内存模式仍走有界直投（1024），是兼容/测试路径，不是生产契约。契约与边界见 spec HEALTH-002..005 与 [健康事件日志设计](../../plane/changes/health-event-log/design.md)。

游标与它造成的状态**同事务**提交：消费者回调返回 `[]store.GroupNodeState`，hub 用 `AdvanceHealthCursorWithState` 一次写入游标 + `group_node_states`。分成两次写，崩溃就可能在游标领先的状态下丢掉一条踢出记录，重启后坏节点以健康身份复活。回调要带上**跨分组传播**的行（踢出/恢复是节点级事实），所以 `SnapshotStates` 扫所有分组。指向已删除分组/节点的行直接丢弃并继续推进游标——那种行无处可去，重试会永久卡住消费者。

`group_node_states` 还有第二个写者：`internal/app` 的异步合并持久化器（当前节点与非健康转换）。两者都写"转换发生时的快照"，所以每行带单调 `revision`，写入只在 `revision = 0` 或不小于已存值时生效，否则晚到的旧快照会把刚解除的踢出写回去。`revision` 随 store→config→runtime 往返，启动时用它抬升全局时钟。

自愈每轮 HealingInputs 仅构建一次全体输入。后续阶段和动作统一使用 HealingInput(groupID) 重读目标组，复用同一候选构造器和成员谓词；组运行快照只锁目标组，不扫描其它组。策略、健康与资格仍在执行前重新读取，审计和预算语义不变。
