# 后续评估与修复

日期：2026-10-27。基线：502e5eb / 2.2.16。状态：已完成（2.2.17）。承接 [首批审计](plan.md) 的全部五项后续建议，不将微基准视为生产容量认证。

## 交付与验证清单

- [x] S1 可观测性：增加默认关闭、仅本机且需管理会话认证的堆/CPU profile；限制类型、时长、并发及写期限。提供不含节点凭据的进程内存/RSS、Go CPU/GC、流量/连接数、分组实例与成员重叠指标，复用健康事件积压接口。验证访问门禁、取消、关闭、参数、指标与 profile 可解析。
- [x] S2 健康日志：实测追加、投递、清理与单写连接饱和；解决容量/保留窗口对规模的限制，保持慢消费者背压、游标与状态原子提交及崩溃重放。报告不同探测速率和消费积压下的边界。
- [x] S3 节点查询：测量事实加载、revision、筛选/排序与序列化成本；优化主导成本，评估 SQL 下推，保持 CONTROL-001、统一成员谓词和接口输出。增加 1k/10k 节点基准及快照一致性回归。
- [x] S4 balance：测量精确最少连接策略成本；评估并实现合适的吞吐优化。若采用双随机选择需明确配置/兼容性并验证公平性与并发，禁止默默改变既有 balance 语义。
- [x] S5 分组资源与规模报告：量化组数、成员重叠率、连接数与堆/RSS；评估跨组共享 outbound 的生命周期成本，依据 profile 实施可验证的资源修复，写明采纳/不采纳共享的理由。运行真实本地转发负载并保存命令、输入和结果。
- [x] 最终验收：逐条核对原五项建议；同步架构/子系统/规格与版本；前端 lint/build、全量 Go 测试、生产 tags vet/链接后提交 Git。未执行的验证必须说明原因。

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
- S1 阶段只交付测量能力，不声称该阶段已经改善吞吐或证明线上 RSS 下降；后续测量与修复见 S2–S5。

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

S1 提交：ddb096b。S2 提交：efeaea8。此处记录该阶段验证，全部交付状态与整体验收见首部清单及文末。

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
- 已同步 CONTROL-001 与子系统文档。S3 时点 Go 生产代码 41,166 行、测试 24,503 行；该阶段版本尚未递增，后续结果见 S4/S5 与整体验收。

## S4 测量与设计

基线 eebb45e。先分别测量 1024/10000 成员在空闲、前 32 个忙、全部忙的精确 balance 选路，以及并发选路和连接计数竞争。现有实现即使已找到 0 连接也扫描剩余全表；可在第一个 0 处结束，保持原有最少连接与拓扑平局顺序。所有连接计数保持非负，由原有一次性关闭释放保证。

若全忙的大池仍有明显线性开销，则增加显式选择的 `balance_p2c` 基础池模式，从同一网络候选快照随机抽取两个不同成员、比较连接数；单成员直接选它。保留原 balance 语义和所有既有默认值，不扩展只支持 fixed/lowest_latency/random 的分组策略。新策略与兼容边界通过配置示例、设置页、规格和公平性/并发测试说明。

### S4 实现与基准

已实现精确 balance 的首个空闲成员早停，以及显式基础池 balance_p2c；后者使用 math/rand/v2 并发安全顶层随机数，避免争用既有池级随机 CAS。normalizeOptions 与 PrepareUpdate/Commit 识别新值，配置示例及设置页新增说明；默认与分组白名单不变。

环境同前，默认 `-benchmem -count 3`，每项中位数，均为 0 B/op、0 allocs/op：

