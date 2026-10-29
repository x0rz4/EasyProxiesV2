# 大批启动探测测试的时间隔离

日期：2026-10-29；基线 4994134；修复版本 2.2.19。

## 失败证据与归因

CI 的 `TestLargeStartupBatchAdvancesWithDetachedProbesAndSlowSubscriber` 报告 Total/Completed=1126、Success=1084、Failed=42、HardTimeouts=17、DetachedProbes=12。用例设置 12 个忽略 context 的阻塞探测、30 个显式失败探测，其余立即成功，113 worker 共用每次尝试 15 ms 的真实时间预算。

最终计数符合 1084/42，失败在硬超时精确等于 12 的断言。`internal/monitor/probe_scheduler.go` 的 `runProbeAttempt` 按每次尝试累计硬超时，启动首轮失败允许重试；因此最终成功的探测也可能在首轮耗尽 deadline，增加累计超时但不留在 DetachedProbes 中。113 个并发任务的 goroutine/GC/系统调度在繁忙 CI 上可能超过 15 ms，原测试把立即返回的函数等同于始终能在真实期限内被调度，这是其不确定性来源。没有证据表明生产超时/重试/最终节点判定错误，不修改生产调度器或放宽断言来掩盖它。

本机 Windows / Go 1.27.2 在未改测试的 4994134 上以 GOGC=1、`-cpu 1,2,4 -count 20` 运行 60 次全部通过（35.432 秒），没有声称在本机复现相同 CI 失败；失败证据为用户提供的 CI 输出，归因结合上述计数与源码路径。

## 修改

将整个无网络 fixture 放入已有标准库 `testing/synctest.Test` bubble。虚拟时钟只在所有 bubble goroutine 都阻塞时前进：立即返回的成功/失败探测不再因主机抢占跨过 deadline，故意阻塞的 12 个探测仍会触发真实调度器的外层 deadline。保留原有 1126 节点、113 worker、15 ms 超时、精确汇总/HardTimeouts/DetachedProbes 断言与三秒推进上限（现在为虚拟时间，不是性能基准）。

两个释放 channel 改为 defer 关闭，保证任意 Fatal 路径也释放故意卡住的探测和订阅回调；新增 entered 信号并在 synctest.Wait 后验证慢订阅确实被调用。没有网络、SQL 或生产代码变动，无新增依赖/migration。生产 timeout 语义与原有实时间单节点超时回归继续保留。

## 验证

修复用例在 GOGC=1、`-cpu 1,2,4,20 -count 50` 下运行 200 次全部通过（7.016 秒），保留精确断言。原始基线/修复输出在忽略目录 `.tools/probe-ci/{baseline,fixed}.txt`。

最终检查顺序执行，全部退出码 0：

```powershell
# 原测试基线（修改前），随后修复用例重复验证
$env:GOTMPDIR = (Resolve-Path .tools/probe-ci).Path
$env:GOGC = '1'
go test -p 1 ./internal/monitor -run '^TestLargeStartupBatchAdvancesWithDetachedProbesAndSlowSubscriber$' -cpu 1,2,4 -count 20 -timeout 90s
go test -p 1 ./internal/monitor -run '^TestLargeStartupBatchAdvancesWithDetachedProbesAndSlowSubscriber$' -cpu 1,2,4,20 -count 50 -timeout 90s
Remove-Item Env:GOGC
# frontend 目录：先构建，再运行 Go；以下 Go 检查使用默认 GOGC
npm run lint
npm test
npm run build
# 仓库根目录
$env:CGO_ENABLED = '0'
go test -p 1 ./...
go build -o .tools/probe-ci/windows.exe ./cmd/easy_proxies
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go vet -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' ./...
go build -trimpath -ldflags '-s -w' -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' -o .tools/probe-ci/linux-amd64 ./cmd/easy_proxies
```

前端 lint / 15 项行为测试 / tsc+Vite 构建通过；Go 全量通过，monitor 包 3.951 秒。日志 `.tools/probe-ci/frontend-{lint,test,build}.txt`、`go-test.txt`、`windows-build.txt`、`linux-{vet,build}.txt` 均保留在忽略目录，不将测试产物入库。版本三处同步 2.2.19，并更新架构文档与审计索引；只修改测试夹具，不变更生产探测预算或统计口径。

当前环境没有 gcc/clang 或可用 WSL Linux 发行版，未执行 Linux 运行态或 race；交叉编译/静态检查不能替代这些检查，远端 CI 需在推送后实际验证。
