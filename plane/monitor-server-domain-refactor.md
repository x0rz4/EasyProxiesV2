# `monitor/server.go` 完整领域分层改造方案

> 状态：独立设计提案。文中 `internal/httpapi` / `internal/controlplane` 为目标结构，不代表当前已存在。大型代理池路线采用责任分层，先硬化现有边界，再按证据决定是否迁移包；参见 [分层设计](changes/scale-proxy-pool/design.md)。
>
> **PR1 已完成（包内拆分 + 路由拉直）**，见 §11。**PR2 已完成（HTTP 层迁到 `internal/httpapi`，含 `assets` 与 11 处构建路径引用）**，见 §13。本文档 §3.2 的 `internal/controlplane` 与 §6 的"HTTP 生命周期移到 `app.Run`"**尚未开始**，仍是 Phase 1–4 的内容。

## 1. 背景

`internal/monitor/server.go` 曾集中承载全部 HTTP 路由以及认证、设置、节点管理、探测、解锁检测、订阅、分组、GeoIP、SSE、JSON 编解码和数据库访问。代码可以运行，但 HTTP 协议、业务编排、运行时控制与持久化耦合，导致后续增加缓存、限流或读写连接分离时难以限定改动范围。

**当前规模**（PR2 落地后）：

| 项 | `internal/httpapi` | `internal/monitor` |
| --- | --- | --- |
| 非测试代码 | 7,975 行 / 22 个文件 | 3,385 行 / 10 个文件 |
| 测试 | 4,249 行 / 23 个文件 / 94 个 `TestXxx` | 1,520 行 / 7 个文件 / 45 个 `TestXxx` |
| 最大文件 | `server_groups.go` 1,068 行 | `manager.go` |
| import 的 internal 包 | 19 个 | 8 个 |

`httpapi/server.go` **405 行**（PR1 时 510 行），只剩 embed、外部类型、`NewServer`、`SetX`、`Start`/`Shutdown`；路由注册 100 条（`routes.go`，一行一路由），契约用例覆盖含错方法与未知路径。

`internal/monitor` 现在只保留运行态：`manager.go`、`node_registry.go`、`probe_scheduler.go`、`health_event_hub.go`、`health_applier.go`、`speedtest.go`、`concurrency.go`、`context.go`，加上 PR2 新增的两个支撑文件 `ports.go`（消费方接口与领域错误的归位点，见下）与 `tag_providers.go`（`TagUnlockFactProviders` / `TagIPQualityFactProviders` / `NewTagFactRegistry`，被 `app` 与 `httpapi` 共用）。

> `writeJSON` / `decodeJSON` / `writeAPIError` 并入既有的 `response.go`（而不是新建 `server_response.go`）：并行提交已把该文件建成"HTTP 响应管线"的落点（`pathID` / `requireStore` / 三个域状态函数），再开一个名字几乎相同的文件只会让人分不清该往哪加。计划里的 `server_response.go` 因此取消。

> `ports.go` 的由来：`NodeManager` / `GroupRuntimeManager` / `GroupMembershipManager` / `SubscriptionRefresher` 这些**消费方定义**的接口，实现方是 `boxmgr` 与 `subscription`，但 HTTP 层也要引用它们。它们留在 `monitor` 而不是随 HTTP 层搬走，`boxmgr` 才不必 import 一个 HTTP 包来实现运行时接口——这保住了 §1 的分层方向（`httpapi → controlplane → Store / Runtime ports`，今天还是 `httpapi → monitor`）。

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

> **PR2 落地情况**：本节的搬迁目标已全部达成（§13）。实际文件名沿用了 PR1 的 `server_<域>.go` 前缀，没有改叫 `handlers_*.go` / `dto_*.go`——PR1 刚把 `server.go` 拆成这批文件，搬迁时再改一遍名字会让 diff 与 `git log --follow` 都失去意义。`dto` 也仍是与 handler 同文件的既有习惯，未单开文件。Handler 直接调用 `store` / `monitor.Manager` 的收敛属于 §3.2 的 controlplane 议题，由 Phase 2–3 处理。

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

