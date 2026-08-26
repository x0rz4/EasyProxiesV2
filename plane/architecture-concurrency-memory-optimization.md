# 系统架构：内存、并发与连接路径优化方案

> 适用范围：EasyProxiesV2 当前 Go 1.25 / sing-box 1.13.x 架构  
> 目标：降低建连热路径的锁竞争与临时分配，约束探测风暴，在不牺牲连接正确性和统计准确性的前提下改善吞吐、P95/P99 延迟与内存稳定性。

## 1. 结论先行

本轮优化不应简单地“再加一个 `sync.Pool`、再写一个熔断器”。当前代码已经具备不少基础能力：

- `internal/outbound/pool/pool.go` 已用 `sync.Pool` 复用候选节点切片；
- 轮询计数、连接数和流量计数已经使用原子变量；
- `internal/monitor/manager.go` 的主动延迟探测已经有并发上限和单节点去重；
- `internal/outbound/pool/shared_state.go` 已实现连续失败、临时拉黑和主动/被动状态联动；
- `internal/geoip/router.go` 已用两个 `io.Copy` 完成双向转发；
- 订阅抓取已复用一个配置了连接池的 `http.Transport`。

真正值得优先处理的热点是：

1. 每次新建连接都会在持锁状态下扫描成员，并逐成员读取多个带锁状态；
2. 每轮拨号重试都会重建候选集合，且每个连接都会分配 `attempted map`；
3. 固定出口的 selector 锁可能覆盖真实网络拨号，导致同组建连串行化；
4. 流量包装层在每次 `Read`/`Write` 都更新统计，并可能阻断 `io.Copy` 的可选快路径；
5. 手动全量探测虽然限制了请求并发，却可能先创建大量等待 semaphore 的 goroutine；
6. 临时拉黑目前还是锁保护的可变状态，全部节点拉黑后的统一释放还可能引发惊群。

因此推荐按以下顺序实施：

| 优先级 | 改造 | 预期收益 | 风险 |
| --- | --- | --- | --- |
| P0 | 建立基准、指标和竞态测试 | 避免凭感觉优化 | 低 |
| P1 | 不可变候选快照 + `atomic.Pointer` | 消除建连读路径的大部分锁和重复扫描 | 中 |
| P1 | 单次候选构建、移除 `attempted map` | 降低每连接分配与重试成本 | 低 |
| P1 | selector 锁不跨越网络拨号 | 避免固定组建连串行 | 中 |
| P2 | 固定 worker pool、分离探测预算 | 限制 goroutine、FD 和外部请求峰值 | 低 |
| P2 | 熔断状态机与半开探测 | 更快故障转移且避免惊群 | 中 |
| P2 | 流量统计批量提交 | 降低每包原子操作与函数调用 | 中 |
| P3 | 按场景恢复 `io.Copy` 快路径 / 缓冲池 | 提升明文 TCP 转发吞吐 | 高，必须基准验证 |
| P3 | `GOMEMLIMIT` / `GOGC` 部署调优 | 使容器内存与 GC CPU 更可控 | 中，依赖负载 |

## 2. 优化边界

### 2.1 控制面与数据面

将运行时职责明确分为两层：

- **控制面**：订阅刷新、配置解析、主动测速、解锁测试、规则筛查、组成员变更、持久化；允许使用锁和批处理，但必须有限流、超时和背压。
- **数据面**：选择节点、建立连接、双向转发、流量统计、被动失败反馈；目标是一次快照读取、常数级选择、无全局锁。

控制面生成不可变运行时快照，数据面只读取快照。流量计数、活跃连接数等高频可变指标放在独立的原子状态对象中，不因快照切换而复制。

### 2.2 不重复实现 sing-box 已有能力

大部分代理协议的加密、复用、协议封装和底层缓冲由 sing-box 负责。应用层只优化自己明确拥有的边界：自定义 pool outbound、GeoIP Router、主动探测、订阅 HTTP 客户端和状态传播。

不得在没有 profile 证据时：

