看了 `internal/` 下主要文件（`monitor/server.go` 3092 行、`boxmgr/manager.go`、`store/sqlite.go`、`subscription/manager.go`、`unlock/unlock.go` 等），说实话整体工程质量不差——health check 用了 CPU 数动态算的 worker pool（`monitor/manager.go:419` `workerLimit := runtime.NumCPU() * 2`）、`Snapshot()` 锁粒度控制得很短、unlock 检测的每个 HTTP 调用都有 `context.WithTimeout`。但翻下来确实有几处能明确指出问题、给出方向的点：

## 1. Reload 架构（已经聊过的部分，排第一优先级）

这个不重复展开了，之前分析的"整实例重建 + 版本化 tag 增量热更"是目前影响最大的一项，其余几点相对都是增量优化。

## 2. `http.Server` 没设超时——一个具体的、可以立刻改的点

`internal/monitor/server.go:216`：

```go
s.srv = &http.Server{Addr: cfg.Listen, Handler: mux}
```

`ReadTimeout` / `WriteTimeout` / `IdleTimeout` / `ReadHeaderTimeout` 全部留空（Go 默认无限）。这是 `gosec` 会直接标记的经典问题（G112，Slowloris 风险）：一个慢连接可以一直占着 handler goroutine 不释放。项目里又有好几个 SSE 长连接端点（`handleDebugStream`、`handleProbeAll`、`handleUnlockAll`），这些确实需要长连接，不能粗暴设一个全局 `WriteTimeout`，但至少应该：

- 给非 SSE 的普通 API 路由单独套一层 `http.TimeoutHandler`，或者
- 只设 `ReadHeaderTimeout`（防止请求头一直不发完卡住连接，SSE 场景不受影响，因为头发完之后 body 阶段才是流式的）+ `IdleTimeout`（防止 keep-alive 连接堆积）。

这个改动成本很低，收益是实打实的（防止面板暴露在公网时被慢连接攻击拖垮）。

## 3. SQLite 单连接：读写都挤在一条连接上

`internal/store/sqlite.go:33`：

```go
db.SetMaxOpenConns(1) // SQLite only supports 1 writer
```

这个注释本身有个常见的误解：SQLite 在 **WAL 模式**下是支持"多个并发读 + 单个写"的，写的时候不阻塞读。但这里把 `MaxOpenConns` 直接锁死成 1，等于把本可以并发的读请求也全部串行化到一条连接上排队。如果面板前端有多个 tab 同时轮询统计接口，或者健康检查在写状态的同时用户在看 debug 面板，会出现不必要的排队等待。

优化方向：开两个 `*sql.DB`（同一个文件路径，DSN 里都开 WAL），一个专门 `SetMaxOpenConns(1)` 给写路径用，另一个 `SetMaxOpenConns(4)` 左右给只读查询用。这是 WAL 模式下的标准做法，modernc.org/sqlite（项目用的这个纯 Go 驱动）完全支持。

## 4. `monitor/server.go` 是个 3092 行的单文件

路由、业务逻辑、JSON 序列化、DB 访问全部堆在一个文件里。这不是性能问题，是可维护性问题——但会间接影响性能优化的效率：现在想单独给某类接口（比如统计查询）做优化，很难不牵一发动全身。方向是按领域拆分：`handlers_nodes.go` / `handlers_subscriptions.go` / `handlers_sse.go` / `handlers_settings.go` 之类，路由表单独收敛到一个文件里注册。这样后面无论是加缓存、加限流还是做前面说的读写连接分离，改动面都能收窄。

## 5. 一个优先级较低、可以观察后再动的点

`internal/outbound/pool/shared_state.go` 用 `sync.Map` 存 tag → 状态。`sync.Map` 是为"少数 goroutine 高频写不相交的 key、大多数 goroutine 只读"这种模式优化的；这里的实际访问模式更像是所有节点的健康检查/流量计数器高频更新（读写都频繁，且 key 集合不算特别大，从 config 看通常是几十到几千个节点级别）。这种模式下普通 `map + sync.RWMutex`（或者分片 map）在中等规模下往往比 `sync.Map` 更快，因为 `sync.Map` 的读写分离设计在这种访问模式下反而有额外开销。不过这个只有在节点数很大（几千+）且流量计数确实成为热点时才值得改，建议先用 `pprof` 实测一下再决定要不要动，别没测先优化。

---

排序建议：**先做 Reload 架构（已定方案）→ 顺手把 `http.Server` 超时补上（5 分钟的事）→ 观察 SQLite 读写是否真的有排队再决定要不要拆连接池 → `server.go` 拆分放最后，作为长期维护性投资**。`sync.Map` 那条不建议现在动，纯属过早优化。