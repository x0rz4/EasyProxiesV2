# 后续评估与修复

日期：2026-10-27。基线：502e5eb / 2.2.16。状态：实施中。承接 [首批审计](plan.md) 的全部五项后续建议，不将微基准视为生产容量认证。

## 交付与验证清单

- [x] S1 可观测性：增加默认关闭、仅本机且需管理会话认证的堆/CPU profile；限制类型、时长、并发及写期限。提供不含节点凭据的进程内存/RSS、Go CPU/GC、流量/连接数、分组实例与成员重叠指标，复用健康事件积压接口。验证访问门禁、取消、关闭、参数、指标与 profile 可解析。
- [x] S2 健康日志：实测追加、投递、清理与单写连接饱和；解决容量/保留窗口对规模的限制，保持慢消费者背压、游标与状态原子提交及崩溃重放。报告不同探测速率和消费积压下的边界。
- [x] S3 节点查询：测量事实加载、revision、筛选/排序与序列化成本；优化主导成本，评估 SQL 下推，保持 CONTROL-001、统一成员谓词和接口输出。增加 1k/10k 节点基准及快照一致性回归。
- [ ] S4 balance：测量精确最少连接策略成本；评估并实现合适的吞吐优化。若采用双随机选择需明确配置/兼容性并验证公平性与并发，禁止默默改变既有 balance 语义。
- [ ] S5 分组资源与规模报告：量化组数、成员重叠率、连接数与堆/RSS；评估跨组共享 outbound 的生命周期成本，依据 profile 实施可验证的资源修复，写明采纳/不采纳共享的理由。运行真实本地转发负载并保存命令、输入和结果。
- [ ] 最终验收：逐条核对原五项建议；同步架构/子系统/规格与版本；前端 lint/build、全量 Go 测试、生产 tags vet/链接后提交 Git。未执行的验证必须说明原因。

每项修复独立记录源码证据、变更和验证。先建立测量入口与本地可复现基线，再执行其余项；不会把“已有测试”直接计为规模验证。

## S1 设计

profile 只在启动时显式设置 `EASY_PROXIES_PROFILE_ENABLED=1` 后开放，走自定义管理路由而非默认 HTTP mux；在已有会话认证之外要求非空管理密码与真实 TCP 对端为 loopback，不信任转发头。CPU 时长 1–30 秒，默认 10 秒；只允许 heap/allocs/goroutine/cpu，二进制输出，不支持强制 GC 或 debug 文本，进程内仅一个 profile 请求。请求取消/服务关闭停止 CPU 采集；设置写期限限制慢读者。

资源接口复用管理认证，读取 Go runtime/metrics 避免 ReadMemStats 的全局暂停，RSS 在 Linux 读 /proc/self/statm、Windows 使用工作集计数，其它平台明确标不可用。RSS 与 Go 管理的内存是不同指标；分组统计只输出数量，不输出 URI、身份、tag 或配置内容。健康事件详情继续通过已有接口读取，避免资源轮询额外扫描日志。

### S1 验证记录

- 新增资源/profile 路由与路由契约；采用 `controlplane.RuntimeResourceReader`，boxmgr 实现、httpapi 消费，没有反向 import 或绕过配置/持久化边界。没有新配置字段、依赖、migration 或后台轮询线程。
- `go test -p 1 ./internal/httpapi ./internal/boxmgr ./internal/monitor ./internal/controlplane`：受影响包完整回归通过。涵盖默认关闭与启动 opt-in、会话/密码/loopback门禁、伪造转发头、参数上限、HEAD、全进程并发限制、CPU 取消/服务关闭、写期限重置、分组重叠及空/停止 slot、实际 Windows RSS。
- `TestProfileReadableByGoTool` 实际生成 heap/CPU 文件，并分别调用 `go tool pprof -top` 成功解析；测试临时产物不入库。
- Windows 本机 `go build ./cmd/easy_proxies` 及 Linux amd64 全生产 tags `go vet ./...` / `go build` 均通过。GOTMPDIR 使用仓库忽略目录，未调整系统策略。
- 操作说明与指标局限已写入 `docs/performance.md`，行为约束同步 PERFORMANCE-001。全目标版本统一在最终验收时递增一次。
- S1 是测量能力交付，不声称已经改善吞吐或证明线上 RSS 下降；S2–S5 继续执行。

