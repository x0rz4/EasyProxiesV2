# `monitor/server.go` 完整领域分层改造方案

## 1. 背景

`internal/monitor/server.go` 当前约 3,300 行，集中承载 26 条 HTTP 路由以及认证、设置、节点管理、探测、解锁检测、订阅、分组、GeoIP、SSE、JSON 编解码和数据库访问。代码可以运行，但 HTTP 协议、业务编排、运行时控制与持久化耦合，导致后续增加缓存、限流或读写连接分离时难以限定改动范围。

本次改造采用三层依赖方向：

```text
internal/httpapi → internal/controlplane → Store / Runtime ports
```

HTTP Server 的装配与生命周期迁到 `app.Run`，确保 Store、boxmgr、subscription 和 monitor runtime 全部就绪后再启动管理接口。

## 2. 目标与非目标

### 2.1 目标

- 将 HTTP 路由、DTO 与响应序列化限制在 `internal/httpapi`。
- 将校验、并发控制、事务/回滚和跨组件编排放入 `internal/controlplane`。
- 由 controlplane 定义消费方最小化的 Repository 和 Runtime port，避免 Service 依赖完整 `store.Store` 或具体 manager。
- 集中注册路由，使鉴权边界、公开端点和 SSE 端点一目了然。
- 每个领域可独立测试，并为后续缓存、限流和存储优化提供明确入口。

### 2.2 非目标

- 不修改 HTTP 路径、方法、状态码、JSON 字段、SSE event 或错误文案。
- 不修改 YAML 配置、SQLite schema、代理协议或运行时调度行为。
- 不在本次加入 HTTP timeout、缓存、限流、kTLS 或 SQLite 读写连接分离。
- 不保留旧 `monitor.NewServer` 等内部兼容层；仓库内调用者一次迁完。

## 3. 目标模块结构

### 3.1 `internal/monitor`

只保留节点运行时监控能力，包括 `Manager`、Snapshot、流量采样、健康检查和事件发布。删除 HTTP Server、路由、Session 与 API DTO。现有 outbound pool 对 monitor runtime 的依赖保持不变。

### 3.2 `internal/controlplane`

建立独立领域包，按文件组织以下 Service：

- `auth.go`：密码验证、Session 创建/校验/过期清理。
- `nodes.go`：节点查询、CRUD、批量启停/删除和 reload 结果。
- `diagnostics.go`：单节点操作、探测、测速、解锁检测及结果持久化。
- `streams.go`：探测、解锁、流量和 debug 的带 context 事件流。
- `subscriptions.go`：状态、刷新、CRUD、启停、独占激活和节点查询。
- `settings.go`：设置快照、校验、保存、ApplyPlan 和 reload 编排。
- `groups.go`：分组 CRUD、端口分配、成员筛选、运行时应用及失败回滚。
- `geoip.go`：数据库状态、下载、更新和 lookup 生命周期。
- `import_export.go`：节点导入去重、名称生成和导出模型。

`Application` 聚合这些 Service，并持有线程安全的当前配置引用。它实现 `OnConfigUpdate(*config.Config)`，供 boxmgr reload 成功后更新配置来源。

### 3.3 `internal/httpapi`

- `server.go`：构造、Start、Shutdown，不包含业务逻辑。
- `routes.go`：集中注册当前全部路由，明确公开路由和 `withAuth` 路由。
- `handlers_*.go`：按 auth、nodes、diagnostics、SSE、subscriptions、settings、groups、geoip、import/export 拆分。
- `dto_*.go`：仅存放带 JSON 标签的请求/响应结构和领域模型转换。
- `response.go`：`decodeJSON`、`writeJSON`、`writeAPIError` 及统一错误映射。
- `static.go`：React 静态资源与 SPA fallback。

Handler 只允许执行方法校验、参数解析、Service 调用和 HTTP 输出，不得直接访问 Store、config、boxmgr 或 monitor manager。

前端产物迁到 `internal/httpapi/assets/`，并同步 Dockerfile、两个 GitHub Actions 工作流、README 与 AGENTS 中的输出路径。

## 4. 领域契约

controlplane 在消费端定义窄接口，现有 SQLite Store 可同时满足多个接口：

- `NodeRepository`：节点查找、列举和更新。
- `UnlockRepository`：解锁结果列举与 upsert。
- `GroupRepository`：分组 CRUD、成员状态清理和列表查询。
- `SessionRepository`：Session 创建、读取、删除和过期清理。
- `SettingsRepository`：订阅刷新设置持久化。
- `NodeAdminPort`：配置节点 CRUD、启停和 TriggerReload。
- `GroupRuntimePort`：应用分组 runtime、激活成员和读取状态。
- `SubscriptionPort`：订阅操作及刷新状态。
- `MonitorRuntimePort`：Snapshot、流量、Probe、Release、Dialer 和 debug 订阅。

controlplane 定义不带 JSON 标签的 `ManagedNode`、`SubscriptionStatus`、`GroupRuntimeStatus`、流式事件和领域错误。boxmgr 与 subscription 改用这些类型。monitor adapter 负责把 `monitor.Manager` 的具体类型转换为 port 模型，controlplane 不反向 import monitor。

HTTP 层按领域依赖小接口，而不是依赖一个巨大的 Application 接口，以便使用 fake Service 测试单个 Handler。

## 5. 路由与兼容边界

`routes.go` 原样保留现有路径和匹配语义：

- 公开：`/api/auth`、`/sub/`、`/`。
- 鉴权：settings、nodes、debug、import/export、subscription、groups、geoip 和 reload 的所有 API。
- SSE：`/api/debug/stream`、`/api/nodes/probe-all`、`/api/nodes/unlock-all`、`/api/nodes/traffic/stream`。

