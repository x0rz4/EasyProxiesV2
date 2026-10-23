# 设计：健康事件日志与游标投递

## 1. 现状与差异

| 环节 | 现状（2.2.5） | 本批 |
| --- | --- | --- |
| 事实与投递 | 测量事实落库，事件走内存 FIFO | 事实与**日志行同一事务**落库 |
| 投递载体 | 事件本身排队 | 只排队**“有新事件”信号**；事件从日志读 |
| 顺序 | 单队列 FIFO | 单日志 seq 全序，按游标有序读 |
| 溢出 | 丢弃事件（会改状态机语义） | 拒收新测量 + 显式错误；消费者滞后不再丢已提交事实 |
| 重启 | 积压丢失 | 持久消费者从游标续读 |
| `SnapshotOnly` | 排队，可能丢 | 独立**可合并**标志，最后投递，不推进计数 |
| 观测 | `Dropped()` 无生产调用 | 积压/最老年龄/游标/重放/空洞/失败 可查 |

为什么不能只把队列放大：队列越大只是把“什么时候丢”推远，仍会在慢消费者上丢；而丢弃是会改状态机的语义损失，不是 UI 延迟。

## 2. Requirement HEALTH-002：测量与有序日志同事务

每条**被接受**的健康测量 MUST 在测量事务内追加一条不可变日志行；被拒绝（`obsolete_identity` / `duplicate` / `older_probe`）或事务失败的结果 MUST NOT 追加。

日志行字段：`seq`（单调、不复用）、`node_id`、`identity_token`、`identity_revision`、`tag`、`probe_id`、`success`、`latency_ms`、`error`、`checked_at`、`recorded_at`。

`seq` 用 `INTEGER PRIMARY KEY AUTOINCREMENT`：清理后不复用编号，否则游标会指到“另一条事件”。

### Scenario: 事务失败不发布

- GIVEN 收据或事实写入失败。
- WHEN `Apply` 回滚。
- THEN 日志不新增行，`Outcome.Applied=false`，订阅者不会被唤醒。

## 3. Requirement HEALTH-003：通知只唤醒，事实按游标读

`HealthResultEvent` 的投递 MUST 从日志按 `seq` 升序进行；内存通知只表示“日志可能有新行”，其丢失 MUST NOT 导致已提交事实丢失。

- 订阅持有游标 `cursor`（最后已投递的 `seq`）。
- 投递循环：读 `seq > cursor` 的一批（上限 64）→ 按订阅的 tag/NodeID 过滤 → 逐条回调 → 推进游标。
- 被过滤掉的事件**也**推进游标，否则消费者会卡在无关事件上。
- 回调失败/阻塞时不再预读下一批（有界内存 + 自然背压）。
- 读日志失败 MUST 退避重试并计入 `read_failures`，不得把游标跳过。

### Scenario: 4096+ 积压后仍无损

- GIVEN 消费者回调被阻塞。
- WHEN 期间提交 5000 条健康测量（远超旧队列 4096）。
- THEN 解除阻塞后 5000 条全部按序投递；分组恢复进度与逐条投递一致；`dropped` 恒为 0。

### Scenario: 订阅晚于事件

- GIVEN 消费者已持久游标 100，日志已有 120。
- WHEN 进程重启后重新注册同一 key。
- THEN 从 101 续读到 120；重启前已投递的 1..100 不再重复投递。

## 4. Requirement HEALTH-004：持久消费者与游标

- 持久消费者以**稳定 key** 注册（pool：`pool:<groupID>`，非分组池为 `pool:base`）。
- 首次注册（无游标行）MUST 从当前日志尾部开始，避免把历史事件整体重放成“启动即批量踢出/恢复”。
- 游标 MUST 在回调成功返回后持久化；持久化失败 MUST 记 `cursor_failures` 并**停止该消费者继续投递**（重试同一 cursor），不得带着未落盘的进度继续前进。
- 临时消费者（HTTP 观察、测试）不持久游标，进程内游标从尾部开始；滞后到日志已被清理时计一次 `gaps` 并跳到最老可用 `seq`，即“已消费 + 空洞”可加总为投递总量。

### Scenario: 崩溃窗口内的重复

- GIVEN 回调已把事件应用到分组状态，游标写入前进程崩溃。
- WHEN 重启后从旧游标重放。
- THEN 同一 `probe_id` 的重复成功/失败 MUST NOT 二次推进恢复计数（见 §5）；重复只可能发生在崩溃所在的那一批（≤64），不会无限放大。

反向边界（本批不修，如实记录）：分组状态经 `group.SetGroupStateObserver` 接到的 `internal/app` 持久化器**异步**落库（`groupStatePersister`，队列 256，满时丢事件）。游标在回调返回后立即持久化，因此崩溃时游标可能**领先于**已落库的分组状态——那种情况下丢的是"一次踢出/恢复记录"，不是重复应用。要彻底消除，需要游标与 `group_node_states` 写入同一事务。

## 5. Requirement HEALTH-005：按 ProbeID 幂等

`group.RecordHealthSuccess` / `RecordGroupHealthFailure` MUST 接受事件身份（`probe_id`），同一成员的最近 64 个已应用 `probe_id` 构成去重环（覆盖一整批 64 条重放）：命中则整条不再生效（不增恢复计数、不重置失败窗口、不重复通知）。

- `probe_id` 为空表示“非日志来源”（手工/测试直调），保持原有语义，不做去重。
- 去重环随分组运行态重建而清空；跨重启的重复由持久游标负责，环只覆盖进程内重放。

## 6. Requirement HEALTH-006：容量、背压与保留

