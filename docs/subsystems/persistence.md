# 子系统：持久化与 JSON（L5）

改动 `internal/store`、`internal/jsonx` 之前先读这份。

## store 唯一持久化层

`Store` 接口是数据库访问的唯一入口（当前 **101** 个方法），涵盖 nodes / subscriptions / stats / sessions / unlock / detection / group pools / tags。`Open()` 内部跑 `Migrate`。生产配置为**单写连接 + WAL + 四读连接**（规避进程内写锁争用）。

## migration 只追加（AGENTS §8）

`internal/store/migrations.go` 的列表按 `Version` 递增、**只允许在末尾新增**。每个 migration 独立事务，`schema_migrations` 记录已应用版本。当前最新 **v23**（v23 仅为 `nodes.name` 建普通索引；名称**故意不唯一**）。**新列必须带 `NOT NULL DEFAULT`**，保证升级行为不变。

表结构变更后同步 `sqlite.go` 的列常量 / `scan*` / `INSERT` / `UPDATE`；改 `store.GroupPool` 还要同步 `config.GroupPoolConfig` + `boxmgr` 的双向转换与相等性判断（否则热重载判定不到变化）。

## 身份与探测收据

- `node_identity.go`：节点身份 / revision。NodeID、身份 revision、runtime tag generation 是三个不同概念，不能互相替代（spec IDENTITY-001）。显示名不作为身份。
- `landing_probe.go` / `sqlite_probe.go`：落地探测收据。
- `probe_id.go`：`NewProbeID(prefix)` = `wall_unixNano + 进程内单调序号`。墙钟 `UnixNano` 在某些平台粒度粗（Windows ≈1ms、legacy 中断 ≈15.6ms），同 tick 紧邻的两次同节点探测会拿到相同 ProbeID 被下游判为 duplicate 丢弃。单调序号消除进程内碰撞；**重试路径必须显式复用同一原 ID**（幂等契约靠这个，绝不能二次调 `NewProbeID`）。
- `sqlite_location.go`：落地地区证据的应用（配合 `nodeloc`）。
- `sqlite_tags.go`：标签 API；`nodes.tags` 只是 `node_tags` 的只读投影。

## 窄写规则（呼应订阅子系统）

失败的订阅刷新走 `RecordSubscriptionFailure`、304 走 `RecordSubscriptionNotModified`、用户编辑走 `UpdateSubscriptionSettings`，而不是"读整行写回"。完整语义见 [subscription.md](subscription.md)。

## jsonx

JSON 编解码统一出口（Sonic + `MarshalCanonical`）。`Marshal`/`Unmarshal` 默认 Sonic；需要**确定性字节序**（做哈希或比较）时用 `MarshalCanonical`。注意 Sonic 的 `ConfigDefault` 不排序 map 键——别拿 `Marshal` 输出直接做哈希/比较。