## S2 设计与源码证据

`internal/monitor/manager.go` 原本每分钟按 5 分钟保留窗口清理；即使消费完全跟上，500 行/秒也会在约 16 秒耗尽 8192 行。改为每秒检查容量，常规年龄清理仍每分钟；到达 75% 水位时缩短已消费历史保留，优先释放所有当前活跃消费者已持久确认的前缀。固定硬上限和真正积压的显式背压保持不变；单次超过容量的突发仍可被拒绝。

压力清理同样只认已提交游标，缺失游标应保守视为 0，不能因尚未保存初始游标就默认已经消费至尾部。停止/删除的消费者仍不约束清理。成功的压力清理与回收行数加入统计，避免隐藏保留窗口缩短。

`HealthEventJournalStats.ConsumerCursors` 全仓没有读调用者，每次统计却额外读取整张游标表并分配 map；删除该旁路，只保留实际使用的聚合统计。基于消费者自身投递进度的现有 API 不变。

### S2 测量与回归证据

环境同首批：Windows amd64、i5-14600KF、Go 1.27.2、GOMAXPROCS=20，SQLite WAL 默认缓存预算。

`TestJournalPressureSustainsHighRateWithBoundedRows` 真实写 SQLite、通过每批事务推进已提交游标，虚拟推进检查时间而不 sleep：90 个一秒批次，100 行/秒接纳 9000 行、峰值 6200 行；500 行/秒接纳 45000 行、峰值 6500 行。运行用时约 0.21/0.78 秒只说明测试执行成本，不能当成真实探测吞吐。此测试验证清理控制和行数边界，未模拟真实网络探测或状态机转换。

`TestJournalPressureReclaimsOnlyConfirmedPrefix` 验证年轻历史提前回收、慢消费者欠账不删、满载继续返回 ErrHealthEventJournalFull、压力回收指标；`TestJournalLowPressurePreservesHistory` 验证低水位保持五分钟历史。新增 missing-cursor 用例在 ddb096b 原实现的 Go overlay 上确实失败（误删除 1 行），修复后通过；没有回退工作区代码。

每项基准顺序运行 3 次，取中位数：

| 基准 | 修改前 | 修改后 | 分配 |
| --- | ---: | ---: | --- |
| HealthJournalStatsWithConsumers（1000 游标） | 320,491 ns/op | 8,238 ns/op | 164,439 → 856 B/op，4803 → 20 allocs/op |
| HealthJournalDelivery/1（逐测量提交、真实单消费者游标确认） | — | 140,552 ns/op，7115 events/s | 18,861 B/op，186 allocs/op |
| HealthJournalDelivery/64（批量提交上界参考） | — | 1,430,563 ns/批，44,738 events/s | 188,252 B/批，3719 allocs/批 |

Delivery 基准实际执行 SQLite 写入、hub 异步投递和游标提交，等待已持久确认后再进入下一批，每 1024 行检查压力；排除测量事实其它写入、网络与分组状态机回调，不能当成系统端到端探测速率或生产容量。单机与批量结果只作单写连接/投递管线参考，不据此保证所有硬件可持续接纳 500 行/秒。

命令及原始文件（均在忽略目录）：

```powershell
$env:GOTMPDIR = (Resolve-Path .tools/pool-performance).Path
go test -p 1 ./internal/store -run '^TestPruneHealthEvents' -count 1
go test -v -p 1 ./internal/monitor -run '^TestJournal' -count 1
go test -p 1 ./internal/store ./internal/monitor -run '^$' -bench 'BenchmarkHealth(JournalStatsWithConsumers|JournalDelivery)$' -benchmem -count 3
# 前态统计基准使用 health-overlay.json 映射 ddb096b 的 internal/store/health_events.go
go test -overlay .tools/pool-performance/health-overlay.json -p 1 ./internal/store -run '^$' -bench '^BenchmarkHealthJournalStatsWithConsumers$' -benchmem -count 3
```