**方法分派已归 ServeMux**（PR1 前的 HEAD 即为 `"GET /api/auth"` 形式）：method pattern 由 `http.ServeMux` 自己返回 405 与 `Allow`，handler 不再检查 method。PR1 删除了 `bindRouteAction` 这层柯里化 `action string` 的**二次分发**——路由表从 `bindRouteAction("get", s.handleSettings)` + handler 内 `switch action` 变成一行一 handler（21 个带 `action` 参数的 handler 全部展开）。`routes_contract_test.go` 的 103 条表驱动用例在拆分前就冻结了公开/鉴权属性、方法、状态码与路径匹配，是这次去分发的安全网。改路由前先扩这张表。

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

**节奏：一个 PR 迁一个领域。** 原计划把 Phase 0–4 放进同一个 PR；PR1 实测表明这不可行（见 §10）。改为按领域推进，每个 PR 独立可编译、可测试、可回滚：

### PR1：包内拆分 + 路由拉直（✅ 已完成，见 §11）

在 `internal/monitor` **包内**按领域拆文件，并删除 `action string` 二次分发。不新建包、不搬 `assets`、不改 HTTP 生命周期。这一步确立的文件边界就是后续搬迁的边界。

### PR2：HTTP 层整体迁到 `internal/httpapi`（✅ 已完成，见 §13）

Server 结构、全部 handler、DTO、`assets` 及 11 处构建路径引用（Dockerfile、`vite.config.ts`、2 个 workflow、README、AGENTS、.gitignore）已全部迁移。

**范围收窄（实测调整）**：原计划把"HTTP 生命周期从 `boxmgr.MonitorServer()` 移到 `app.Run`"也放进 PR2。实测发现该步骤的收益与本 PR 的搬迁目标**无关**——生命周期装配是 §6 的独立议题（"所有依赖就绪后才开放监听"），把它绑进来会让一个纯搬迁 PR 同时改启动顺序，违反 §10 的"单个 PR 过大"控制。故本 PR **只搬不装配**：`boxmgr` 仍持有 `*httpapi.Server` 并调用 `httpapi.NewServer`，HTTP 生命周期迁移留到 Phase 4。

### Phase 0：冻结契约（进行中）

- ✅ 为全部路由补充表驱动契约测试（`routes_contract_test.go`，103 条）。
- ✅ GeoIP JSON 形状（`handlers_contract_test.go`）：`enabled` + `database` 全字段，含"文件不存在"与"文件存在"两条分支、400 早退（未配置路径时**必须不发网络请求**）。
- ✅ 两类此前无形状断言的 SSE 初始帧：`/api/nodes/traffic/stream`（`type/node_count/total_*/node*_speed/sampled_at/nodes`）、`/api/node-check/tasks/{id}/events`（`type:"task"` + task 快照，终态任务立即返回；未知任务 404）。
- ✅ 既有覆盖（无需新增）：`/api/nodes/probe-all`、`/api/nodes/unlock-all` 的 start→progress→complete 序列（`operations_test.go`）；`/api/debug/stream` 的初始帧（`server_timeout_test.go`）；auth/settings/nodes/groups/subscriptions 的响应断言分散在各 `*_test.go`。
- ⬜ 取消语义：SSE 客户端断开后生产协程退出（`test -race` 下验证，本地无 gcc 留给 CI）。

### Phase 1：模型与 ports

- 新建 controlplane 模型、领域错误和窄接口。
- 增加 monitor adapter。
- 将 boxmgr 和 subscription 对 `monitor` API 类型的引用迁到 controlplane。

### Phase 2：领域 Service（PR3+ 按领域逐个 PR）

- 依次迁移 Auth/Session、Settings、Nodes、Subscriptions、Diagnostics/Streams、Groups、GeoIP、Import/Export。
- 每迁移一个领域，将相关测试改为 Service 单元测试并保持 `go test ./...` 通过。
- Groups 保留当前操作锁、并发端口分配和 runtime 失败后的数据库回滚语义。

