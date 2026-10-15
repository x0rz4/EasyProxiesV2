# P1 首批：健康写入与取消边界

关联：P0.5、P1.1/P1.2/P1.3；IDENTITY-001、HEALTH-001、DIAGNOSTIC-001。首批只收敛明确的测量入口问题，未宣称阶段 1 或 M1 已完成。

## 审计发现与变更

| 入口 / 调用者 | 写入与门禁 | 分类与本批处理 |
| --- | --- | --- |
| `Manager.Probe` ← API、Healer、单节点健康请求 | Begin 捕获身份，Apply/receipt 后投影；原先直接使用 callback 返回值 | 修复调用方取消后仍写健康、总超时后 callback 返回成功仍写成功的问题；取消释放 lease，探测自身超时仍是合法失败 |
| `MarkAvailableWithoutProbe` ← 无目标的 startup / targeted 请求 | 原先没有测量也能 Begin/commit 成功 | 持久化模式拒绝制造健康收据，保持待测；optimistic/strict 启动选路按原政策；无 Store 内存兼容模式保留 |
| `boxmgr.waitForHealthCheck` ← 启动等待 | 健康数量不足时原先一直等到超时 | 缺少目标时立即返回明确错误，由既有启动流程记录警告；已有足够可用节点的兼容行为不变 |
| `EntryHandle.MarkAvailable` | 直接写 available，无调用者 | 删除未使用的生产可见 setter，减少可旁路入口 |
| `MarkInitialCheckDone` / `RecordSuccessWithLatency` | 直接更新初检/成功事实；仓库调用仅出现在 monitor/pool 测试 | 保留测试 fixture 兼容，明确不作为生产调用；其内部 success helper 仍是健康投影所需实现 |
| `EntryHandle.RecordFailure/RecordSuccess` ← pool | 流量统计与 timeline，不改 available/初检 | 合法数据面统计，不迁为诊断或健康成功 |
| `EntryHandle.Blacklist/ClearBlacklist` ← sharedMemberState | 失败阈值、到期或显式 release | 合法数据面隔离与管理输入，不能把它视为诊断健康写入 |
| `SetProbeStore` ← boxmgr.ensureMonitor / Server.SetStore | runtime 注册前注入 Store；nil 允许内存兼容 | 核对生产绑定，不删除 nil 兼容；反复绑定、运行中更换 Store 的强一致性另行评估 |
| `RestorePersistedHealth` / registry migrate | 恢复历史与身份/代际清理 | 属于生命周期状态，初检/Provisional 约束继续保留 |
| `Server.applyMeasuredResult` ← 测速/质量/解锁等诊断 | 拒绝 Health，Applier 校验 origin/kind，写独立 stream 并确认收据 | 新增四类成功诊断 × 两组共享剔除节点 × 已有恢复进度矩阵，不能触发健康恢复 |

原始审计检索见 [write-path-audit](../../write-path-audit.md)。当前结论只覆盖表中入口；其余策略竞争、订阅补货、增量快照仍按 tasks 继续。

## 回归测试

文件：`internal/monitor/health_boundaries_test.go`。

- `TestPersistentHealthRequiresMeasurementWithoutTarget`：持久化节点无目标时不生成健康；启动选路模式不变。
- `TestInMemoryNoTargetCompatibility`：嵌入/测试的内存兼容行为保留。
- `TestStandaloneProbeCancellationDoesNotChangeHealth`：预先取消、不成功返回、取消后迟到成功均不改变已有健康/收据，lease 可复用。
- `TestStandaloneProbeTimeoutCannotCommitLateSuccess`：synctest 固定虚拟时间，探测预算到期后的迟到成功记为失败。
- `TestHealthResultCannotCrossIdentityRoundTrip`：在途探测期间 A→B→A，旧 revision 不进入健康收据或运行态。
- `TestSuccessfulDiagnosticsDoNotRecoverEvictedMember`：延迟、测速、质量、解锁可持久化，不能改变两组剔除或恢复次数；FIFO barrier 保证异步健康事件已被消费后再比较。
- `internal/boxmgr/manager_test.go` 的 `TestInitialHealthWaitWithoutTargetFailsImmediately`：虚拟时间下无目标不消耗等待预算。原有 `TestHealingPromotesStandbyWithoutRestartingBoxes` 改为通过 Probe/Applier 注入测试测量，继续验证增量提升和禁止重启。

## 验证记录

应用版本 `2.1.1`；Linux amd64、Go 1.27.1、Node 24.19.0，race 使用 gcc，Go 检查使用 `GOMAXPROCS=4` / `-p 4`。前端先构建，再运行 Go 检查，以保证 embed 资源在编译期间稳定。

| 检查 | 结果 |
| --- | --- |
| 新增用例在修复前运行 | 复现无目标制造健康、调用方取消改健康、迟到成功越过超时三类缺陷；随后修复 |
| `go test -p 4 ./internal/monitor -run 'TestPersistentHealthRequiresMeasurementWithoutTarget\|TestInMemoryNoTargetCompatibility\|TestStandaloneProbe\|TestSuccessfulDiagnosticsDoNotRecoverEvictedMember\|TestHealthResultCannotCrossIdentityRoundTrip' -count=1` | 通过 |
| `go test -p 4 -race ./internal/group ./internal/outbound/pool ./internal/monitor ./internal/probe` | 四个包通过；CGO_ENABLED=1 |
| `go test -p 4 ./...` | 24 个有测试的包通过 |
| `go test -p 4 -race ./internal/boxmgr -run 'TestInitialHealthWaitWithoutTargetFailsImmediately\|TestHealingPromotesStandbyWithoutRestartingBoxes' -count=1` | 受影响的启动等待和增量自愈用例通过 |
| `npm run lint` / `npm run build`（frontend） | 通过；保留 Vite 原有大 chunk 提示 |
| `git diff --check`、版本/锁文件一致性及依赖差异检查 | 通过；仅应用版本递增，依赖内容未变 |

工具链下载经官方 SHA256 校验。网络策略阻止了大模块的默认下载地址；SQLite、libc 和 compress 改从官方源码标签生成临时模块缓存，并由 Go 验证与仓库 go.sum 完全一致，不使用 replace、不修改 go.mod/go.sum 或依赖版本。日志暂存 `/tmp/easyproxies-p1-before.log`、`/tmp/easyproxies-p1-after.log`、`/tmp/easyproxies-p1-full.log`、`/tmp/easyproxies-p1-race.log`，临时路径不作为永久证据链接。

本批没有真实公网订阅演练、规模压测、备份恢复或完整 dry-run→执行 UI 验收，不将本结果升级为 M1/M2 通过。

兼容影响：不改 HTTP API、配置格式、数据库 schema 或自愈默认开关。没有目标的持久化节点不再显示为已测活；optimistic 仍允许 provisional 选路，strict 仍等待有效测量。应用版本递增到 `2.1.1`，不自动发布新 Release。

## 后续边界

本批不实现 Topology 事件总线、不改恢复阈值/候选规则、不做事件队列丢弃或缓存优化。调用方取消与内部探测超时分开：前者不形成节点事实，后者在节点未迁移时形成测量失败。仍需完成导入交错、旧 generation、审计/策略竞争、预算窗口和规模报告，见 [tasks](tasks.md)。