原始结果：`.tools/pool-performance/health-benchmarks.txt`、`health-stats-baseline.txt`、`health-missing-cursor-baseline.txt`。

S2 最终验证：`go test -p 1 ./...` 全包通过（包括原有日志/游标重启、失败重试、游标与状态原子提交、重复 ProbeID、陈旧 revision 及新压力清理用例）；Windows 本机编译、Linux amd64 全生产 tags vet 与链接通过。未引入 migration、扩大硬上限或旁路事务边界。当前实现要求可用的清理调度与已确认前缀：若每秒新增超过剩余空间、写连接长期被占用或消费者卡死，仍可能触发显式背压。单写管线参考吞吐不构成无条件接纳率承诺。

S1 提交：ddb096b。S2 提交：efeaea8。各项当前状态以上方清单为准，尚未完成整体验收。

## S3 测量计划

基线 efeaea8。分别测量真实 SQLite 的完整视图加载（有/无事实）、事实 Loader、已有视图筛选排序、50 行响应序列化，以及合成 32 字段事实的纯视图构造。覆盖 1000/10000 节点，采用确定性输入并记录 CPU/分配 profile。依据主导开销决定是否 SQL 下推：禁止为了分页在存储层重新实现地区/标签/事实 DSL，或让计数、query-ids 与页面范围漂移。

首先评估新鲜加载的数据在 HTTP→nodeview 交接时重复复制、全量 JSON revision 临时缓冲、ID 页的全量排序/物化，以及 Loader 对已有 managed 节点的二次读取。所有采纳的变化必须保留不可变快照和一致读事务，不新增跨请求陈旧缓存。

### S3 已实施变更与 SQL 取舍

- 新增 `nodeview.NewOwned` 交接新鲜数据的所有权，保留 `New` 的防御性复制；页面和候选输出复制嵌套切片。统一空嵌套集合为原有 nil 表示，避免 JSON null/[] 形态漂移。
- revision 采用排序后固定批次的 canonical JSON 增量哈希（节点 256 条/批，事实 32 条/批）；不再制造全量快照 JSON 缓冲。revision 是不透明 token，新实现允许 token 改变；同内容不受输入节点/事实顺序影响，相关内容变化会改变 token。
- ID 页直接在有序快照上扫描计数，仅保留当前页；其它排序只持有下标，预计算名称小写键；诊断条件与搜索文本每请求准备一次，保持已知诊断优先、ID 稳定打破平局、原有方向和成员谓词。
- `Loader.LoadFromNodes` 复用同一读事务里已加载的 managed 行，跳过重复 ListNodes；加载器按基础字段与 provider 行数预留 map 容量，减少扩容，不限制字段集合。

SQL 下推评估：基线 10k 节点不含事实的完整加载约 31.7 ms，而含事实约 332 ms；分配 profile 中 canonical JSON 缓冲增长占 37.7%、事实 Set/map 扩容占 26.6%、nodeview.New 自身复制占 14.6%。当前优先修复这些主导成本；没有另写 SQL 成员/事实 DSL。运行态状态与健康、新旧地区语义、标签和诊断条件需保持共同谓词及 query/query-ids/facets 一致，下推一部分谓词还必须保留完整计数和事实快照 revision；本批不以新增第二套规则换取未经验证的 SQL 分页。元数据与必要事实仍为 O(N) 读取，未宣称查询已变为与总节点数无关。

### S3 基准与 profile

环境同首批。每项 `-benchtime=3x -count 3`，取三次运行中位数；短基准存在缓存/调度噪声，未作统计显著性声明。SQLite fixture 为启用的手动 HTTP 节点，无检测结果；facts_only 为真实 Loader，纯视图构造另用每节点 32 个合成整数字段。所有建库/种子在计时外。