### Phase 3：HTTP Handler/DTO 收敛

- Handler 改为仅调用 Service；直接 Store/manager/config 调用视为该领域迁移未完成。
- 移动 group subscription，改由 GroupService 返回 body、文件名及 MIME 类型。

### Phase 4：装配与清理

- 将 HTTP 生命周期迁到 `app.Run`，按既定顺序启动和关闭（与 PR2 合并或紧随其后）。
- 移动嵌入资源并更新构建路径。
- 删除旧 HTTP group subscription、旧 Server API 和过渡适配代码。

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

**全局终态**（Phase 2–4 结束后）：

- `internal/monitor` 不再包含 HTTP、JSON、Session 或 Store 访问。
- `internal/httpapi` Handler 中不存在对 Store、boxmgr、subscription manager 或 monitor manager 的直接调用。
- `internal/controlplane` 不 import `internal/httpapi` 或具体 SQLite 实现。
- 路由、JSON/SSE、配置和数据库契约与重构前一致。
- 所有新增 goroutine 都受 context 或 Application 生命周期管理。
- 全量测试、race test、前端构建、跨平台 Go 构建和 Docker 构建通过。

**PR1 判据**（已全部满足）：

1. ✅ `server.go` 从 4,261 行降到 510 行；全包按领域分成 9 个新文件 + 并入 `response.go` + 保留的 6 个域文件。
2. ✅ `grep -rn "bindRouteAction\|action string\|switch action\|, action)" internal/monitor/*.go` 无输出（返修后才真正为空，见上）。
3. ✅ `grep -rn "r\.Pattern" internal/monitor/*.go` 无输出（`group_subscription.go` 的运行时 pattern 判断也拆掉了）。
4. ✅ `routes.go` 每条注册都是一行 `(method, pattern) → 带语义的 handler` 映射。
5. ✅ `go test ./... -count=1` 全绿（27 个包），既有用例无改写（Step 0 新增契约用例、返修的互斥组 CRUD 用例除外）。
6. ✅ `internal/boxmgr` / `internal/subscription` / `internal/app` 的 diff 为空。
7. ✅ `git status` 不含 `internal/monitor/assets/` 的路径变更。
8. ✅ `gofmt` / `go vet` 干净；生产 tags 构建通过。

**PR2 判据**（已全部满足，详见 §13）：

1. ✅ HTTP 层（Server、全部 handler、DTO、路由、响应管线、`assets`）整体位于 `internal/httpapi`；`internal/monitor` 只剩运行态。
2. ✅ `grep -rn "internal/httpapi" internal/monitor/*.go` 无输出（无环）。
3. ✅ 11 处构建路径引用全部指向 `internal/httpapi/assets/`，`.gitignore` 规则同步（产物仍不入库）。
4. ✅ 路由、JSON 形状、SSE 事件、状态码与错误文案零变化——无产品逻辑改动，纯搬迁。
5. ✅ `go build ./...` / `go test ./... -count=1`（27 包）/ `go vet ./...` 全绿；生产 tags 构建通过。
6. ✅ 跨包只新增 6 个产品符号 + 2 个测试口子，每个都有不可替代的理由（§13 表）。
7. ⬜ `go test -race`（本机无 gcc/CGO）与手动冒烟留给 CI。
8. ⬜ HTTP 生命周期**未**迁移到 `app.Run`，按 §7 的范围收窄留到 Phase 4。

## 10. 风险与控制