| 场景 | 修改前 balance ns/op | 修改后 balance ns/op | 显式 p2c ns/op |
| --- | ---: | ---: | ---: |
| 1024 空闲 | 698.3 | 1.734 | 12.68 |
| 1024 前 32 个忙 | 727.4 | 33.02 | 12.99 |
| 1024 全忙 | 711.3 | 768.6 | 12.57 |
| 10000 空闲 | 17004 | 1.756 | 13.00 |
| 10000 前 32 个忙 | 16913 | 32.93 | 13.27 |
| 10000 全忙 | 16816 | 17269 | 13.06 |
| 并发调度/计数 1024 | 104.0 | 107.5 | 2.434 |
| 并发调度/计数 10000 | 1382 | 1397 | 2.443 |

精确策略全忙仍需全表扫描，分支增加有小幅成本（此测量约 3–8%），不声称所有负载都改善。p2c 的收益换取了近似选择，常空闲的精确策略反而更快；所以不默认切换。并发数字是 RunParallel 汇总吞吐倒数，不是单连接延迟，也不等于真实代理网络吞吐。

固定种子公平性测试在 64 成员上做 128000 次平局抽样，每成员 1600–2400 次；再做 64000 次持有计数的累计选路，最大/最小活动数差不超过 8。另有 16 worker 共 160000 次并发选路/计数平衡、两候选不重复、双成员必取较少连接、单成员、TCP/UDP 与黑名单边界、策略提交和配置保存/重载测试。这些是特定确定输入下的回归条件，不是所有流量的硬公平性上限。

命令：`go test -p 1 ./internal/outbound/pool -run '^$' -bench '^Benchmark(Balance|TwoChoice)' -benchmem -count 3`；原始结果 `.tools/pool-performance/balance-before.txt`、`balance-after.txt`。首轮新增策略提交测试的手工 fixture 缺少 production 构造器会初始化的 runtimeByTag map，panic 后 defer Close 等锁；通过 15 秒隔离超时栈确认并补齐 fixture，未改生产生命周期代码来掩盖问题。修正后完整 pool/controlplane 包实际运行通过。

S4 最终验证：前端 `npm run lint` / `npm run build` 通过后再执行 Go 检查；`go test -p 1 ./...` 全包通过、Windows 编译及 Linux amd64 全生产 tags vet/链接通过。Vite 保持既有大 chunk 提示。规格同步 RUNTIME-003，配置不新增字段和 migration。该阶段版本尚未递增，后续结果见 S5 与最终验收。

## S5 实验计划

基线 3663506。增加显式 opt-in 的本地 TCP 实验：真实 HTTP CONNECT 客户端 → sing-box 基础池/分组监听 → 本地 HTTP CONNECT 上游 → TCP echo；校验每块 payload。覆盖 100/1000 个独立节点身份、1/8/32 组、每组 50%/100% 成员、1/16/64 个并发长连接。每个配置单独启动/关闭 manager，记录启动时间、资源 API 的 heap/GoMemory/RSS、活动连接、成员放大倍数、收发统计与有效 echo 吞吐；输出 JSON 与 heap/CPU profile 到忽略目录。

输入仅使用 loopback 与合成凭据，不访问公网，不改变实际配置或数据库。实验程序、上游与负载发生器同在测试进程，因此 RSS 包含这些固定/随连接变化的成本；报告测量范围与噪声，不作生产容量承诺。通过强制实验 GC 的存活堆和 profile 识别放大项，再选取保持代际与排空语义的资源修复；另用池快照发布基准验证局部收益。

### S5 发现与修复选择

完整七场景已在临时 SQLite 上跑通，节点、分组、合成已缓存诊断与管理会话均通过正常 store/API；初检保持原有 optimistic 语义，没有把诊断写成路由健康。每场景 3 秒、32 KiB payload，逐块校验。100 节点全重叠从 1/8/32 组时，空闲存活堆约 3.55/7.48/19.27 MB；1000 节点 8 组约 37.46 MB，确认成员与实例数的线性放大。这里 MB 是十进制原始字节的近似，完整结果后续按 MiB 展示。

