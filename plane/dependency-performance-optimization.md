# 依赖库与基础设施性能优化方案

> 状态：设计提案  
> 范围：Go 依赖、JSON、HTTP、日志、sing-box 集成、SQLite 与依赖治理  
> 原则：先测量、再替换；优先优化真实热路径，不为了 benchmark RPS 迁移整个技术栈

## 1. 结论摘要

对当前仓库核对后，最初建议中的多数“迁移项”实际上已经完成或不适用：

| 建议 | 当前实际情况 | 结论 |
| --- | --- | --- |
| `encoding/json` → Sonic | 已直接依赖 `github.com/bytedance/sonic v1.15.2`，生产代码通过 `internal/jsonx` 使用 | 保留 Sonic，补齐覆盖率、兼容性和 first-hit 基线，不再重复替换 |
| Sonic → `goccy/go-json` | 当前无兼容性阻塞，现有 Sonic benchmark 已显著更快 | 不引入第二套 JSON 库；只有跨平台/兼容问题才做 A/B 候选 |
| 精简 Gin/Echo 中间件 | 项目没有使用 Gin/Echo/Fiber，管理 API 已基于 `net/http.ServeMux` | 不迁移框架；优化 handler、缓存、超时和高频端点 |
| Gin Logger → Zap | 没有全局请求日志中间件；Zap 只是 sing-box 的间接依赖 | 不为“结构化”盲目切日志库；先消除每代理连接 Info 日志 |
| CLI sing-box → Library | 已直接依赖 `sing-box v1.13.19`，通过 `box.New()` 在进程内启动 | 已完成；下一步是增量生命周期和按需 registry |
| GORM prepared statement | 项目没有 GORM，已使用原生 `database/sql` + `modernc.org/sqlite v1.57.0` | 不引入 GORM/sqlx；按查询热点决定 prepared statement、索引和缓存 |

当前优先级最高的真实问题是：

1. 所有 JSON API 都强制 `SetIndent()`，浪费 CPU、响应字节和带宽。
2. `/sub/{group}` 每次请求都会全量读取节点、复制 monitor/group snapshot、构建 map 并重新渲染订阅。
3. `poolOutbound` 每次 TCP/UDP 新连接都写 Info 日志，高并发下字符串拼接、锁和 I/O 可能远高于 JSON 成本。
4. SQLite 已开启 WAL，但 `MaxOpenConns(1)` 把读写全部串行化；注释“SQLite only supports 1 writer”不等于只能有一个数据库连接。
5. `http.Server` 没有显式 header/idle/header-size 限制；通用 JSON decode helper 也没有统一 body 上限。
6. `include.*Registry()` 注册 sing-box 全量组件，当前依赖图约 571 个 Go package、349 个 module；这更影响构建、二进制、冷启动和供应链面，而非请求 RPS。

## 2. 当前基线

### 2.1 版本与架构

```text
Go             1.25.0 / toolchain 1.25.5
JSON           github.com/bytedance/sonic v1.15.2
HTTP           net/http.ServeMux
Proxy core     github.com/sagernet/sing-box v1.13.19（Library）
Database       database/sql + modernc.org/sqlite v1.57.0
SQLite PRAGMA  WAL + busy_timeout(5000) + synchronous(NORMAL)
               cache_size(-64000) + foreign_keys(ON)
DB pool        MaxOpenConns(1), MaxIdleConns(2)
```

生产代码已经集中通过：

```text
internal/jsonx/json.go
```

调用 Sonic `ConfigDefault`；需要稳定 map 顺序的节点身份摘要使用 `ConfigStd` 的 `MarshalCanonical()`。这是正确的边界，后续不得在业务包中散落新的第三方 JSON 直接 import。

### 2.2 JSON 本机 benchmark

命令：

```bash
go test ./internal/jsonx -run '^$' \
  -bench 'Benchmark(Marshal|Unmarshal)Nodes' -benchmem -count=3
```

环境：Windows/amd64，Intel i7-12700KF，256 个节点 payload。以下为三次结果的代表值，不能代替生产机器基线：

| 场景 | encoding/json | Sonic | 观察 |
| --- | ---: | ---: | --- |
| Marshal | 约 128 µs | 约 39 µs | Sonic 约快 3.3x |
| Marshal B/op | 约 41 KB | 约 43 KB | Sonic 略高，不应宣称编码内存全面下降 |
| Marshal allocs | 2 | 3 | Sonic 略高 |
| Unmarshal | 约 670 µs | 约 150 µs | Sonic 约快 4.5x |
| Unmarshal B/op | 约 102 KB | 约 115 KB | Sonic 略高 |
| Unmarshal allocs | 1808 | 约 523 | 分配次数下降约 71% |