- **包循环依赖**：controlplane 只依赖 ports；monitor 转换由单向 adapter 完成。
- **JSON 漂移**：迁移前建立 DTO 契约测试，禁止直接序列化领域模型。
- **SSE 泄漏**：所有 stream 测试必须覆盖取消并等待生产协程退出。
- **配置指针过期**：仅 Application 持有并原子更新当前配置引用。
- **启动窗口缺失依赖**：取消运行期 Setter，由 `app.Run` 完整构造后再监听。
- **单个 PR 过大**（PR1 实测得出的新风险）：原方案把 4 个 Phase 塞进一个 PR，涉及 4,261 行的搬迁 + 新建两个包 + 迁移 `assets` 及 11 处构建路径 + HTTP 生命周期改装配，任何回归都难以定位。**改为一个 PR 迁一个领域**，每个 PR 独立绿。
- **`assets` 路径迁移**：`internal/monitor/assets` 的路径被 11 处硬编码（Dockerfile、`vite.config.ts`、2 个 workflow、README、AGENTS、.gitignore）。PR1 因此把 `//go:embed assets/*` 留在 `server.go`（该文件保留为核心文件），一处都不用改；搬迁留到 PR2 与 HTTP 层一起做。
- **拆分中丢失动作名 → 端点静默 404**：Step 0 的 103 条路由契约用例先冻结，拆分后必须全绿。
- **`handleGroupItem` 的 per-group 操作锁被绕过**：前置逻辑（ID 解析 + `lockGroupMutation`）提取为单一 helper，所有 mutation 动作必须经过它。

## 11. PR1 完成记录

**范围**：仅在 `internal/monitor` 包内拆文件 + 去掉 `action string` 二次分发。不新建 `internal/httpapi` / `internal/controlplane`，不搬 `assets`，不改 HTTP 生命周期装配。HTTP 层仍允许直连 `monitor.Manager` / `store`，按领域逐步收敛留到后续 PR。

**Step 0 — 路由契约冻结**（`routes_contract_test.go`，新增 319 行）

把 `routes_test.go` 的 3 个 mux 语义用例扩成 103 条表驱动契约：`classPublic` / `classSession` / `classAPIKey` / `classSubscription` / `classStatic` 五类；`contractServer(t)` 用真 SQLite + `Config{Password:"secret"}` + `sessionTTL: time.Hour`；用响应指纹区分鉴权类别（会话拒绝文案 `未授权，请先登录`、API Key 拒绝文案 `缺少 API Key`），不用反射。用例：`TestRouteTableIsFullyRegistered`、`TestRouteTableAccessClass`、`TestSSERoutesAnswerHEAD`、`TestUnknownRoutesAreNotFound`、`TestWrongMethodIsRejectedByTheMux`。

**Step 1 — 路由去双重分发**

`routes.go` 删除 `bindRouteAction(action string, handler func(w, r, action))`，每条注册直接绑无 action 参数的 handler。21 个带 `action` 参数的 handler 展开为具名方法：`server.go` 15 个、`tag_handlers.go` 3 个、`nodecheck_api.go` 3 个、`nodeview_handlers.go` 1 个。其中 `handleTagItem` 一个函数内 16 个动作名全部展开。`handleSubscriptionItem` 的 CRUD 转发（`handleSubscriptionCRUD`）随拆分自然消失。

`default:` 死分支按计划删除：动作集合与注册集合一一对应后，错方法由 `http.ServeMux` 自己返回 405 + `Allow`，保留兜底 JSON 405 只会与 ServeMux 文案分叉。已确认既有代码里没有用例断言这些被删分支的 `请求方法不允许` 文案；所有 `405` 断言（`routes_test.go:40`、`routes_contract_test.go:311`、`unlock_meta_test.go:46`、`public_api_test.go:144`）打的都是 ServeMux 自己拥有的路由。