| 10k 节点阶段 | 修改前 → 后 ns/op | 修改前 → 后 B/op | 修改前 → 后 allocs/op |
| --- | --- | --- | --- |
| 纯视图构造（防御性 New） | 160,502,433 → 129,266,633 | 365,203,418 → 144,413,322 | 390,226 → 391,511 |
| 无事实完整加载 | 31,667,633 → 30,001,733 | 45,458,229 → 26,208,133 | 249,947 → 250,075 |
| 含事实完整加载 | 332,005,567 → 207,445,067 | 588,560,069 → 181,641,085 | 1,253,470 → 924,286 |
| facts_only | 123,149,933 → 107,960,400 | 189,224,330 → 117,383,728 | 652,670 → 612,664 |
| 已有视图 ID 页（50 行） | 1,682,500 → 544,700 | 9,502,040 → 32,080 | 10,026 → 10 |
| 已有视图名称排序页 | 2,821,567 → 1,334,400 | 9,982,568 → 793,602 | 30,048 → 10,032 |
| 已有视图风险排序页 | 3,812,900 → 2,132,367 | 9,502,040 → 389,762 | 10,026 → 31 |

1k 含事实加载：37,729,267 → 20,244,500 ns/op，58,745,976 → 18,596,802 B/op；ID 页：102,567 → 58,300 ns/op，603,864 → 32,080 B/op。50 行响应序列化仍为微秒级（10k fixture 中位数约 13 us），未改响应序列化管线，不据短基准波动宣称这部分提速。

含事实完整加载的累计分配减少约 69%，耗时减少约 38%；ID 页临时分配不再随总节点数线性增长，但计数扫描仍为 O(N)。纯构造与无事实加载的 allocs/op 略增，换取的是更小的分段编码缓冲，未隐瞒分配次数变化。

单独运行相同 `load_facts/10k -benchtime=10x` 的 CPU/alloc profile：总累计分配样本约 6.87 GB → 2.20 GB；GC scanObject 累计 CPU 从约 3160 ms 降至 680 ms。profile 含 fixture 与校准过程，只用于定位成本，不视为单次请求指标。修复后主要分配是必要的事实 map、分段编码输出、managed SQL 行和 time.Time JSON，原先的大规模 GrowSlice/完整复制已不再主导。

```powershell
$env:GOTMPDIR = (Resolve-Path .tools/pool-performance).Path
go test -p 1 ./internal/nodeview ./internal/httpapi -run '^$' -bench 'Benchmark(ViewConstruction|NodeViewPipeline)$' -benchtime=3x -benchmem -count 3
go test -p 1 ./internal/httpapi -run '^$' -bench '^BenchmarkNodeViewPipeline/10000/load_facts$' -benchtime=10x -cpuprofile .tools/pool-performance/view-after-cpu.pprof -memprofile .tools/pool-performance/view-after-mem.pprof -o .tools/pool-performance/view-after.test.exe
go tool pprof -top -alloc_space .tools/pool-performance/view-after.test.exe .tools/pool-performance/view-after-mem.pprof
```

原始结果在忽略目录 `.tools/pool-performance/nodeview-baseline.txt`、`nodeview-final.txt`、`view-before-*.pprof`、`view-after-*.pprof`。基线在变更生产代码前测量；中间曾测试逐记录编码，最终使用固定大小批次，表格只引用最终实现。

### S3 最终验证

- `go test -p 1 ./internal/nodefacts ./internal/nodeview ./internal/httpapi` 和随后 `go test -p 1 ./...`：全包通过。
- 新增回归验证 defensive/owned 构造等价与输入脱离、响应不能修改快照、revision 的排序稳定性与内容覆盖、所有既有排序键/方向/已知值优先/稳定平局、ID 页偏移与空页、共享成员筛选，以及 Loader 空集合不扩大与不重复读取。
- `TestNodeViewReusesManagedRowsWithinConsistentReadSnapshot` 在 managed 行读取后真实提交并发写入，再按旧名称事实查询，确认元数据/事实仍来自同一 WAL 读快照，且 ListNodes 调用数为 0。
- Windows 本机编译、Linux amd64 全生产 tags vet 与链接均通过；无新依赖、migration、Store 方法或前端代码变更。未执行 race（环境仍为 CGO_ENABLED=0 且无 C 编译器）。
- 已同步 CONTROL-001 与子系统文档。当前 Go 生产代码 41,166 行、测试 24,503 行；版本留待全目标最后统一递增。S4/S5 与整体验收继续执行。