结论：Sonic 已明显降低 CPU 和解码分配次数，但当前 payload 下并未降低总分配字节。下一步应优化调用方式和热路径，而不是继续更换 JSON 库。

## 3. 性能目标与门槛

所有优化合并前必须给出 before/after；没有可重复收益的依赖变更不进入主分支。

### 3.1 指标

- CPU：`ns/op`、进程 CPU%、热点函数占比。
- 内存：`B/op`、`allocs/op`、heap in-use、GC 次数与 pause。
- HTTP：RPS、p50/p95/p99、响应字节、错误率。
- 订阅端点：缓存命中率、渲染次数、数据库查询次数。
- SQLite：query latency、等待时间、busy/locked 次数、连接池 stats。
- sing-box：reload 耗时、冷启动耗时、二进制体积、RSS。
- 日志：每秒日志行数、写入字节、丢弃/采样数。

### 3.2 推荐验收门槛

- 纯微优化：目标场景 CPU 或 allocations 至少改善 10%，且无兼容回归。
- 新依赖或框架替换：端到端 p95 至少改善 20%，并证明维护成本可接受。
- 订阅缓存：稳定 generation 下命中请求不得执行节点全表查询或完整渲染。
- 日志降噪：默认 info 级别下不得为每个代理连接生成一行路由日志。
- 数据库连接调整：并发读 p95 改善且写入 busy/locked 错误不增加。

## 4. P0：建立可重复性能基线

### 4.1 Benchmark 套件

新增或扩展：

```text
internal/jsonx/json_test.go
  BenchmarkWriteJSONCompact
  BenchmarkWriteJSONIndented
  BenchmarkSSEEvent

internal/monitor/group_subscription_test.go
  BenchmarkGroupSubscriptionEntry
  BenchmarkGroupSubscriptionMembers100
  BenchmarkGroupSubscriptionMembers1000

internal/store/*_test.go
  BenchmarkListNodes
  BenchmarkGetGroupPool
  BenchmarkConcurrentGroupSubscriptionReads
  BenchmarkSubscriptionSnapshotWrite

internal/builder/builder_test.go
  BenchmarkBuild100Nodes
  BenchmarkBuild1000Nodes
```

benchmark 使用固定随机种子和预生成输入，准备数据不得计入被测循环。

### 4.2 Profile

增加仅绑定 loopback、仅在显式配置开启时注册的 pprof server：

```text
127.0.0.1:<debug-port>/debug/pprof/
```

禁止把 pprof 挂到公开管理端口或无认证公网地址。至少采集：

```bash
go tool pprof -http=:0 http://127.0.0.1:<port>/debug/pprof/profile?seconds=30
go tool pprof -http=:0 http://127.0.0.1:<port>/debug/pprof/heap
go tool pprof -http=:0 http://127.0.0.1:<port>/debug/pprof/allocs
go tool pprof -http=:0 http://127.0.0.1:<port>/debug/pprof/mutex
```

场景至少覆盖：WebUI 轮询、100/1000 节点订阅生成、批量订阅刷新、探测/解锁并发、reload 和持续代理连接。

### 4.3 自动化防回退

- CI 保存 benchmark 文本或 benchstat 基线。
- PR 不用硬性阻断所有轻微波动，但新增依赖/核心优化必须附 benchstat。
- Windows amd64、Linux amd64、Linux arm64 至少运行 `jsonx` 兼容测试和构建。
- 每次 sing-box/Sonic 升级运行全量测试、race、订阅 golden tests。

## 5. JSON 优化

### 5.1 保留 `internal/jsonx`

继续使用单一门面：

```go
jsonx.Marshal
jsonx.Unmarshal
jsonx.NewEncoder
jsonx.NewDecoder
jsonx.MarshalCanonical
```

业务代码不得直接 import Sonic 或 goccy。这样可以在平台回退、兼容问题或未来 Go 标准 JSON 实现成熟时，仅修改一个包。

### 5.2 取消生产 API 的全局 pretty-print

当前 `writeJSON()` 每次都：

```go
enc.SetIndent("", "  ")
```