> **返修（同一批次内，不重复递增版本）**：首次拆分时 `handleTagMutexGroups` 漏掉了——五个路由包装函数仍把动作字面量传进一个**依然带 `action, rawID string` 参数并在内部 `switch action`** 的辅助函数，`action string` 的 grep 因为签名是两参数形式而没命中。这正是本 PR 要消灭的双重分发。已把五个包装函数各自展开成完整 handler，删除该辅助函数及其两个不可达 `default:` 405 分支，新增共享的 `tagMutexGroups` / `tagMutexGroupForRoute` 两个 helper（后者用 `pathID` 解析并自带 400/404）。行为等价性用语句多重集核对：旧函数 81 条语句，唯一"丢失"的 25 条全部是 5 个 action `case` 标签、2 个 `default:` 405 分支、被删的函数签名，以及 `groupID`→`existing.ID` / `strconv.ParseInt`→`pathID` / `break`→`return` 这几处重命名。同时补了 `TestTagMutexGroupCRUDRoundTrip`：这条 CRUD 路径此前只有"互斥组不存在"一个 404 用例，五个 handler 全部重写后必须端到端钉住 create→list→get→update→delete→404 与重复名 409。

`group_subscription.go` 的 `if r.Pattern == "GET /sub/{groupID}/entry"`（全包唯一的 pattern 运行时判断）拆成 `handleGroupSubscription` / `handleGroupSubscriptionEntry`，共用 `serveGroupSubscription(w, r, entryMode bool)`。

**Step 2 — 按领域拆分 `server.go`**

4,261 → 510 行的纯搬运。为保持"零构建路径变更"，`//go:embed assets/*` 留在 `server.go`。新增 9 个文件：`server_auth.go`、`server_settings.go`、`server_nodes.go`、`server_nodeview.go`、`server_import.go`、`server_subscriptions.go`、`server_groups.go`、`server_geoip.go`、`server_static.go`；响应管线的 `writeJSON` / `decodeJSON` / `writeAPIError` 并入既有的 `response.go`（见 §1 说明）。

搬运中提取的具名 helper（替代原先散落的重复前置逻辑）：

- `authPassword()` — 带锁读 `s.cfg.Password`，替代 3 处裸读。
- `groupRouteID` / `lockGroupMutation` / `requireGroupStore` / `groupForUpdate` — 分组 mutation 的 ID 解析与 per-group 操作锁单一入口。
- `tagForRoute` — tag 路由的 ID 解析 + `existing == nil` 前置检查。
- `savedFilterID` / `subscriptionItemID` / `nodeCheckTaskID` / `configNodeName` — 各域路径参数解析。
- `writeSubscriptionError` / `nodeCheckConfigSource` — 域错误策略与配置来源。

`SetGeoipLookup` 全仓库零调用点，按计划**保留**（删它是行为变更，不在本次范围），标注为"保留的测试/嵌入方接口"。

**内容保真的机械证明**：对拆分前后各取一次 `server.go`（及被改动的 4 个文件）的语句多重集（去掉空行与 `//` 注释后按行计数）做 Counter 比较。首次比对 `EXACT MULTISET MATCH`（3,923 语句）。HEAD 在其间前进到 `5a8979d`（并行提交了 `response.go` 抽取）后重跑，出现 131 处删除 / 249 处新增，逐条分类后确认：非分发脚手架的 40 行全部被 `authPassword()`、`writeSubscriptionError`、`subscriptionItemID`、`configNodeName`、`groupRouteID`+`lockGroupMutation` 等具名抽取解释，**无逻辑丢失**。

**顺带修复**：`public_api_test.go` 的既有 flake。`newAPIKeyTestServer` 构造 `&Server{}` 时没设 `sessionTTL`，`createSession` 用它盖 `ExpiresAt` → 零值 TTL 让会话在签发瞬间即过期，`validateSession` 随时钟 tick 随机拒绝。在 HEAD 的干净 worktree 上复现（6 次 1 失败）确认**先于本次改动存在**，补 `sessionTTL: time.Hour` 后 8 次连续通过。

**验证命令与结果**

```bash
go build ./...                       # OK
go test ./... -count=1               # 27 个包全绿
go vet ./internal/monitor/...        # 干净
gofmt -l internal/monitor            # 干净（CRLF 用 tr -d '\r' 预处理后检查）
go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" \
  -o /tmp/ep ./cmd/easy_proxies      # 通过（GOPROXY=https://goproxy.cn）
```

