# 子系统：订阅与解析（L4）

改动 `internal/subscription`、`internal/config`、`internal/subrender` 之前先读这份。

## 刷新写路径必须窄写（护栏 15）

刷新耗时可达分钟级。**失败 / 304 只能调 `RecordSubscriptionFailure` / `RecordSubscriptionNotModified`**，用户编辑只能调 `UpdateSubscriptionSettings`。任何"读整行再写回"都会把期间的用户编辑、或已提交的 `last_success`/`etag` 静默回滚（旧的 `UpdateSubscription` 因此已从接口删除）。

提交阶段的 `created/updated` 由 `CommitSnapshot` 按 `identity_hash` **局部查询**算出，不要再为了统计做全表 `ListNodes`。

## 刷新语义

- 新增 / 启用 / 改配置后在后台异步抓取一次（接口本身立即返回）；创建、改 URL、启用从未同步过的订阅现在都后台抓取（阻塞在刷新槽位、用 manager context）。
- 全量刷新并发抓取（上限 3）、串行提交。
- 同一次刷新未出现于上游的成员累计 `missed_refreshes`，**连续 2 次缺失即删除该成员与仅属于该订阅的节点**，并清理分组里的显式/排除/active 引用。
- **修改订阅 URL 视为换源，立即清空旧成员**——合并两个不相关上游的节点从来不正确。
- 刷新进行中再次触发返回 `controlplane.ErrRefreshInProgress`（HTTP 409）。
- 抓取前在 `resultMu` 下重读请求配置和进程内源版本；URL / Format / UserAgent 改动推进版本（包括 A→B→A）。提交与编辑/删除共用此锁，过期结果返回错误，不改成员、验证器或刷新元数据；网络抓取、运行时重载与 observer 均在锁外。此门禁依赖单进程内 `subscription.Manager` 的生产写入口，不保证外部直接改库的并发语义。
- 304 持久化错误必须返回给调用方；单项刷新状态在运行时协调结束后记录，因此重载失败也进入 `LastError`。

## 自动刷新配置的写入者

`GET/PUT /api/subscription/refresh-settings`（订阅管理页拥有）是 `config.SubscriptionRefresh` 的**唯一写入者**。`/api/settings` 全量 PUT 已不含 `sub_refresh_*`，且 `updateAllSettings` 显式保留 `c.SubscriptionRefresh = old.SubscriptionRefresh`——**不要把这几个字段加回通用设置面**，否则设置页全量 PUT 会覆盖订阅页改动。

生效策略：`DrainTimeout`/`MinAvailableNodes`（boxmgr 拥有）变化才 `TriggerReload`；`Interval`/`Timeout`/`HealthCheckTimeout` 由 subscription manager 热应用、无需重载。

## 配置解析（config）

旧配置订阅与托管订阅都经 `config.ReadSubscriptionBody` 限制实际响应体为 10 MiB，至多多读一个字节用于判定超限；不依赖 Content-Length。超限返回明确错误，不解析或提交截断内容，托管订阅保留已有成员并记录刷新失败。

- `ParseSubscriptionContent`（clash/v2ray/base64）、`ParseImportContent`（行级 issues 必须透出，不能静默吞掉 `ssr://`/非法行）。
- `NodeKey()` 用于内联节点与库内节点的匹配回填。
- **协议白名单唯一真源是 `nodecodec.ParseURI`**，`supportedProxyURISchemes` 必须与它一致。

## 订阅渲染（subrender）

把节点 URI 渲染成 Clash/Mihomo 订阅文档与入口代理（`RenderClash`）。无运行时依赖，供 `/sub/*` 复用。
