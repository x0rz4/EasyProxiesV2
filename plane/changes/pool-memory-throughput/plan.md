# 池快照内存与选路吞吐审计

日期：2026-10-27。基线：2.2.15（d0cd1f1）。状态：已完成（2.2.16）。

## 架构评估

单进程内嵌 sing-box 避免跨进程转发；数据面快照与控制面写入分离、SQLite WAL 单写多读、统一成员谓词和健康事件日志适合单机代理池。现阶段应先消除局部复杂度与生命周期放大，再凭生产 profile 决定是否拆分服务。

源码发现的本批问题：

1. `internal/outbound/pool/pool.go` 的 DialContext/ListenPacket 把快照 release 回调保留至连接关闭。每份旧快照包含全部成员及三个索引；一个长连接可保留整份旧拓扑，并阻止未使用的退休成员排空。改为快照保护选路和拨号，成功后用选中 memberRuntime 的 operations 保护连接，先取得成员引用再释放快照；TCP/UDP 同步修改。
2. 同文件 nextFixedIndex 在拓扑扫描内再次扫描候选列表，失效回退最坏 O(N×K)。生产路径复用快照候选索引，回退 O(N) 且零分配；控制面任意候选子集构建局部索引，保留原有顺序语义。

## 验收与变更记录

- [x] P1 快照生命周期：覆盖 TCP/UDP、拨号阻塞与热更新交错、未选成员独立排空、选中成员关闭前不排空、重复关闭只释放一次。用 runtime operation 代替连接持有整份快照，删除不再需要的 release 闭包及旧包装入口。
- [x] P2 固定模式回退：覆盖网络候选、环绕、任意重排子集、重复候选与已移除当前成员；1024/4096 成员基准记录修改前后 ns/op、B/op、allocs/op。
- [x] 同步当前形态与规格；版本三处统一递增一次；前端构建后运行 Go 全量测试、生产 tags 构建及 vet，通过后提交 Git。

## 后续建议（已评估与修复，2.2.17）

全部建议的评估、修复和验收结果见 [后续评估与修复](followup.md)；本文件保留首批提交时的建议与证据。

- `balance` 为精确最少连接仍逐连接 O(N) 扫描。若 profile 确认为热点，再评估双随机选择；会改变分配语义，需独立规格和公平性压测。
- 每个分组独立 box/outbound 的资源成本随成员重叠放大。先量化节点数、组数、重叠率、连接数与堆/RSS，再评估共享数据面；不要破坏代际排空和独立策略。
- 节点管理视图仍先加载完整事实快照再分页。大量节点下应测量事实加载、筛选和序列化成本，评估可下推到 SQL 的谓词，同时保持 CONTROL-001 一致快照。
- 健康日志固定容量 8192，已消费保留 5 分钟，持续约 27 行/秒即接近容量。探测速率随规模增长时，应按积压/保留需求规划容量与保留窗口，验证慢消费者和写连接饱和。
- 缺少统一在线 profile 和端到端规模报告。本批微基准只说明局部算法，不等于网络吞吐、RSS 或生产容量已获认证。

建议顺序：先增加受认证/本地访问保护的 profile 与连接数、事件积压、分组重叠率观测，完成固定负载的 RSS/堆/CPU 基线；再处理健康日志容量与管理查询；最后根据 profile 决定是否改变 balance 策略或共享 outbound。后两者涉及语义/生命周期，不能仅凭代码行数决定。

## 基准与回归证据

环境：Windows amd64，Intel Core i5-14600KF，Go 1.27.2，默认 GOMAXPROCS=20。所有基准在同一机器顺序运行，每项 3 次；表中为中位数，不作统计显著性声明。

| 基准 | 修改前 ns/op | 修改后 ns/op | 修改前 → 后 B/op | 修改前 → 后 allocs/op |
| --- | ---: | ---: | ---: | ---: |
| FixedFailover/1024 | 56,418 | 2,199 | 0 → 0 | 0 → 0 |
| FixedFailover/4096 | 858,018 | 9,232 | 0 → 0 | 0 → 0 |
| PoolDialClose | 252.4 | 247.4 | 411 → 379 | 12 → 11 |
| SelectMember1000 | 7.171 | 7.173 | 0 → 0 | 0 → 0 |

固定模式**失效回退这一局部操作**耗时约下降 25.7/92.9 倍；不代表代理网络端到端吞吐同比提升。PoolDialClose 使用无网络 fake outbound，覆盖选路、活动计数、连接包装与关闭，每次分配减少 32 字节与 1 次；延迟变化较小，不据此宣称整体吞吐提升。顺序选择保持原有零分配基线。

内存回归先在原实现复现失败：`TestConnectionPinsOnlySelectedRuntime` 的 TCP/UDP 均因旧快照仍有 reader、无关成员不能排空而失败。修改后通过，且测试在拨号中发布新拓扑并退休旧成员，再让拨号返回，确认保护引用交接顺序。旧快照的 Members/allMembers/TCP/UDP 切片与三个索引不再由已建立连接间接持有；实际 RSS 收益取决于快照更新频率、节点数及长连接数，未做线上采样。

命令：

```powershell
$env:GOTMPDIR = (Resolve-Path .tools/pool-performance).Path
go test -p 1 ./internal/outbound/pool -run '^TestConnectionPinsOnlySelectedRuntime$' -count 1
go test -p 1 ./internal/outbound/pool -count 1
go test -p 1 ./internal/outbound/pool -run '^$' -bench 'Benchmark(FixedFailover|SelectMember1000|PoolDialClose)$' -benchmem -count 3
```

前两项原基准在未改 pool.go 时运行；PoolDialClose 通过 Go `-overlay` 映射 `git show HEAD:internal/outbound/pool/pool.go` 的原实现运行，工作区实现未回退。最终结果均重新测量。基准原始产物放在忽略目录 `.tools/pool-performance/`，本文件保存命令、环境和结果。

数据/配置影响：无 migration，无配置字段变更，健康归因与成员筛选不变；版本三处同步为 2.2.16。

## 最终验收

- `npm run lint`、`npm run build`（frontend）：通过。前端构建完成后才执行全量 Go 测试及构建；Vite 仍有既有约 655 kB chunk 提示。
- `go test -p 1 ./...`：全包通过；GOTMPDIR 指向仓库忽略目录，本轮无应用控制拦截。新增 TCP/UDP 生命周期与固定模式回退测试均实际执行通过。
- `go build -o .tools/pool-performance/easy-proxies-windows-amd64.exe ./cmd/easy_proxies`：链接通过，产物 49,347,584 字节。
- 设置 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` 后执行 `go vet -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' ./...`：通过。
- 相同 Linux 环境执行 `go build -trimpath -ldflags '-s -w' -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' -o .tools/pool-performance/easy-proxies-linux-amd64 ./cmd/easy_proxies`：链接通过，产物 51,593,376 字节。
- `git diff --check`：通过。复核工程规范 §L 与 LESSONS 的锁边界、代际、构建顺序、验证诚实条目；无新依赖、迁移、凭据或手工 assets 变更。
- 未执行 `-race`：本机 CGO_ENABLED=0，且无 gcc/clang；未安装编译器或改变系统策略。未进行线上 RSS/堆采样、真实网络端到端吞吐或持续规模压测。
- 非测试 Go 40,742 行、测试 23,642 行、前端 TS/TSX 10,003 行；当前形态文档基线已同步。
- 本批提交到本地 `codex/pool-memory-throughput` 分支，未推送或部署。