- 为所有代理流量再包一层通用字节池；
- 为原始代理隧道建立通用 `net.Conn` 连接池；
- 用应用代码替换 sing-box 内部已经优化的复制循环；
- 假设所有连接都能使用 Linux `splice`。

TLS、WebSocket、VMess/VLESS 等需要用户态解析或加解密时，通常无法实现纯 socket-to-socket 零拷贝。

## 3. 性能目标与基线

先建立可重复的基线，优化后的验收以相同配置、节点数、并发数和目标地址为准。

### 3.1 建议场景

| 场景 | 节点规模 | 并发 | 观察内容 |
| --- | ---: | ---: | --- |
| pool 建连 | 100 / 1,000 | 100 / 1,000 | 选择耗时、分配、锁等待、拨号成功率 |
| 节点故障 | 1,000，10% 故障 | 500 | 熔断时间、重试放大、P99 |
| 双向转发 | 1 / 10 Gbit 测试档 | 100 / 1,000 连接 | CPU、吞吐、统计误差 |
| 主动探测 | 1,000 / 5,000 | 配置化 | goroutine、FD、完成时间、队列长度 |
| 快照更新 | 每秒 1 / 10 / 100 个事件 | 持续流量 | 快照重建 CPU、旧快照存活量 |

### 3.2 必须采集的指标

- 建连：选择耗时、实际拨号耗时、每连接尝试次数、失败分类；
- 内存：RSS、heap live、heap objects、分配字节率、分配对象率；
- GC：周期数、GC CPU、assist CPU、pause 分布、`GOMEMLIMIT`、`GOGC`；
- 并发：goroutine、打开 FD、探测队列长度、运行中探测数、丢弃/合并任务数；
- 锁：mutex profile、block profile，重点关注 `poolOutbound.mu`、`selectorMu`、group runtime 和 shared state；
- 连接：当前活跃数、半开探测数、熔断节点数、空闲 HTTP 连接数。

基准阶段开启 `pprof` 的 CPU、heap、mutex、block profile；压力测试和状态机单测执行 `go test -race ./...`。

## 4. 建连读路径无锁化

### 4.1 当前热点

`poolOutbound.pickMemberExcluding()` 当前需要：

1. 获取 `poolOutbound.mu`；
2. 扫描所有成员；
3. 对每个成员调用 group availability；
4. 获取 monitor snapshot；
5. 读取临时拉黑状态；
6. 排除本连接已尝试节点；
7. 才执行策略选择。

成员较多时，这会把 O(N) 扫描、多个锁以及临时切片操作放在每次建连热路径上。失败重试时还会重复扫描。`DialContext` 和 `ListenPacket` 又分别创建 `map[*memberState]struct{}` 记录已尝试节点。

### 4.2 不可变快照模型

建议每个 pool 发布一个不可变选择快照：

```go
type memberRuntime struct {
    member *memberState // 长生命周期对象，包含原子计数
    score  int64
}

type selectionSnapshot struct {
    generation uint64
    tcp        []memberRuntime
    udp        []memberRuntime
    bestTCP    int
    bestUDP    int
}

type poolOutbound struct {
    snapshot atomic.Pointer[selectionSnapshot]
    rr       atomic.Uint64
    // writerMu 只用于控制面重建，读路径不取此锁
    writerMu sync.Mutex
}
```

规则：

- snapshot 内的 slice、元素和衍生索引发布后永不修改；
- writer 在私有内存中完成过滤、排序和打分，再调用一次 `Store()`；
- `DialContext` 开始时只 `Load()` 一次，整个拨号过程使用同一代快照；
- 旧快照由 GC 自然回收，已经建立的连接继续持有对应 `memberState`；
- 活跃连接数、流量计数等继续放在 `memberState/sharedMemberState` 的原子字段，不复制到快照；
- 禁止把可变 `map` 或共享 backing array 发布进快照；必须深拷贝后发布。

选择路径变为：

```text
DialContext
  -> snapshot.Load() 一次
  -> 从 tcp/udp 候选中选取
  -> 失败则从同一候选序列选择下一个
  -> 成功后包装连接并返回
```

