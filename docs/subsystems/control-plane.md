# 子系统：控制面（HTTP + 领域服务）

改动 `internal/httpapi`、`internal/controlplane` 之前先读这份。

## 边界

```
httpapi → monitor（读运行时状态） / controlplane（消费接口） / store（直连持久化） / config
controlplane → config / store（不 import monitor / httpapi）
boxmgr / subscription → controlplane（实现其接口）
monitor 从不反向 import httpapi；httpapi 不 import boxmgr
```

跨层消费方接口（`NodeManager` / `GroupRuntimeManager` / `GroupMembershipManager` / `SubscriptionRefresher` 等）定义在 **`internal/controlplane/ports.go`**，由 `boxmgr` / `subscription` 实现、`httpapi` 消费。**不在 `monitor`**（`monitor/ports.go` 已于 `1e05e17` 迁至 controlplane；AGENTS 旧版曾记错，本文件为准）。

## httpapi 文件分布

| 文件 | 承载 |
| --- | --- |
| `server.go` | `Server` 生命周期、`//go:embed assets/*`、节点管理器注入 |
| `routes.go` | 全部路由表 |
| `response.go` | 响应管线：`pathID` / `pathIDOrNotFound` / `requireStore` / 三个域状态函数 / `decodeJSON` |
| `server_auth.go` | 会话认证、`withAPIKey`、API Key 中间件 |
| `server_groups.go` / `server_nodes.go` / `server_subscriptions.go` / `server_settings.go` / `server_geoip.go` / `server_import.go` / `server_static.go` | 各领域 handler |
| `nodecheck*.go` / `nodeview_handlers.go` / `tag_handlers.go` / `tag_schema.go` / `group_preview.go` / `group_subscription.go` / `node_maintenance.go` / `healer_handlers.go` / `probe_applier.go` | 检测 / 视图 / 标签 / 分组 / 自愈 / 探测应用 handler |

## 认证

- `/api/*` 需认证（`management.password` 为空则免密；Cookie `session_token` 或 `Authorization: Bearer`，会话 24h 存 SQLite）。
- `/sub/{groupID}` 公开，但用分组 `SubscriptionToken` 做常量时间比较。
- 免登录公共 API（`POST /api/public/subscriptions`、`/api/public/import`）由 `withAPIKey` 保护，接受 `X-API-Key`/`Bearer`/`?api_key=`，**故意不接受会话 Cookie**（面向脚本）。新公共接口一律复用现有 handler，不另写实现。

## 错误→状态码是三个策略（护栏 12）

`response.go`：`nodeErrorStatus`（类型驱动，`respondNodeError` 额外带 `existing_node_id`）、`subscriptionErrorStatus`（消息驱动，未识别→500）、`tagErrorStatus`（消息驱动，未识别→400）。**故意不一致**：同是"节点不存在"，tag 域 400、订阅域 404，`tag_handlers_test.go` 钉住了这个 400。调用方必须显式点名用哪个策略，不存在统一分发器。`pathID`（400）与 `pathIDOrNotFound`（公开 `/sub/*` 用 404，避免探测 ID 形态）不可互换。

## controlplane 领域服务

综合检测任务的状态写入按任务串行，运行进度至多每秒落库一次，状态转换与终态绕过节流。每次写入最多 5 秒，运行中写入随任务取消；取消后的终态使用独立有界预算。失败进入日志、SSE warning 与任务 `persistence_error` 字段；初始任务无法保存时创建失败，不启动后台检测。

- `Auth`：会话、登录限流、API Key 凭据。
- `Settings`：配置读写、校验、持久化、`ApplyPlan` —— **所有配置写入者的唯一序列化点**。
- `ports.go`：跨层接口与领域错误（`ErrNodeNotFound`、`NodeDuplicateError`、`ErrRefreshInProgress` 等）。

**不要**为"减少 handler 样板"引入泛型 `handle[Req,Resp]`（护栏 13）：14 个解码请求体的 handler 各带 3–19 个错误分支，包装只省最外层 decode/serialize，分支一个不少，且会丢掉 `respondNodeError` 的 `existing_node_id` 造成行为变化。

综合检测 SSE 在同一任务锁内订阅并复制初始状态；已结束任务直接发送 done。终态事件在满队列时替换一条进度，保证可达。连接每 15 秒心跳，每次写入/flush 设置 10 秒期限，写失败、请求取消或 Server 关闭后解除订阅。

Server 关闭幂等发送停止信号，HTTP 优雅关闭超时则关闭连接；等待综合检测任务完成终态写入后返回。自动随运行上下文关闭时使用十秒预算。

GET /api/nodes、/api/nodes/config、/api/nodes/unlock-results、/api/node-check/results 支持 node_ids 正整数逗号列表（最多 200 个）。参数缺席保持全量兼容；显式空列表返回空，非法值返回 400。配置详情复用 controlplane.NodeDetailReader 与 store.ListManagedNodesByIDs，保持启用订阅可见性；认证权限与原接口一致。

Auth 按 API Key ID 合并使用时间，仅在首次使用及之后每分钟写入；失败五秒后重试并保留最新观测。惰性启动一个消费者，单次数据库调用三秒上限，Server 关闭时用剩余预算写完待办。最多保留 1024 个 Key 的元数据状态，溢出只跳过新 Key 的使用元数据并限频记录累计丢弃数，认证不受影响；闲置/删除 Key 会回收。重复绑定同一 store 保留待办，换库清空旧 ID 状态，SQLite 也拒绝旧时间覆盖新时间。
