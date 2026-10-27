# 性能观测与采样

当前实现：`internal/httpapi/server_resources.go`、`internal/boxmgr/resources.go`、`internal/monitor/manager.go`。采样走已有管理监听器和会话认证，不创建第二个监听器，不注册默认 `/debug/pprof` mux。

## 资源指标

`GET /api/operations/resources` 返回采样时间、Go 堆对象字节、Go 管理且未归还 OS 的内存估计、累计分配字节、GC 次数、Go 估计 user/GC CPU 秒数、goroutine/GOMAXPROCS、RSS，以及流量/活动连接与探测状态。

- Go 指标使用 `runtime/metrics`，不调用会触发全局暂停的 `ReadMemStats`，不主动 GC；CPU 指标是 Go runtime 的累计估计，不能等同于系统工具的进程 CPU 百分比。
- `rss_bytes` 在 Linux 读取 `/proc/self/statm`，Windows 读取当前进程 working set；不可用时 `rss_available=false`，不能把 0 当成实际占用。RSS 与 Go heap/GoMemory 不是相同指标。
- `runtime` 由可选 `controlplane.RuntimeResourceReader` 提供：base/分组实例数、当前成员引用数、独立分组成员数、单成员最多覆盖组数、退休中成员数及 `group_amplification=group_members/unique_group_members`。计入 standby，因为它们仍持有 outbound。停止的 slot 不计作运行实例；退休引用单独计数。
- 分组指标是跨 slot 的观测值，不是与并发重载原子一致的拓扑快照；不返回 tag、URI、身份或组配置。流量/连接数来自 monitor 注册表的聚合。
- 健康日志容量、积压、最老事件年龄、拒绝次数与每消费者游标继续查看 `GET /api/operations/health-events`，避免每次资源采样都扫描日志。

常规生产采样建议每 5–10 秒一次，按时间差计算累计分配/流量/CPU 的速率；保持节点数、组数、重叠率、连接数和负载模式相同再比较。

## 按需 profile

启动前设置 `EASY_PROXIES_PROFILE_ENABLED=1`。不设置时 profile 返回 404；开启后仍需已设置管理密码、有效会话 Cookie/Bearer，以及真实连接对端是 loopback。`X-Forwarded-For` 不参与判定；远程采样可通过 SSH 本地端口转发访问。会话凭据的获取和有效期沿用管理登录接口。

支持 `GET /api/operations/profile/heap`、`allocs`、`goroutine`、`cpu`，输出 gzip 的 pprof protobuf。CPU 支持 `?seconds=1..30`，默认 10 秒；其它类型不接受查询参数。禁止 debug 文本、强制 GC、trace、cmdline 和任意 profile 名称。HEAD 返回 405，避免探活触发采样。

```sh
# TOKEN 是已登录的管理会话，不是 API Key；不要把真实凭据写入版本库。
curl --fail -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:9888/api/operations/resources
curl --fail -H "Authorization: Bearer $TOKEN" \
  -o heap.pprof http://127.0.0.1:9888/api/operations/profile/heap
curl --fail -H "Authorization: Bearer $TOKEN" \
  -o cpu.pprof 'http://127.0.0.1:9888/api/operations/profile/cpu?seconds=10'
go tool pprof -top ./easy-proxies cpu.pprof
go tool pprof -top -inuse_space ./easy-proxies heap.pprof
```

全进程仅一个 profile 请求，竞争返回 429。CPU 采样随请求取消或 Server 关闭停止；CPU 写期限为采样时长加 10 秒，堆等即时 profile 为 10 秒，结束后重置该请求的写期限。heap 是最近一次 GC 的存活样本，可能滞后；不要据此承诺每次更新后 RSS 立即下降。profile 文件可含调用栈与运行细节，应按管理诊断数据保管，不提交到 Git。

## 验证边界

当前路由与访问控制、CPU 取消/关闭/并发、RSS 读取、分组重叠计数及 `go tool pprof -top` 解析由同包回归覆盖。可复现实验和具体规模结果记录在 `plane/changes/pool-memory-throughput/followup.md`；功能通过不代表特定生产机器的吞吐或内存容量已获认证。