这样可同时删除每连接的 `attempted map`。实现上可选择以下方式之一：

- 对候选索引做一次偏移遍历；
- 为 weighted/random 生成候选顺序后逐个消费；
- 小规模成员使用栈上/复用的索引 slice；
- 大规模成员使用带代数的复用 bitset，禁止每连接清零完整 N 位数组。

优先采用“候选序列只构建一次并逐个移除/前移”的简单实现，等 profile 证明必要后再引入 bitset。

### 4.3 快照更新触发与合并

不能把每次流量计数或单个 RTT 抖动都转化为一次 snapshot `Store()`。建议事件管线：

```text
订阅/配置/规则/健康事件
        -> 有界 channel
        -> 按 pool 合并和去重
        -> 50~200ms debounce
        -> 构建新快照
        -> atomic Store
```

需要立即反应的“节点进入熔断 Open”可走快速失效标志，随后触发合并重建；普通延迟变化只在一个探测轮次结束后发布一次。

### 4.4 group runtime 快照

`internal/group/runtime.go` 的 `MemberAvailable()` 当前每个节点读取都要获取 group mutex，并可能顺手裁剪失败历史。应拆成：

- **写侧状态**：失败窗口、踢出记录、当前固定节点、持久化标志，继续由 mutex 保护；
- **读侧快照**：当前可选节点集合、固定节点 tag、版本号，以 `atomic.Pointer` 发布；
- 历史裁剪只在写事件或低频维护任务中执行，不在数据面查询时执行。

pool 快照重建时直接消费 group 读侧快照，避免逐成员调用 `MemberAvailable()`。

### 4.5 selector 锁边界

当前 selector 路径需要检查 `selectorMu` 是否覆盖 `SelectOutbound()` 和真实 `DialContext()`。网络拨号绝不能持有全组互斥锁，否则同组的新连接会被上一个慢拨号串行阻塞。

建议：

1. pool 能直接取得成员 outbound 时，直接拨成员，避免通过共享 selector 修改全局选择；
2. 必须使用 selector 时，锁只保护“比较并更新选择”这一小段；
3. 解锁后再执行网络拨号；
4. 若 sing-box selector 的选择是全局可变且无法保证更新后拨号一致性，则为该模式提供每成员独立拨号句柄，不能仅缩短锁后继续依赖全局 selector。

这一项需要并发测试验证“所选成员”和“实际拨号成员”一致，不能只看锁竞争下降。

## 5. 内存池与分配优化

### 5.1 `sync.Pool` 的正确定位

`sync.Pool` 适合复用跨请求的临时对象，但池中对象可能在任何时候被运行时移除，因此它不是缓存，也不能承载状态正确性。

当前候选 slice pool 应保留以下约束：

- 归还时长度重置为 0；
- 不保留异常大的 backing array，当前 `cap > 4096` 不回池的思路正确；
- 从池中取得后不假设内容已清零；
- 任何 slice 在异步任务或 I/O 尚未完成时不得归还；
- 基准确认池本身没有造成跨 P 竞争或扩大常驻内存。

### 5.2 优先消除分配，而不是池化所有对象

推荐顺序：

1. 用一次候选序列替代 `attempted map`；
2. 避免每次重试重新构建候选 slice；
3. 检查 `destination.String()` 和日志字段是否在关闭日志时仍发生格式化；
4. 减少每连接闭包和包装对象逃逸；
5. 只有无法消除且 profile 占比显著的固定尺寸对象才进入池。

不建议池化：

- 带连接生命周期的 `trackedConn`；
- context、timer、错误对象；
- 包含跨请求敏感状态且难以完整重置的结构；
- 长期存活或尺寸高度离散的对象。

### 5.3 字节 Buffer 池的限定范围

应用自有的 `internal/geoip/router.go` 是可评估 `io.CopyBuffer` 的边界。若 heap profile 证明 `io.Copy` 的临时 buffer 分配显著，可使用有限尺寸池：

```go
var copyBufferPool = sync.Pool{
    New: func() any { return make([]byte, 32*1024) },
}
```