改为默认 compact JSON；只允许在开发模式或显式 `?pretty=1` 且已认证时缩进。收益需要通过真实 Nodes/Groups/Unlock payload benchmark 和响应大小验证。

预期影响：

- 减少编码工作。
- 减少响应体和前端下载/解析字节。
- SSE 事件保持单行，避免不必要的格式处理。

### 5.3 用具体 response struct 代替高频 `map[string]any`

优先改造：

- `/api/nodes`
- `/api/groups`
- `/api/nodes/traffic/stream`
- probe/unlock SSE 事件

具体 struct 可减少 interface/map 构造、运行时类型判断和字段拼写漂移。低频、极小错误响应可以继续使用 map，不做机械式全仓替换。

### 5.4 Sonic 兼容与 JIT

补充 differential tests，同一输入分别用 `encoding/json` 和 `jsonx` 验证语义：

- HTML escaping。
- 非法 UTF-8。
- `omitempty`、嵌入字段、自定义 Marshaler。
- `DisallowUnknownFields()` 与单 JSON 文档 EOF 检查。
- 数值边界、NaN/Inf 错误。
- canonical map key 稳定性。

Sonic 使用 JIT/SIMD。对于大 schema 或首请求敏感端点，先测量 cold-first-call；只有出现明显尖峰时才在启动阶段对少数顶层响应调用 Pretouch。不要对所有类型无差别 Pretouch，以免增加启动时间和 RSS。

### 5.5 `goccy/go-json` 的定位

不立即引入。仅在以下条件之一成立时建立独立实验分支：

- Sonic 在目标 CPU/OS 上只能回退标准库，且性能不达标。
- Go/toolchain 升级产生长期兼容阻塞。
- differential/fuzz 测试发现无法接受的行为差异。
- 实际 payload A/B 显示 goccy 在 CPU、B/op、冷启动或二进制总体更优。

实验必须通过相同 API、golden、fuzz 和三种目标架构 benchmark，不能引用库 README 的通用数字作为项目结论。

## 6. HTTP 层优化

### 6.1 保持 `net/http`

项目已经是标准库 ServeMux，且包含 SSE、静态文件、认证、长连接和 context cancel。迁移 Fiber/fasthttp 会带来：

- handler/middleware 全量重写。
- SSE、标准 `http.Handler` 生态和 context 行为适配成本。
- 更高的兼容测试与维护成本。

在 profile 证明 router dispatch 是主要 CPU 热点前，不引入 Fiber。当前路由规模下，业务查询、渲染和日志更可能是瓶颈。

Chi 已作为 sing-box 间接依赖出现，但 EasyProxies 没有直接使用它。若未来需要 path parameter、middleware composition，可从可维护性角度评估；不要把迁移 Chi 当作性能优化。

### 6.2 Server 限制与 SSE 例外

当前 server 只有 Addr/Handler。建议增加：

```go
http.Server{
    Addr:              cfg.Listen,
    Handler:           handler,
    ReadHeaderTimeout: 5 * time.Second,
    IdleTimeout:       60 * time.Second,
    MaxHeaderBytes:    64 << 10,
}
```

`WriteTimeout` 不能直接设置成普通短超时，因为 traffic/debug/probe/unlock SSE 是长响应。选择之一：

1. 保持全局 `WriteTimeout=0`，普通 handler 使用 `http.ResponseController` 或 context deadline。
2. 将长流端点放入明确例外的 listener/server。

任何 timeout 数值都要覆盖反向代理、慢客户端和 SSE reconnect 集成测试。

### 6.3 统一请求体上限

订阅抓取已经限制为 10 MiB，但通用 `decodeJSON()` 直接读取 body。改成接收 `ResponseWriter` 和 endpoint limit：

```go
decodeJSON(w, r, dst, maxBytes)
```

内部使用 `http.MaxBytesReader`、`DisallowUnknownFields()`、只允许一个 JSON 文档，并区分 400 与 413。建议：

- 普通 CRUD：256 KiB。
- batch 操作：1 MiB。
- import：保持独立的 10 MiB 上限。

这首先是资源保护，也可避免恶意/误操作触发大对象分配。

### 6.4 Group subscription 缓存

`/sub/{group}` 是最值得优化的 HTTP 端点。当前每次请求都会：

```text
GetGroupPool
→ ListNodes（全表）
→ Manager.Snapshot（全量）
→ GroupRuntimeSnapshots（全量）
→ 构建多个 map/slice
→ 逐节点解析 Clash proxy
→ YAML/base64/URI 渲染
```