**未覆盖**：`go test -race` 本地跑不了（需要 `CGO_ENABLED=1`，本机无 gcc），留给 CI。手动冒烟（登录 → 设置 → 标签 → 分组 → 节点探测 SSE）未做。

**已知遗留**：`server_groups.go` 1,067 行，略超原计划的 ~1,010 上限；分组域本就更重，容忍，PR2/PR3 迁 Groups 时一并消化。

## 12. Phase 0 契约补全记录

**范围**：只加测试，不改产品代码。补齐 §7 Phase 0 里 PR2 尚未满足的前置条件——文档自己把"迁移前建立契约测试"列为搬迁前置，且 §10 明确警告过"JSON 漂移"。

**新增** `handlers_contract_test.go`（286 行，6 个用例 + 2 个 helper）：

| 用例 | 钉住的契约 |
| --- | --- |
| `TestGeoipStatusKeepsResponseShape` | `/api/geoip/status` 的 `enabled` + `database` 全字段；文件不存在时 `exists=false`、`size_bytes=0`、`modified_at=""`（不得凭空造值） |
| `TestGeoipStatusReportsExistingFile` | 文件存在时 `exists=true` + 真实 size + RFC3339 UTC（`Z` 结尾）mtime |
| `TestGeoipMutationRejectsMissingPathBeforeTouchingTheNetwork` | download/update 在路径未配置时**先答 400**，文案 `GeoIP 数据库路径未配置`；顺序错了会变慢而不是变错 |
| `TestTrafficStreamKeepsInitialEventShape` | `/api/nodes/traffic/stream` 初始帧的 8 个键与 `type:"traffic"`、`node_count` |
| `TestNodeCheckTaskEventsKeepsInitialEventShape` | `/api/node-check/tasks/{id}/events` 初始帧 `type:"task"` + 快照字段；终态任务发完即返回 |
| `TestNodeCheckTaskEventsUnknownTaskIsNotFound` | 未知任务 404，而不是开一条永不产事件的流 |

**确定性做法**（遵循 §7 测试准则"注入固定状态、避免 sleep"）：

- traffic stream 与 node-check events 都靠"已取消的 context / 已终态的任务"让 handler 在首帧后立即返回，因此 recorder 里恰好一帧，无 sleep、无 ticker 等待。
- GeoIP fixture 用 `os.WriteFile` 造真实文件与缺失文件，`DatabaseStatus` 用 `os.Stat` 判断，不需要 mmdb 内容。

**踩到的坑（值得记录）**：首版 fixture 走 `config.LoadForReload` + `geoip.database_path: ""`，以为能构造"未配置"场景。实际 `applyDefaults` 会给空路径填 `GeoLite2-Country.mmdb`，于是该用例**真的发起了下载**，跑了 124 秒并返回 500。改成直接构造 `&config.Config{}` 并把 `GeoIP.DatabasePath` 设为空，用例从 124s 降到 0.00s。教训：涉及网络路径的"早退"断言，必须确认 fixture 真的落在那条分支上——慢，就是没落上。

**版本**：`2.1.6 → 2.1.7`。

## 13. PR2 完成记录

**范围**：把 HTTP 层从 `internal/monitor` 整体迁到新包 `internal/httpapi`。纯搬迁——不改路由、不改 DTO 字段、不改状态码、不改错误文案、不改装配顺序。

**搬迁结果**

| | 迁到 `internal/httpapi` | 留在 `internal/monitor` |
| --- | --- | --- |
| 产品文件 | 22 个 / 7,975 行 | 10 个 / 3,385 行 |
| 测试文件 | 23 个 / 4,249 行 / 94 用例 | 7 个 / 1,520 行 / 45 用例 |

按 `git mv` 搬迁，保留文件历史。`assets/` 目录同步从 `internal/monitor/assets/` 移到 `internal/httpapi/assets/`，并更新 11 处引用：`Dockerfile`、`frontend/vite.config.ts`、`.github/workflows/build-and-release.yml`（2 处）、`.github/workflows/docker-publish.yml`、`.gitignore`、`.tools/binary-workflow-header.txt`（2 处）、`README.md`、`AGENTS.md`（2 处）。`.gitignore` 的忽略规则一并改到新路径——**产物不入库的前提不能因为搬家而失效**。