实现要求：

- 先用 16 KiB、32 KiB、64 KiB 三档基准，默认不拍脑袋选大 buffer；
- 双向复制必须各自持有独立 buffer；
- `defer Put` 必须在对应 `io.CopyBuffer` 完成之后；
- 超大 buffer 不回池；
- 正确处理半关闭、取消、超时和错误传播；
- 若 src 实现 `WriterTo` 或 dst 实现 `ReaderFrom`，`io.CopyBuffer` 仍可能走其快路径，传入 buffer 不代表一定使用。

不要在 sing-box 内核转发路径外围无差别套 `io.CopyBuffer`，否则可能复制更多、破坏内核已有池化或可选接口快路径。

## 6. 主动探测、被动熔断与并发预算

### 6.1 主动探测现状与改造

`internal/monitor/manager.go` 已按 CPU 数设置并发上限，也有全量轮次去重和按 tag 去重。这部分应配置化，而不是重写。

`internal/monitor/server.go` 的手动 probe/unlock 路径虽使用 `semaphore.Weighted` 限制运行中的请求，但若先为所有节点启动 goroutine，再在 goroutine 内 `Acquire`，大批节点时仍会产生大量等待 goroutine。建议改成固定 worker pool：

```text
请求产生任务 -> 有界队列 -> N 个固定 worker -> 执行 -> 聚合结果
```

并发上限不能只依赖 `NumCPU()`，还应同时受以下配置约束：

- 最大打开 FD 预算；
- 单节点可能发起的子请求数；
- 请求超时和平均完成时间；
- 容器内存上限；
- 外部探测服务的速率限制。

普通 URL Test 与 unlock 检测应使用不同预算。unlock 一次会访问多个站点且持续更久，不宜与轻量探测共用同一个无权重 semaphore。

建议配置：

```yaml
monitor:
  latency_workers: 32
  unlock_workers: 8
  queue_capacity: 2048
  enqueue_timeout: 2s
  schedule_jitter: 20%
```

队列满时的策略必须可观测：周期任务合并/跳过旧任务，用户手动任务返回明确的“系统繁忙”，不能无限阻塞或静默丢弃。

### 6.2 被动熔断状态机

现有连续失败和临时拉黑能力建议演进为明确状态机：

```text
Closed --连续有效失败--> Open
Open   --冷却到期------> HalfOpen
HalfOpen --单个试探成功--> Closed
HalfOpen --试探失败-----> Open（退避时间增加）
```

关键约束：

- 每个节点 HalfOpen 同时只放行 1 个或极少量试探连接；
- Open 时长采用指数退避并加 jitter，设置最大值；
- 主动探测可以推动状态转换，但要有连续成功/失败阈值，避免抖动；
- 所有节点都 Open 时，只挑一个“最早到期/最近质量最好”的节点半开试探，不要清空全部黑名单形成惊群；
- `context.Canceled`、客户端主动关闭、正常 EOF 不能计为节点故障；
- 区分 DNS、超时、拒绝、认证、TLS、协议错误和已建连后的 I/O 错误；
- “短期熔断”与 group 的“5 分钟 3 次后 EVICTED”是两个不同层次，不能共享一个含糊状态。

熔断状态的热读部分可用原子枚举和截止时间；复杂的失败窗口和退避计算在状态迁移锁内完成。只有发生状态转换时才发布快照更新事件，单次失败不重建整个 pool 快照。

### 6.3 健康事件背压

当前健康事件回调在复制订阅者后同步执行。同步回调适合必须立即生效的轻量 failover，但不适合数据库写入、HTTP 通知或复杂规则重算。

建议拆分：

- fast subscriber：只更新内存状态、触发一次非阻塞 dirty 标志；
- async subscriber：进入有界队列，负责持久化、审计和规则重算；
- 对异步队列定义合并键，例如 `poolID/nodeTag`，避免同一节点的高频状态覆盖系统。

## 7. 数据拷贝、统计与零拷贝

### 7.1 当前约束