引入不可变 `SubscriptionSnapshot`：

```go
type SubscriptionSnapshot struct {
    Generation uint64
    GroupID    int64
    Bodies     map[RenderKey]RenderedBody
    Upload     int64
    Download   int64
    ETag       string
}

type RenderKey struct {
    Format    string
    Mode      string
    AliveOnly bool
    Host      string
}
```

成员、group 配置、外部 host 或节点 URI 变化时生成新 generation 并失效 body；普通 GET 只做 token 校验、snapshot Load、选 body、写 header/body。

注意：

- `Cache-Control: no-store` 可以继续告诉客户端不要持久缓存，不妨碍服务端内存缓存生成结果。
- `Subscription-Userinfo` 的实时流量可单独读取，不必使 body cache 每秒失效。
- 支持 `ETag`/`If-None-Match`，内容未变返回 304；ETag 不得包含 token。
- 缓存必须按外部 host 和 render mode 区分，不能跨 group/token 泄露内容。
- 限制缓存总字节和 generation 数，只保留当前 generation。

### 6.5 压缩

对较大的 JSON/YAML 响应，在反向代理未负责压缩时评估 gzip/zstd。只对超过阈值的响应压缩，并将 CPU 与带宽收益一起 benchmark。已经是 base64 的订阅通常压缩收益有限，不默认重复压缩。

## 7. 日志优化

### 7.1 先移除真正的热日志

`poolOutbound.DialContext()` 和 `ListenPacket()` 当前每个新连接执行：

```text
Info("→ destination ⇒ member [network]")
```

这是比管理 API 请求日志更高频的路径。改为：

- 默认 info 不记录每连接路由。
- debug/trace 时才记录明细。
- 生产调试需要时支持按节点、目标或比例采样。
- 失败、blacklist、eviction 保留 Warn，但增加重复错误抑制/窗口聚合。
- WebUI debug timeline 使用已有内存事件，不依赖打印每个成功连接。

必须 benchmark 日志关闭、同步写入和采样三种模式下的拨号路径。

### 7.2 是否引入 Zap

Zap 已是 sing-box 的间接依赖，但 EasyProxies 直接 import 后仍应在 `go.mod` 声明直接依赖并维护自己的日志抽象。

只有 profile 显示剩余结构化日志编码/锁/I/O 是热点时才迁移。否则：

- 继续使用现有 Logger 接口。
- 避免在热路径提前 `fmt.Sprintf`。
- 使用级别检查，参数延迟格式化。
- 将访问日志、运行日志与逐连接 trace 分开。

换 Zap 不能解决日志量本身的问题；先降日志基数的收益更大。

## 8. sing-box 集成优化

### 8.1 保持 Library 集成

当前 `box.New()`、`Box.Start()` 方式已经消除外部 CLI 进程、配置文件落盘和 IPC。不得退回 `os/exec`。

性能重点不是再次更换集成方式，而是：

- 按节点增量 Create/Remove。
- pool immutable snapshot。
- outbound drain。
- 避免整 box reload。

具体方案见 `plane/incremental-hot-reload.md`。

### 8.2 按需 registry

当前 `include.InboundRegistry()`、`include.OutboundRegistry()`、Endpoint、DNS、Service registry 会带入 sing-box 的完整实现和大量间接依赖。

建立实验分支，仅注册 EasyProxies 实际支持的组件：

- inbound：http、socks、mixed 所需实现。
- outbound：http、socks、vless、shadowsocks、trojan、vmess、anytls，以及代码真正支持的其他协议。
- DNS transport：builder 当前生成配置实际需要的集合。
- EasyProxies 自定义 pool。

实施前必须做 feature matrix，防止遗漏某个 URI 协议、TLS transport、endpoint 或 DNS 依赖。比较：

- `go list -deps` package 数。
- `go list -m all` module 数。
- clean build 时间。
- 可执行文件大小。
- 冷启动时间和空载 RSS。
- 所有协议集成测试。

这是构建与供应链优化，除非 profile 证明 registry 初始化占比显著，不承诺提高 steady-state RPS。

### 8.3 避免替换 sing-box 间接依赖

Chi、Zap 等已经由 sing-box 间接引入。不要通过 `replace` 强制升级/降级这些内部依赖来追求版本新或 benchmark 数字；必须跟随 sing-box 的兼容矩阵升级。

