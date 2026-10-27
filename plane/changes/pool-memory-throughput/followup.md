# 后续评估与修复

日期：2026-10-27。基线：502e5eb / 2.2.16。状态：实施中。承接 [首批审计](plan.md) 的全部五项后续建议，不将微基准视为生产容量认证。

## 交付与验证清单

- [x] S1 可观测性：增加默认关闭、仅本机且需管理会话认证的堆/CPU profile；限制类型、时长、并发及写期限。提供不含节点凭据的进程内存/RSS、Go CPU/GC、流量/连接数、分组实例与成员重叠指标，复用健康事件积压接口。验证访问门禁、取消、关闭、参数、指标与 profile 可解析。
- [x] S2 健康日志：实测追加、投递、清理与单写连接饱和；解决容量/保留窗口对规模的限制，保持慢消费者背压、游标与状态原子提交及崩溃重放。报告不同探测速率和消费积压下的边界。
- [ ] S3 节点查询：测量事实加载、revision、筛选/排序与序列化成本；优化主导成本，评估 SQL 下推，保持 CONTROL-001、统一成员谓词和接口输出。增加 1k/10k 节点基准及快照一致性回归。
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

S1 提交：ddb096b。S2 与本记录同批提交；S3–S5 与最终整体验收仍未完成，目标继续保持进行中。