`trackedConn` 在每次 `Read`/`Write` 后更新流量并检查错误。这样统计及时，但带来：

- 每个读写块的函数调用和原子加法；
- monitor entry 的额外更新；
- 连接包装只暴露 `net.Conn` 接口时，底层连接的 `io.ReaderFrom`、`io.WriterTo` 等可选能力不会自动可靠透传；
- `io.Copy` 因而可能退回用户态 buffer 循环。

### 7.2 先降低统计热度

在尝试“零拷贝”前，先将流量统计改为连接局部累计、分批提交：

- 每累计 256 KiB～1 MiB 或每 500 ms～1 s flush 一次；
- `Close()` 前执行最终 flush；
- 总量统计必须保持最终一致；
- 活跃连接计数仍在连接建立/关闭时立即更新；
- 实时面板允许亚秒到 1 秒延迟，并明确 SLA；
- 避免为每个连接单独创建 ticker，可使用时间阈值检查或共享 flush 调度器。

批量阈值必须通过吞吐与面板实时性基准确定。

### 7.3 快路径设计

可考虑为包装连接实现 `ReadFrom` / `WriteTo`，在底层支持时委派，然后一次性累计字节数。但必须验证：

- 不会递归回自身的 `Read`/`Write`；
- 半关闭语义保持一致；
- 错误仍按正确类别上报；
- 统计在长连接传输结束前可能不可见；
- TLS/协议包装下是否实际命中快路径；
- sing-box 的连接包装是否允许安全透传。

不应盲目暴露 `syscall.Conn` 或底层 FD。它可能绕过限速、统计、截止时间或协议包装，且使上层错误地假设可以直接操作真实 socket。

建议提供经基准验证的统计模式，而不是一个全局“零拷贝开关”：

| 模式 | 实现 | 适用 |
| --- | --- | --- |
| precise | 每次读写记账 | 默认兼容、低流量、需要最实时统计 |
| batched | 用户态复制，本地累计后批量提交 | 推荐默认，兼顾统计与吞吐 |
| throughput | 委派 `ReaderFrom/WriterTo`，流结束时结算 | 明文 TCP、大流量、允许统计延迟 |

Linux `splice` 的收益必须在 Linux 生产内核上测，Windows 开发机结果不能代替。加密或协议转换路径预期不会获得真正的内核零拷贝。

## 8. 连接复用

### 8.1 可以复用的连接

订阅抓取当前已经共享 `http.Client/http.Transport`，并配置 `MaxIdleConns`、`MaxIdleConnsPerHost` 和 `IdleConnTimeout`。后续只需：

- 确认所有 response body 都被关闭，必要时在可控上限内 drain；
- 根据订阅域名数量和并发刷新量调整 per-host 上限；
- 增加 `MaxConnsPerHost` 防止单域名突发；
- Manager 停止时继续调用 `CloseIdleConnections()`；
- 用指标确认复用率，不按理论值过度增大空闲池。

unlock 检测当前每次 check 创建 transport，并禁用 KeepAlive；同一次 check 的多个服务因此也不能复用连接。是否开启复用需要单独验证：不同站点本身无法共享普通 HTTP/1.1 连接，而长期缓存每节点 transport 会持有大量空闲 FD。推荐先维持“每次任务独立生命周期”，只在同 host 多请求且 profile 证明握手占比显著时开启短生命周期 KeepAlive，并在任务结束后 `CloseIdleConnections()`。

### 8.2 不应复用的连接

不能为任意代理 TCP 隧道建立通用连接池。原始隧道通常与目标地址、认证、协议状态、用户会话和半关闭状态绑定，错误复用会造成串流、数据泄漏或协议损坏。

代理协议本身需要多路复用时，应使用 sing-box 对该协议支持的 mux，并为并发流、连接寿命、空闲超时和故障域设置上限。

## 9. Go GC 与容器内存调优

### 9.1 原则

Go GC 的主要工作是并发完成，不能把所有延迟简单归因于“频繁 STW”。是否调整 GC 必须根据分配率、live heap、GC CPU、assist 和 pause 数据判断。