## 9. SQLite 与缓存优化

### 9.1 保持 `database/sql`

现有代码已经是原生 SQL。引入 GORM 或 sqlx 不会自动提高性能，反而增加抽象和依赖。继续使用 store interface + `database/sql`。

### 9.2 连接池实验

当前：

```go
db.SetMaxOpenConns(1)
db.SetMaxIdleConns(2)
```

`MaxIdleConns` 高于 MaxOpen 没有实际意义；更重要的是单连接让 WAL 下的读取也和写入串行。

在相同数据集上比较：

```text
MaxOpenConns = 1 / 2 / 4
MaxIdleConns = 相同或更小
```

场景：高频 `/sub` 读 + subscription refresh 写 + stats flush。记录 p95、`DB.Stats().WaitCount/WaitDuration` 和 SQLite busy 错误。不要直接把连接数调到 CPU 数；SQLite 仍只有一个 writer，过多连接会增加竞争。

所有 PRAGMA 必须确认应用到每个新连接；当前 DSN `_pragma` 方式应保留跨连接测试。

### 9.3 Prepared statements

当前批量事务已经在事务内 prepare 重复 INSERT/UPDATE，这是正确使用方式。进一步只处理 profile 中的高频、固定 SQL，例如：

- session/token lookup。
- group lookup。
- node identity lookup。
- stats upsert。

如果在 `*sql.DB` 生命周期缓存 stmt：

- store Close 时逐个 Close。
- migration 前后不要复用 schema 已变化的 stmt。
- transaction 内使用 `tx.StmtContext()` 或在 tx 内 prepare，避免操作逃离事务。
- 先比较 driver 自动 prepare/SQLite VM cache 下的实际收益。

不建立“所有 SQL 全部预编译”的机械式缓存。

### 9.4 查询与索引

对高频 API 使用 `EXPLAIN QUERY PLAN`，优先消除：

- `/sub/{group}` 为单个 group 调用 `ListNodes()` 全表扫描。
- group list 对每个 group 单独读取 node states 的 N+1。
- 为得到少量字段加载完整 URI、凭据和 JSON 列。

建议增加针对用途的窄查询：

```text
ListEffectiveGroupSubscriptionNodes(groupID)
ListGroupPoolsWithStates()
ListNodeSummaries(filter)
```

索引必须由 query plan 和数据规模证明，避免给写多表增加无效索引维护。

### 9.5 读多写少缓存

优先缓存稳定派生结果，而不是缓存任意 SQL 行：

- group definition snapshot。
- group effective member IDs。
- NodeID → subscription render input。
- persisted unlock facts。
- rules/group membership generation。

缓存用不可变 snapshot + atomic pointer；写事务成功后发布新 generation。不要做 TTL-only 缓存，因为配置更新后短时间返回旧节点会造成错误路由或商品质量不一致。

## 10. 依赖治理

### 10.1 固定流程

每次依赖升级执行：

```bash
go mod tidy
go mod verify
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
```

并完成 Linux amd64/arm64、Windows amd64 构建。代理协议依赖升级还要运行真实握手/订阅解析集成测试。

### 10.2 更新策略

- sing-box、sing 及相关 sagernet 模块作为一组升级，不单独强推间接版本。
- Sonic 升级必须运行 differential/golden/fuzz 和 cold-start benchmark。
- SQLite 升级必须运行 migration、并发读写、WAL 和 backup/recovery tests。
- 不仅关注最新版本，也记录 Go 版本、CPU 架构、许可证和安全公告。
- 对全量 registry 实验前后生成 SBOM/模块 diff，量化供应链缩减。

### 10.3 不建议的动作

- 为减少反射再次引入多套 JSON 库并在业务代码混用。
- 仅根据第三方 README benchmark 迁移 Fiber、goccy 或 Zap。
- 用 `replace` 覆盖 sing-box 的传递依赖版本。
- 为“预编译”把动态 SQL 字符串和所有一次性查询都做 stmt cache。
- 在没有 pprof 证据时投入高风险框架重写。

## 11. 分阶段实施

### Phase 0：测量与防回退

- 扩展 JSON、订阅渲染、SQLite、builder benchmark。
- 增加受限 pprof 和关键运行指标。
- 固化 100/1000 节点测试数据与 benchstat 流程。

### Phase 1：低风险高收益

