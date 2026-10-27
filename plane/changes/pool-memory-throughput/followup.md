# 后续评估与修复

日期：2026-10-27。基线：502e5eb / 2.2.16。状态：实施中。承接 [首批审计](plan.md) 的全部五项后续建议，不将微基准视为生产容量认证。

## 交付与验证清单

- [x] S1 可观测性：增加默认关闭、仅本机且需管理会话认证的堆/CPU profile；限制类型、时长、并发及写期限。提供不含节点凭据的进程内存/RSS、Go CPU/GC、流量/连接数、分组实例与成员重叠指标，复用健康事件积压接口。验证访问门禁、取消、关闭、参数、指标与 profile 可解析。
- [ ] S2 健康日志：实测追加、投递、清理与单写连接饱和；解决容量/保留窗口对规模的限制，保持慢消费者背压、游标与状态原子提交及崩溃重放。报告不同探测速率和消费积压下的边界。
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