- 日志容量上限 `HealthEventJournalLimit`=8192 行。
- 清理条件：`recorded_at < now-window` **且** `seq <= MIN(**当前注册**的持久消费者游标)`；没有注册消费者时按 30 分钟年龄清理。**最慢的活跃消费者之后的任何行都不清理**——否则等于静默丢失已提交事实。
- 游标行可以比消费者活得久（分组被删除、禁用、实例停止）。这类行 MUST NOT 参与清理约束：它会把最慢位置永久钉在旧 seq 上，日志填满后**所有**分组的测量都被拒绝，一个不活跃分组就能拖垮全站健康测量。它重新注册时从自己的游标续读，中间被清理的行计一次空洞。
- `window` 不是单一常量：已消费的行按 `healthEventConsumedRetention`=5 分钟清理（没有任何消费者时退回 `healthEventRetention`=30 分钟）。稳态日志大小 = `速率 × window`，5 分钟窗口在 8192 容量下约合 27 行/秒，远高于任何可配置探测间隔；若沿用 30 分钟，约 4.5 行/秒（≈137 个节点按 60s 探测）就会在消费者完全跟上的情况下持续触顶。
- 追加时若已达上限：返回 `store.ErrHealthEventJournalFull`，`Apply` 整体失败（不落事实、不追加），探测调度把错误记入节点 `LastError` 与日志。这是显式背压：宁可让测量失败可见，也不丢旧事实。
- 清理周期 1 分钟，由 monitor 后台执行。

### Scenario: 满容量

- GIVEN 消费者被永久阻塞，日志已达 8192。
- WHEN 新的健康测量提交。
- THEN `Apply` 返回 `health event journal full`；事实与日志均未变化；节点错误可见；解除阻塞后容量恢复后测量重新成功。

### Scenario: 不活跃消费者的陈旧游标

- GIVEN 分组 9 被删除，`pool:9` 游标停在 seq 3；其余活跃消费者都已跟上。
- WHEN 清理运行。
- THEN 已消费且超过窗口的行按活跃游标释放；`pool:9` 的行不被改写。分组 9 重建后从 seq 3 续读并记一次空洞。

## 7. Requirement HEALTH-007：可观测与关闭

`Manager.HealthEventStats()` 与 `GET /api/operations/health-events`（会话认证）返回：

| 字段 | 含义 |
| --- | --- |
| `capacity` / `count` / `backlog` | 上限、当前行数、`count - MIN(游标)` 估算积压 |
| `oldest_age_seconds` | 最老未清理事件的年龄 |
| `consumers[]` | key、游标、`delivered`、`replayed`、`gaps`、`read_failures`、`cursor_failures` |
| `rejected_measurements` | 因容量被拒的测量次数 |

关闭语义：`hub.close()` 只取消等待并拒绝后续入队；已开始的回调可以完成，未投递的日志行**保留**给下次启动，不再像 FIFO 那样随进程消失。关闭路径不得持有 manager/pool 锁等待回调。

## 8. 迁移

migration v24（只追加）：

```sql
CREATE TABLE health_events (
  seq               INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id           INTEGER NOT NULL,
  identity_token    TEXT    NOT NULL DEFAULT '',
  identity_revision INTEGER NOT NULL DEFAULT 0,
  tag               TEXT    NOT NULL DEFAULT '',
  probe_id          TEXT    NOT NULL DEFAULT '',
  success           INTEGER NOT NULL DEFAULT 0,
  latency_ms        INTEGER NOT NULL DEFAULT 0,
  error             TEXT    NOT NULL DEFAULT '',
  checked_at        TEXT    NOT NULL DEFAULT '',
  recorded_at       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_health_events_recorded ON health_events(recorded_at);
CREATE TABLE health_event_cursors (
  consumer   TEXT    PRIMARY KEY,
  seq        INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT    NOT NULL DEFAULT ''
);
```

升级后旧库没有历史事件：首次启动时所有持久消费者从尾部开始，行为与 2.2.5 一致。

## 9. 验收矩阵

| 维度 | 用例 |
| --- | --- |
| 无损 | `TestHealthJournalDeliversBeyondLegacyQueueLimit`（5000 > 4096） |
| 顺序 | 同上，断言 `seq` 严格递增 |
| 重启续读 | `TestHealthEventCursorResumesAfterRestart` |
| 首注册从尾部 | `TestHealthEventFirstRegistrationStartsAtTail` |
| 幂等重放 | `TestHealthReplayDoesNotAdvanceRecoveryTwice`、`TestHealthFailureReplayIsIgnored` |
| 同事务 | `TestHealthMeasurementAppendsJournalRowInSameTransaction`（诊断不写日志） |
| 背压回滚 | `TestHealthMeasurementRejectedWhenJournalFull`（无收据、状态不变）、`TestHealthEventJournalRejectsWhenFull` |
| 清理边界 | `TestPruneHealthEventsKeepsSlowestConsumerBacklog`、`TestPruneHealthEventsWithoutConsumersUsesAgeOnly`、`TestPruneHealthEventsIgnoresInactiveConsumerCursor` |
| 容量可持续 | `TestHealthEventJournalSustainsConfiguredProbeRate`（已消费行按 5 分钟窗口释放；复用旧 30 分钟窗口会被该断言挡住） |
| 游标失败停投 | `TestHealthEventCursorFailureStopsDelivery`（注入游标写失败后只停在 1 批，恢复后继续） |
| 关闭 | `TestHealthEventCloseKeepsJournalForNextStart` |
| 可观测 | `TestHealthEventStatsReportsJournalBacklog`（`GET /api/operations/health-events`） |
| 兼容 | 无 store 的 in-memory 路径：`TestHealthEventInlineFallbackIsExplicitlyBounded` 与既有 monitor/probe/pool 测试通过 |

未覆盖（明确不做）：多进程共用数据库、日志跨迁移前历史回填、消费者在 `Close` 期间的新提交。