推荐优先使用部署层环境变量：

```text
GOMEMLIMIT=<进程可使用的 Go 内存预算>
GOGC=100
```

`GOMEMLIMIT` 是软限制，不是容器 OOM 的硬保护。它主要约束 Go 运行时管理的内存，内核 socket buffer、部分 mmap、cgo/系统库和其他进程开销仍需预留。

### 9.2 内存预算

若容器限制为 `C`，起始建议不是直接设置 `GOMEMLIMIT=C`，而是：

```text
GOMEMLIMIT = C - 非 Go 内存峰值 - 安全余量
```

通用服务至少保留 10% 左右余量；本项目还包含 GeoIP 数据、网络缓冲和大量连接时，建议先以容器上限的 75%～85% 做实验，再根据 RSS 和 OOM 风险上调。过低的限制会使 GC 进入频繁回收但无法有效降内存的 thrashing 状态，CPU 和尾延迟反而恶化。

### 9.3 `GOGC` 实验矩阵

不要直接把生产默认写死为 200。用固定负载测试：

| 组合 | 目的 |
| --- | --- |
| `GOGC=100` + 合理 `GOMEMLIMIT` | 基线 |
| `GOGC=150` + 同一 limit | 用更多 heap 换较少 GC CPU |
| `GOGC=200` + 同一 limit | 高内存档 |
| `GOGC=off` + limit | 仅压测诊断，不作为默认生产配置 |

比较吞吐、P99、GC CPU、RSS 峰值和 OOM 余量。若 `GOMEMLIMIT` 经常接管回收节奏，提高 `GOGC` 的收益可能有限。

应用启动代码默认不调用 `debug.SetGCPercent()` 或 `debug.SetMemoryLimit()` 覆盖运维环境。只有在明确增加配置项、启动日志展示最终值并支持诊断接口后，才考虑由应用管理。

## 10. 配置建议

新增配置应提供安全默认值，并允许环境覆盖：

```yaml
performance:
  selection:
    snapshot_debounce: 100ms
  health:
    latency_workers: 32
    unlock_workers: 8
    queue_capacity: 2048
    schedule_jitter: 20
  circuit_breaker:
    failure_threshold: 3
    open_duration: 30s
    max_open_duration: 10m
    half_open_requests: 1
  traffic_accounting:
    mode: batched
    flush_bytes: 524288
    flush_interval: 1s
```

注意：`flush_interval` 不应通过“每连接一个 ticker”实现；配置值是最大可见延迟语义，不代表必须为每条连接建立定时器。

## 11. 分阶段实施

### 阶段 A：测量与低风险分配优化

涉及：

- `internal/outbound/pool/pool.go`
- `internal/outbound/pool/shared_state.go`
- benchmark / race / failure-injection tests

任务：

1. 增加 pool 选择、失败重试、1,000 成员并发建连 benchmark；
2. 增加 mutex/block/alloc 指标；
3. 单次构建候选序列并移除 `attempted map`；
4. 降低或采样每次拨号尝试的高频 Info 日志；
5. 校验现有 candidate pool 的容量上限与归还时机。

验收：成功路径每连接临时分配明显下降，失败重试不重复全量扫描；功能和选择策略不变。

### 阶段 B：不可变快照

涉及：

- `internal/outbound/pool/pool.go`
- `internal/outbound/pool/shared_state.go`
- `internal/group/runtime.go`
- `internal/monitor/manager.go`

任务：

1. 定义 pool/group 读侧快照；
2. 建立健康/配置事件合并器；
3. 读路径改为单次 atomic Load；
4. 将历史清理移出读路径；
5. 修复 selector 锁覆盖网络 I/O 的问题；
6. 做并发快照切换和连接持续性测试。

验收：pool 选择热路径不获取 pool/group 全局锁；健康变化在目标时间内生效；旧连接不受快照切换影响。

### 阶段 C：探测与熔断

涉及：

- `internal/monitor/manager.go`
- `internal/monitor/server.go`
- `internal/outbound/pool/shared_state.go`
- `internal/group/runtime.go`