继续由 Handler 检查 HTTP method，避免切换到新版 ServeMux method pattern 后改变 404/405、Allow header 或路径匹配行为。路由迁移前先用契约测试固定公开/鉴权属性、方法、状态码和 Content-Type。

流式 Service 接受请求 context，返回只读事件 channel。客户端断开、Server Shutdown 或操作失败后必须停止 ticker、worker、订阅和结果写入协程；HTTP 层负责 SSE header、flush 和既有 event 格式。

## 6. 生命周期与装配

`app.Run` 调整为唯一 composition root：

1. 打开 Store 并加载节点、分组。
2. 创建并启动 boxmgr，取得 monitor runtime。
3. 创建并启动 subscription manager。
4. 创建 monitor adapter 与 controlplane Application，一次性注入所有 Repository/port。
5. 注册 controlplane 和 subscription 为 boxmgr 的配置监听者。
6. 当 management enabled 时创建并启动 httpapi Server。

关闭顺序固定为 HTTP Server → controlplane → subscription → boxmgr → Store，防止请求在依赖已经关闭后继续执行。

boxmgr 删除 `monitorServer` 字段、`MonitorServer()` 和对 Server Setter 的调用；`ensureMonitor` 只创建 monitor runtime。httpapi 构造完成后不提供 `SetStore`、`SetConfig`、`SetNodeManager` 或 `SetSubscriptionRefresher`。

## 7. 分阶段实施顺序

### Phase 0：冻结契约

- 为全部路由补充表驱动契约测试。
- 为代表性 settings、nodes、groups、subscriptions、geoip JSON 建立响应断言。
- 固定四类 SSE 的初始、进度、完成、错误和取消行为。

### Phase 1：模型与 ports

- 新建 controlplane 模型、领域错误和窄接口。
- 增加 monitor adapter。
- 将 boxmgr 和 subscription 对 `monitor` API 类型的引用迁到 controlplane。

### Phase 2：领域 Service

- 依次迁移 Auth/Session、Settings、Nodes、Subscriptions、Diagnostics/Streams、Groups、GeoIP、Import/Export。
- 每迁移一个领域，将相关测试改为 Service 单元测试并保持 `go test ./...` 通过。
- Groups 保留当前操作锁、并发端口分配和 runtime 失败后的数据库回滚语义。

### Phase 3：HTTP 层

- 建立 httpapi Server、集中路由表、通用响应和领域 Handler/DTO。
- Handler 改为仅调用 Service；直接 Store/manager/config 调用视为迁移未完成。
- 移动 group subscription，改由 GroupService 返回 body、文件名及 MIME 类型。

### Phase 4：装配与清理

- 将 HTTP 生命周期迁到 `app.Run`，按既定顺序启动和关闭。
- 移动嵌入资源并更新构建路径。
- 删除 `internal/monitor/server.go`、旧 HTTP group subscription、旧 Server API 和过渡适配代码。
- 全部阶段放在一个 PR 中，但按 Phase 形成独立提交且每个提交可编译、测试通过。

## 8. 测试计划

### 8.1 controlplane

- 节点 CRUD、批量操作、reload 成功/失败和重复节点错误。
- 导入名称冲突、URI identity 去重和部分失败结果。
- settings 校验、ApplyPlan、持久化失败和 reload 错误回传。
- subscription 缺失依赖、CRUD、刷新、启停和错误分类。
- probe/unlock 并发上限、结果持久化、GeoIP 回退和 context 取消。
- group 成员过滤、并发自动端口、运行时失败回滚及订阅 token。
- Session 内存命中、Store 回退、过期删除和 cleanup 停止。

### 8.2 httpapi

- 26 条路由的鉴权、方法、状态码和 Content-Type 矩阵。
- JSON 请求失败、领域错误到 HTTP 状态的映射及既有中文错误文案。
- 四类 SSE 的事件顺序、flush、错误和断连清理。
- 静态文件、SPA fallback 和 group subscription 输出。

### 8.3 集成验收

- management disabled 时不创建 HTTP Server，但 monitor runtime 正常工作。
- 所有依赖就绪后才开放监听；配置 reload 后 Service 读取新配置。
- 关闭时不出现 goroutine 泄漏、关闭后 DB 调用或重复 close。
- 每阶段执行 `go test ./...`；最终执行 `go test -race ./internal/controlplane ./internal/httpapi`、前端 lint/build、带现有 tags 的 Linux amd64/arm64 与 Windows amd64 构建，以及 Docker 镜像构建。

## 9. 验收标准

- `internal/monitor` 不再包含 HTTP、JSON、Session 或 Store 访问。
- `internal/httpapi` Handler 中不存在对 Store、boxmgr、subscription manager 或 monitor manager 的直接调用。
- `internal/controlplane` 不 import `internal/httpapi` 或具体 SQLite 实现。
- 路由、JSON/SSE、配置和数据库契约与重构前一致。
- 所有新增 goroutine 都受 context 或 Application 生命周期管理。
- 全量测试、race test、前端构建、跨平台 Go 构建和 Docker 构建通过。

## 10. 风险与控制

- **包循环依赖**：controlplane 只依赖 ports；monitor 转换由单向 adapter 完成。
- **JSON 漂移**：迁移前建立 DTO 契约测试，禁止直接序列化领域模型。
- **SSE 泄漏**：所有 stream 测试必须覆盖取消并等待生产协程退出。
- **配置指针过期**：仅 Application 持有并原子更新当前配置引用。
- **启动窗口缺失依赖**：取消运行期 Setter，由 `app.Run` 完整构造后再监听。
- **大 PR 回归定位困难**：按 Phase 提交，每阶段保持绿色，不混入任何性能改造。