- `writeJSON()` 默认取消缩进。
- 通用 decode 增加 body limit 和单文档检查。
- 逐连接 Info 日志改为 debug/采样。
- 配置 `ReadHeaderTimeout`、`IdleTimeout`、`MaxHeaderBytes`，保留 SSE 例外。
- 高频响应 map 改为 struct。

### Phase 2：订阅与数据库

- Group subscription immutable render cache + ETag。
- 单 group 窄查询，消除 ListNodes 全表读取。
- group/node states 批量查询，消除 N+1。
- benchmark 连接池 1/2/4，选择最优而非拍脑袋修改。
- 只缓存被 profile 证明的 prepared statements。

### Phase 3：sing-box 生命周期

- 实施增量 reload、pool snapshot、按 tag drain。
- 避免规则/订阅变化引发整 box 重建。
- benchmark cold start、reload CPU 和连接中断。

### Phase 4：依赖瘦身

- 实验 selective registry。
- 完成协议 feature matrix 和跨平台构建。
- 以二进制体积、build time、RSS、模块数决定是否合并。

### Phase 5：可选替换实验

只有前述优化完成且仍存在明确瓶颈时，才评估：

- Sonic 与 goccy/go-json A/B。
- 标准 log/slog 与 Zap A/B。
- ServeMux 与 Chi 的维护性迁移。
- 极端订阅下发服务独立进程或专用 server。

## 12. 测试计划

### 12.1 JSON

- Sonic/标准库 differential tests。
- API golden JSON 忽略无意义空白后内容一致。
- canonical identity 在 1000 次运行和三种架构上一致。
- fuzz malformed JSON、深层对象、超大字符串、未知字段和多文档输入。

### 12.2 HTTP

- compact JSON 能被当前 React 前端正常解析。
- 请求体超过上限返回 413，小于上限行为不变。
- 慢 header 被超时，SSE 可持续运行并正常取消。
- `/sub` cache key 覆盖 group/format/mode/aliveOnly/host/generation。
- token 错误请求不能命中并泄露缓存内容。
- ETag 变化与成员/config generation 一致。

### 12.3 日志

- 默认 info 下成功代理连接不输出逐连接日志。
- debug/采样开启时能定位 member 和 destination。
- 同一节点重复失败在窗口内聚合，eviction/blacklist 关键事件不丢。

### 12.4 SQLite

- WAL 并发读写压力测试无数据竞争、死锁和异常 busy 增长。
- 每个连接均启用 foreign_keys、busy_timeout 等要求的 PRAGMA。
- stmt/cache Close 后无资源泄漏。
- migration、transaction rollback 和 node identity reconciliation 行为不变。

### 12.5 sing-box

- 所有支持 URI 协议都能创建、启动、拨号和关闭。
- selective registry 缺少组件时测试明确失败，不能运行时静默跳过节点。
- 增量 reload 下未变化连接不中断。

## 13. 验收标准

- 不再提出项目已经完成的 Sonic、net/http、sing-box Library、原生 SQL 迁移。
- 所有性能 PR 附真实 payload before/after 和复现命令。
- 管理 API 默认输出 compact JSON，前端与 API tests 无回归。
- `/sub` 稳定 generation 的缓存命中请求不做全表节点查询和完整渲染。
- 默认日志级别不产生逐代理连接 Info 日志。
- HTTP 资源限制不破坏 SSE。
- SQLite 连接池选择由压力测试决定，busy 错误率不升高。
- 依赖瘦身不减少任何已支持协议。
- `go test ./...`、race、vet、govulncheck 和目标平台构建通过。

## 14. 参考资料

- [Sonic 官方仓库](https://github.com/bytedance/sonic)：支持范围、兼容配置、平台回退和 Pretouch 提示。
- [goccy/go-json 官方仓库](https://github.com/goccy/go-json)：作为兼容性/A-B 候选，不作为当前默认方案。
- [Go：Using prepared statements](https://go.dev/doc/database/prepared-statements)：statement 生命周期、DB/Tx/Conn 绑定语义。
- [Go：Managing database connections](https://go.dev/doc/database/manage-connections)：`sql.DB` 连接池及限制。
- [net/http Server 文档](https://pkg.go.dev/net/http#Server)：header/read/write/idle timeout 和 header 大小限制。

本方案的核心不是“换更多高性能库”，而是让已经选用的 Sonic、net/http、database/sql 和 sing-box 以符合当前负载模型的方式运行，并用 profile 和端到端 benchmark 决定下一步。