**边界与依赖方向**

`internal/monitor` **不 import** `internal/httpapi`（已验证零命中），所以不存在环。反向只有一处：`boxmgr` 现在 import `httpapi`，4 处使用（`monitorServer` 字段类型、`MonitorServer()` 返回类型、局部 `serverToStart`、`httpapi.NewServer` 调用）。`internal/app` **不 import** `httpapi`——它经 `boxMgr.MonitorServer()` 拿到 `*httpapi.Server` 后只调 `Start`/`Shutdown`；app 对 `monitor` 的 3 处引用（`monitor.Config`、`monitor.NewTagFactRegistry`、`monitor.TagUnlockFactProviders`）都是运行时符号，未受影响。

**为跨包而新开的口子（6 个符号 + 2 个文件）**

搬迁必然要把 `httpapi` 用到的包内符号导出。逐一说明为什么是这个口子而不是别的：

| 符号 | 原形态 | 导出理由 |
| --- | --- | --- |
| `monitor.EffectiveProbeConcurrency` | `effectiveProbeConcurrency` | `/api/operations/probe-status` 要按同一口径估算并发与轮次；重算一份必然与运行时漂移 |
| `monitor.NormalizeStartupAvailabilityPolicy` | `normalizeStartupAvailabilityPolicy` | 设置页写入前要按运行时同一规则归一，否则存进去的值与生效值不同 |
| `monitor.ParseProbeTarget` | `parseProbeTarget` | 设置校验必须与运行时解析同一个目标 |
| `monitor.InitialProbeRetryDelay` | `initialProbeRetryDelay` | 出现在 `probe-status` 的耗时估算公式里，是与运行时的共享常量 |
| `monitor.CollectLimited` / `monitor.RunLimited` | `collectLimited` / `runLimited` | 泛型并发原语，HTTP 批处理（探活/解锁/检测）与运行时探测共用同一套上限语义 |
| `monitor.NewTagFactRegistry` / `TagUnlockFactProviders` / `TagIPQualityFactProviders` | 在 `tag_schema.go` | 本来 `httpapi` 的标签路由要用，但 `app` 也要用（装配标签服务）。**没有跟着 HTTP 层搬走**，否则 `app` 就得为了拿事实提供者去 import 一个 HTTP 包。新开 `tag_providers.go` 安放 |

**测试用到的两个新口子**（不是为搬而搬，是测试真正需要、但包边界挡住了）：

- `Manager.PublishHealthResult`（原 `publishHealthResult`）：跨包后测试无法再访问未导出方法。它对外的语义是"通过探针路径使用的同一个 FIFO 投递事件"，`health_boundaries_test.go` 需要它作为**顺序屏障**——发布一个 `SnapshotOnly` 标记，只有在此前发布的事件的回调全部跑完后才会被观察到，从而不靠 sleep 就能断言"诊断不产生健康事件"。文档注释已写明它同时是投递口，不只是排序口（成功事件会真的驱动分组恢复）。
- `Manager.BeginProbeLease` / `EndProbeLease`（原 `beginTagProbe` / `endTagProbe`）：`NewManager` 就启动调度 goroutine，每 tag 的租约是真实并发原语（占住它，调度器就跳过该 tag）。`operations_test.go` 用它把节点钉在初始收敛队列里，才能断言 `probe-status` 的 `queued` 深度。

这两个都是**把已有的真实机制暴露出来**，而不是为测试新造钩子；`monitor` 包内的 `health_events_test.go` 与 `probe_scheduler_test.go` 改用同一组导出名，未新增重复实现。