1000 节点、8 组 heap profile 中，独立 dialer 约占存活样本 22.5%；另外有构建选项、pool 成员/策略、预留 timeline、DNS 缓存和分组状态。累计分配中 YAML emitter 约占 13%，来自保存分组应用指纹；数据传输 CPU 样本主要落在 Windows I/O/调度，不在选路算法。

本批选取三项不改变资源归属的修复：

1. pool 快照不为没有 UDP 的池预分配整张 UDP 索引；当网络候选顺序相同，共用当前快照内的只读切片/索引，全部可用时与 allMembers 共用只读列表。每次发布仍有独立快照数据，不跨版本复用可变容器。
2. monitor 注册新节点不立即预分配 20 条 timeline，首次实际事件才按原上限分配，避免大量未使用/备用节点承担完整历史缓冲。迁移仍保留已有历史。
3. 分组 appliedFingerprint 不保存包含全体成员的 YAML 文本。显式提取原 YAML 包含的五个连接字段和外层策略，使用 canonical JSON 后计算 SHA-256，只保留 64 字符摘要；空 refill 规范化为 nil。YAML emitter 本身分配很高，故没有仅把 YAML 输出接到哈希器。地区/标签等原 yaml:"-" 元数据仍通过统一成员判定影响成员集合，不直接变成连接字段。

暂不跨组共享 outbound：HTTP 实验下可见 dialer 重复成本，但单纯把同一实例塞进多个 box 会破坏每 box manager 的 Close/Remove 归属、路由/DNS context 和代际排空；现有共享 sharedMemberState 仅共享计数/历史，不是 outbound 所有权协议。共享需要独立的中心 runtime owner、租约/引用与跨 box 关闭协议，不能用当前快照引用直接代替。保留独立分组/增量更新的边界，先消除已确认的无效容器与文本保留；此结论不宣称所有协议的 dialer 成本都与 HTTP 相同。

### S5 局部基准

环境同前；修改前 3663506 与修改后在同一机器顺序运行，默认校准、`-benchmem -count 3`，取中位数：

| 10000 节点阶段 | 修改前 → 后 ns/op | 修改前 → 后 B/op | 修改前 → 后 allocs/op |
| --- | --- | --- | --- |
| TCP 快照发布 | 842960 → 378879 | 1589683 → 377696 | 140 → 36 |
| 双协议快照发布 | 1010720 → 379142 | 1900072 → 377696 | 158 → 36 |
| 混合协议快照发布 | 851791 → 629137 | 1588776 → 982832 | 154 → 104 |
| 空闲注册表构建 | 7709072 → 2403958 | 22745987 → 7385991 | 30164 → 20164 |
| 分组应用指纹 | 36691268 → 9083061 | 102215270 → 20975265 | 160196 → 88 |

TCP 快照分配减少约 76%，混合协议约 38%；注册表每个未发生事件的节点少预留 1536 B。分组指纹保留字节从 759237 B 变为 64 B，计算累计分配减少约 79.5%。摘要仍需暂时编码成员连接形态，不能据保留长度宣称计算只需常量内存。1k TCP 快照 154400 → 45360 B/op，空闲注册表 2312250 → 776243 B/op，分组指纹 9888075 → 1089103 B/op。

命令与原始结果：

```powershell
$env:GOTMPDIR = (Resolve-Path .tools/pool-performance).Path
go test -p 1 ./internal/outbound/pool ./internal/monitor ./internal/boxmgr -run '^$' -bench '^Benchmark(SnapshotPublication|RegistryIdleNodes|AppliedGroupFingerprint)$' -benchmem -count 3
```

`.tools/pool-performance/snapshot-before.txt`、`resource-before.txt`、`resource-after.txt`。回归覆盖不可变拓扑与旧版本索引、延迟排序的复制边界、可用性/协议/固定回退、首次 timeline 分配与有界迁移历史，以及指纹对连接/策略的敏感性和对非连接元数据的排除。

### S5 真实连接矩阵与独立进程复测