任务：

1. 手动全量任务改为固定 worker pool；
2. latency/unlock 分离并发预算；
3. 增加队列深度、等待时间、跳过/合并计数；
4. 实现 Closed/Open/HalfOpen 和单节点试探令牌；
5. 统一错误分类与退避 jitter；
6. 内存状态转换与异步持久化解耦。

验收：5,000 节点任务的 goroutine 和 FD 峰值均受配置约束；故障节点在阈值内熔断；恢复无惊群。

### 阶段 D：复制路径与流量统计

涉及：

- `internal/outbound/pool/pool.go` 中 tracked connection
- `internal/geoip/router.go`

任务：

1. 流量统计局部累计并批量提交；
2. 针对 GeoIP Router 评估有限 copy buffer pool；
3. 原型验证 `ReaderFrom/WriterTo` 委派；
4. 在 Linux 上分别测试明文 TCP、TLS 和协议包装路径；
5. 保留精确统计模式作为兼容回退。

验收：总流量最终一致，关闭时无漏记；吞吐模式只在证明确有收益的路径启用；半关闭和错误处理测试通过。

### 阶段 E：部署参数

任务：

1. 暴露 Go runtime/metrics 和进程 RSS；
2. 为小/中/大内存实例提供 `GOMEMLIMIT` 样例；
3. 跑 `GOGC=100/150/200` 矩阵；
4. 文档化容器内存余量和回滚方式；
5. 生产灰度观察至少一个完整订阅、探测和流量周期。

## 12. 测试矩阵

### 12.1 正确性

- TCP/UDP、IPv4/IPv6；
- round-robin、random、lowest-latency、fixed；
- 节点在选择后、拨号前、拨号中、已建连后分别失效；
- snapshot 切换时旧连接持续传输；
- 所有节点 Open 时只放行受控 HalfOpen；
- context 取消、客户端 EOF 不触发错误熔断；
- 双向复制单边 EOF 后 half-close 行为正确；
- 流量统计在正常关闭、超时、reset 和异常退出时不重复/不漏记。

### 12.2 并发与资源

- `go test -race ./...`；
- 1,000 并发建连与持续快照更新；
- 5,000 节点手动探测，确认 goroutine/FD 上界；
- 慢订阅者、慢数据库和满队列的背压行为；
- pool buffer 在高峰后能被 GC 回收，不形成永久大对象保留；
- 在低 `GOMEMLIMIT` 下验证服务降级和告警，而非无限重试。

### 12.3 性能验收建议

最终阈值应以现有硬件基线填写，建议至少满足：

- pool 选择阶段 P99 相比基线下降 50% 以上；
- 1,000 成员成功建连路径 allocs/op 下降 30% 以上；
- selector 模式不存在“一个慢拨号阻塞同组所有新拨号”；
- 探测任务运行中 goroutine 数不再与节点数线性增长；
- 相同吞吐下 GC CPU 和总 CPU 均不劣化；
- 流量统计误差为 0，展示延迟不超过配置 SLA；
- 快照更新期间已有连接中断数为 0。

## 13. 回滚策略

- 快照读路径保留一个版本周期的旧实现 feature flag，灰度对比后再删除；
- 新熔断器支持切回当前连续失败 + 固定 blacklist duration 逻辑；
- batched/throughput 统计可切回 precise；
- copy buffer pool 和可选接口委派分别设置开关，便于定位回归；
- `GOMEMLIMIT/GOGC` 只通过部署配置变更，回滚不依赖重新编译；
- 任一阶段必须独立可发布，禁止把快照、熔断、复制路径和 GC 调优合成一次大改。

## 14. 参考资料

- Go `sync.Pool`：<https://pkg.go.dev/sync#Pool>
- Go `io.Copy` / `io.CopyBuffer` 快路径语义：<https://pkg.go.dev/io#Copy>
- Go GC Guide（`GOGC`、soft memory limit、余量与 thrashing）：<https://go.dev/doc/gc-guide>
- Go `runtime/metrics`：<https://pkg.go.dev/runtime/metrics>

