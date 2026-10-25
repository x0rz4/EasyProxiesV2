# 子系统：持久化与 JSON（L5）

改动 `internal/store`、`internal/jsonx` 之前先读这份。

## store 唯一持久化层

`Store` 接口是数据库访问的唯一入口（当前 **101** 个方法），涵盖 nodes / subscriptions / stats / sessions / unlock / detection / group pools / tags。`Open()` 内部跑 `Migrate`。生产配置为**单写连接 + WAL + 四读连接**（规避进程内写锁争用）。

页缓存默认每连接 4096 KiB，五个连接合计建议预算 20 MiB，按需增长而非预分配。启动环境变量 `EASY_PROXIES_SQLITE_CACHE_KIB` 可设置每连接预算（1–65536 KiB）；非法值导致启动明确失败，不支持热修改。SQLite 页缓存不是整个进程的硬内存上限。

## migration 只追加（AGENTS §8）

`internal/store/migrations.go` 的列表按 `Version` 递增、**只允许在末尾新增**。每个 migration 独立事务，`schema_migrations` 记录已应用版本。当前最新 **v25**（v23 仅为 `nodes.name` 建普通索引，名称**故意不唯一**；v24 新建 `health_events` 与 `health_event_cursors`；v25 给 `group_node_states` 加 `revision`，见下）。**新列必须带 `NOT NULL DEFAULT`**，保证升级行为不变。

表结构变更后同步 `sqlite.go` 的列常量 / `scan*` / `INSERT` / `UPDATE`；改 `store.GroupPool` 还要同步 `config.GroupPoolConfig` + `boxmgr` 的双向转换与相等性判断（否则热重载判定不到变化）。

## 身份与探测收据

- `node_identity.go`：节点身份 / revision。NodeID、身份 revision、runtime tag generation 是三个不同概念，不能互相替代（spec IDENTITY-001）。显示名不作为身份。
- `landing_probe.go` / `sqlite_probe.go`：落地探测收据。
- `probe_id.go`：`NewProbeID(prefix)` = `wall_unixNano + 进程内单调序号`。墙钟 `UnixNano` 在某些平台粒度粗（Windows ≈1ms、legacy 中断 ≈15.6ms），同 tick 紧邻的两次同节点探测会拿到相同 ProbeID 被下游判为 duplicate 丢弃。单调序号消除进程内碰撞；**重试路径必须显式复用同一原 ID**（幂等契约靠这个，绝不能二次调 `NewProbeID`）。
- `sqlite_location.go`：落地地区证据的应用（配合 `nodeloc`）。
- `sqlite_tags.go`：标签 API；`nodes.tags` 只是 `node_tags` 的只读投影。

## 健康事件日志（v24）

- `health_events`：被接受的健康测量按 `seq`（`AUTOINCREMENT`，清理后**不复用**编号，否则游标会指到另一条事件）追加，字段含 NodeID、身份 token/revision、tag、ProbeID、成功/延迟/错误、`checked_at`、`recorded_at`。写入只发生在 `probe.Applier.Apply` 健康分支的**同一事务**内。
- `health_event_cursors`：持久消费者的投递进度（`consumer` 为主键）；`PutHealthEventCursor` 只前进不后退，重试旧批次不会回退游标。游标 MUST 与该批事件产出的 `group_node_states` 同一事务提交（`AdvanceHealthCursorWithState`），否则崩溃可能留下"游标说已应用、状态却没落盘"的组合，踢出记录随之丢失。
- 容量 `HealthEventJournalLimit`=8192；满时 `AppendHealthEvent` 返回 `ErrHealthEventJournalFull`，测量整体回滚（显式背压，不静默丢旧事实）。
- `PruneHealthEvents(olderThan, activeConsumers)` 只删"超过保留窗口**且** `seq <= MIN(活跃游标)`"的行。`activeConsumers` 是**当前注册**的消费者键，不是整张 `health_event_cursors` 表：已删除/禁用分组留下的游标行必须被忽略，否则会永久钉住清理（详见 spec HEALTH-005）。没有注册消费者时按年龄清理。
- 时间列用固定宽度布局 `2006-01-02T15:04:05.000000000Z07:00`：`RFC3339Nano` 会去掉末尾零，使 `…:00Z` 在 SQL 字符串比较里排到 `…:00.1Z` 之后，按年龄清理就会删错行。

## 分组节点状态（v25）

`group_node_states` 有**两个写者**：健康投递的同步事务，以及 `internal/app` 的异步合并持久化器（当前节点与非健康转换）。两者写下的都是"转换发生时的快照"，落库顺序不保证与转换顺序一致。

- `revision`（v25 新增，`NOT NULL DEFAULT 0`）由 group runtime 在持有运行时锁时分配，全局单调；`UpsertGroupNodeState` 只在 `revision = 0 OR revision >= 已存值` 时生效。`0` 表示无版本，兼容既有调用方。
- `revision` MUST 随 `store → config → runtime` 往返（`GroupNodeStateConfig.Revision`），启动时用磁盘上的最大值抬升全局时钟；否则重启后时钟归零，所有新写入都会被当成过期写入丢掉。
- 外键失败（分组或节点已被删除）在 `AdvanceHealthCursorWithState` 里是**丢弃该行并继续**，不是可重试错误——否则一个已删除目标会永久卡住该消费者。

## 窄写规则（呼应订阅子系统）

失败的订阅刷新走 `RecordSubscriptionFailure`、304 走 `RecordSubscriptionNotModified`、用户编辑走 `UpdateSubscriptionSettings`，而不是"读整行写回"。完整语义见 [subscription.md](subscription.md)。

## jsonx

JSON 编解码统一出口（Sonic + `MarshalCanonical`）。`Marshal`/`Unmarshal` 默认 Sonic；需要**确定性字节序**（做哈希或比较）时用 `MarshalCanonical`。注意 Sonic 的 `ConfigDefault` 不排序 map 键——别拿 `Marshal` 输出直接做哈希/比较。