前后相同 harness、真实 SQLite、每场景 3 秒，矩阵均启用 heap/CPU 采样；全部 payload、活动连接数、最终归零和双向流量下界通过。MiB = 2^20 字节，吞吐只计算完成 echo 校验的单向 payload：

| 节点 / 组 / 每组成员 / 连接 | 空闲堆 MiB 前 → 后 | RSS MiB 前 → 后 | echo MiB/s 前 → 后 |
| --- | --- | --- | --- |
| 100 / 1 / 100% / 1 | 3.39 → 3.25 | 37.18 → 36.38 | 588.9 → 584.5 |
| 100 / 1 / 100% / 16 | 3.76 → 3.45 | 38.39 → 37.48 | 3602.4 → 3598.7 |
| 100 / 8 / 100% / 16 | 7.13 → 6.91 | 43.42 → 42.44 | 3594.2 → 3601.4 |
| 100 / 32 / 100% / 16 | 18.38 → 17.81 | 56.39 → 54.91 | 3560.0 → 3524.9 |
| 100 / 8 / 50% / 16 | 6.07 → 5.88 | 55.71 → 51.44 | 3603.5 → 3569.8 |
| 1000 / 8 / 100% / 16 | 35.73 → 33.15 | 101.08 → 89.89 | 3587.4 → 3589.8 |
| 1000 / 8 / 100% / 64 | 35.61 → 33.16 | 102.31 → 90.83 | 4215.3 → 4200.1 |

表内为完整矩阵各一次，顺序场景的 RSS 包含前序高水位影响，不能横向解释成成员越少 RSS 必然越低。随后只选 `n1000-g8-p100-c16`，关闭 profile，用前后测试二进制各启动 3 个独立进程，中位数：空闲堆 35.77 → 33.18 MiB（约 -7.2%），RSS 96.20 → 85.16 MiB（约 -11.5%），持有连接堆 38.51 → 35.69 MiB，启动 742 → 716 ms，echo 3642.0 → 3614.6 MiB/s。RSS/堆收益在重复中方向一致；吞吐差异在此短测试的波动范围内，未证明端到端吞吐提升，也未作统计显著性结论。

代码与环境变量见 [性能观测文档](../../../docs/performance.md)。前后编译相同新增 harness（修改生产代码前保留 before 二进制），命令：

```powershell
$env:EASY_PROXIES_SCALE_EXPERIMENT = '1'
$env:EASY_PROXIES_SCALE_SECONDS = '3'
$env:EASY_PROXIES_SCALE_OUTPUT = Join-Path $env:GOTMPDIR 'scale-after.json'
$env:EASY_PROXIES_SCALE_PROFILE_DIR = Join-Path $env:GOTMPDIR 'scale-after-profiles'
go test -p 1 ./internal/boxmgr -run '^TestPoolScaleExperiment$' -v -count 1 -timeout 180s -o .tools/pool-performance/scale-after.test.exe
# 独立进程复测时精确筛选单场景、清空 PROFILE_DIR，分别输出每次 JSON
$env:EASY_PROXIES_SCALE_CASE = 'n1000-g8-p100-c16'
$env:EASY_PROXIES_SCALE_PROFILE_DIR = ''
& .tools/pool-performance/scale-after.test.exe '-test.run=^TestPoolScaleExperiment$' '-test.v=true' '-test.timeout=90s'
```

JSON/日志：`.tools/pool-performance/scale-before.*`、`scale-after.*`、`scale-fresh-{before,after}-{1,2,3}.{json,txt}`；profile 在 `scale-{before,after}-profiles/`。PowerShell 直接执行测试 exe 必须把含点的 testing 参数作为完整字符串，否则会被传成不存在的 `-test`；首次复测仅参数错误未运行实验，修正后六次全部通过。关闭负载时取消 context 主动关闭连接产生 Windows I/O abort 日志，负载有效期间的 I/O 错误仍使测试失败。