**ports.go**：`NodeManager` / `GroupRuntimeManager` / `GroupMembershipManager` / `SubscriptionRefresher` / `SubscriptionStatus` / `ManagedNodeConfig` / 哨兵错误（`ErrNodeNotFound` 等）/ `NodeDuplicateError` 从 `server.go` 拆出到此文件，**留在 `monitor`**。这些是消费方定义的接口，实现方是 `boxmgr` / `subscription`；它们若跟着 HTTP 层走，`boxmgr` 就要 import 一个 HTTP 包来实现运行时接口，依赖方向当场反转。

**未搬迁的两处（有意）**

- **HTTP 生命周期**仍由 `boxmgr` 持有（`httpapi.NewServer` / `MonitorServer()`），未移到 `app.Run`。理由见 §7 PR2 的范围收窄说明。
- **`tag_schema.go` 与 `tag_handlers.go` 的划分**：标签的 HTTP 面（handler、DTO、路由分发）随 HTTP 层搬走，成为 `httpapi/tag_handlers.go` + `httpapi/tag_schema.go`；但事实提供者与注册表工厂留在 `monitor/tag_providers.go`。两者同名不同职责，阅读时注意。

**过程中踩到的坑**

1. **迁移期批量加包名前缀的正则伤了自身**：把 `monitor.` 前缀批量加到搬迁文件上时，正则也命中了结构体字面量的字段名（`monitor.Config{monitor.ProbeTarget: ...}`）、本包内新声明的方法接收者（`func (m *serialGroupRuntimeManager) monitor.GroupRuntimeStatus(...)`）以及局部类型声明。这些逐条手工回退。**教训：跨包搬迁的前缀改写必须按"调用位置"而不是"文本出现"来做**，`gofmt`/`go vet` 能抓到它们，但要在一次编译里全部暴露出来（用 `go test -run XXX_NONE` 一次性收齐）而不是一个个修。
2. **`tag_schema.go` 拆分时丢掉文件头**：脚本从 `type tagSchemaLimits` 开始写 `rest`，导致新文件没有 `package` 声明与 import 块，报 `expected 'package', found 'type'`。补回完整头部。
3. **测试 helper 不能跨包 import**：Go 的 `_test.go` 符号对其他包不可见。`health_boundaries_test.go` 拆出一半到 `httpapi` 后，原先复用的 `newPersistentHealthFixture` 拿不到，只能在 `httpapi` 内联一份 `newHealthFixture`；`monitor` 侧保留 5 个不需要 HTTP 层的用例。导出测试夹具会把测试代码打进生产二进制，不做。

**验证命令与结果**

```bash
go build ./...                       # OK
go test ./... -count=1               # 27 个包全绿（含 httpapi 20.2s / monitor 4.9s）
go vet ./...                         # 全模块零输出
gofmt -l <changed>                   # 干净（CRLF 用 tr -d '\r' 预处理后检查，7 个文件曾被 gofmt 判定不合格，均为空行/import 排序/结构体对齐/缺尾换行，已格式化）
go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" \
  -o /tmp/ep ./cmd/easy_proxies      # 通过（GOPROXY=https://goproxy.cn），71 MB
```

`-mod=mod` 会顺手把 `golang.org/x/text` 从 indirect 挪到 direct，构建后已 `git checkout -- go.mod` 还原；`golang.org/x/net` 仍钉在 `v0.50.0`（AGENTS.md §10.10）。

**未覆盖**：`go test -race` 本地跑不了（本机无 gcc/CGO），留给 CI。

**版本**：`2.1.7 → 2.1.8`。

**观察到的既有 flake（非本次引入）**：全量 `go test ./...` 首轮时 `internal/nodetag` 的 `TestQueueSweepsAndSurvivesFailures` 失败一次（`a failed recompute must be reported: [nodetag: 自动标签重算失败: 数据库忙]`）。该包在本次改动中**零修改**（`git status --internal/nodetag/` 为空）；用例用 20ms sweep 间隔并要求至少两条日志，在整仓测试争抢 CPU 时会少记一条。单独 `-count=20` 与重跑全量均通过。属于需要放宽时间假设的既有问题，不在本 PR 修（避免把 flake 修复混进纯搬迁）。