最终复核修正实验期限：15 秒仅约束 CONNECT 握手，成功后清除该期限，负载退出由 context 关闭连接负责；否则允许的 30 秒场景会被握手期限提前打断。额外用 100 节点/1 组/1 连接运行 30 秒上界，原始结果保存为 `scale-duration-30.{json,txt}`。

修改后 1k/8 组 heap 样本中独立 dialer 仍约 21.9%，builder、DNS 缓存、成员和分组状态继续占用资源；默认 heap 采样误差导致该 profile 总样本甚至略高于前态，故实际存活量用资源指标判断。CPU 1 秒样本以 Windows I/O 和调度为主；累计 alloc profile 包含启动及前序场景，不能当作该场景单次请求成本。下一步是否承担跨 box owner 改造，应由实际协议分布与线上 profile 决定。

## 整体验收（2.2.17）

原五项建议逐一闭环：

| 原建议 | 结果与保留边界 |
| --- | --- |
| profile 与统一规模观测 | S1 默认关闭的认证本机 profile、资源 API；S5 真实本地 TCP 矩阵与重复进程测量。未测线上/公网容量。 |
| 日志容量/保留与吞吐 | S2 75% 压力回收确认前缀、缺游标按 0、去除无用全游标读取；原子游标与慢消费者背压保留。 |
| 查询事实/筛选成本与 SQL 下推 | S3 减少输入复制、分段 revision、复用同事务 managed 行与仅物化 ID 页；已评估暂不另写 SQL DSL，完整事实加载仍 O(N)。 |
| 精确 balance 与近似策略 | S4 精确规则空闲早停、显式基础池 balance_p2c、并发/公平性及配置持久化回归；默认和分组白名单不变。 |
| 多组重叠与共享数据面 | S5 量化资源，减少快照容器、闲置历史和指纹文本；明确独立 owner/租约未建立前不共享 outbound，保留排空边界。 |

版本三处从 2.2.16 同步为 2.2.17；无新依赖、Store 方法或 migration（仍 111 个接口方法、v25）。核对源码实际行数：Go 生产 41254、测试 25357，前端 TS/TSX 10005；同步架构依赖图 boxmgr→jsonx、数据面/性能文档及 RUNTIME-001。首批 502e5eb、S1 ddb096b、S2 efeaea8、S3 eebb45e、S4 3663506；S5 源码、实验与最终记录在本次提交中。

最终命令（顺序执行前端后 Go，全部退出码 0）：

```powershell
# frontend 目录
npm run lint
npm run build
# 仓库根目录
$env:GOTMPDIR = (Resolve-Path .tools/pool-performance).Path
$env:CGO_ENABLED = '0'
go test -p 1 ./...
go build -o .tools/pool-performance/final-windows.exe ./cmd/easy_proxies
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go vet -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' ./...
go build -trimpath -ldflags '-s -w' -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' -o .tools/pool-performance/final-linux-amd64 ./cmd/easy_proxies
git diff --check
```

日志在 `.tools/pool-performance/final-{frontend-lint,frontend-build,go-test,windows-build,linux-vet,linux-build}.txt`。所有新增回归及原有启动/增量/回滚/组间隔离/退休排空链已纳入全量包测试；实验另有两轮完整矩阵和六次独立进程通过。Vite 仍提示既有大 chunk，不影响构建。自审对照 code-standards §L：未旁路统一谓词/节点加载/JSON 出口、未改锁与事件通知顺序、未修改健康归因，只有合成 fixture 凭据入库；二进制、真实配置、SQLite、profile 和原始日志均不提交。

未执行 race：当前 CGO_ENABLED=0 且本机无 gcc/clang；已执行确定性并发与真实连接回归，不能替代 race 认证。Linux 已完成生产 tags 的 vet/链接，未在 Linux 上运行负载。短时 loopback 吞吐没有显著提升结论；线上不同协议与长时间运行容量需用已交付观测入口另行采样。本任务不包含推送、打 tag 或发布。
