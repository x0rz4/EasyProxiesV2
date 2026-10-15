# 回归、规模与运营验收

本文件是执行计划，**本批未运行后端测试、race 或压测**。已有用例来自静态核对，新增场景仍待实现；历史记录见 [nodeview-validation](../../nodeview-validation.md)，不能直接当作本次结果。

## 1. 测试矩阵与证据入口

| Requirement / 场景 | 已有入口 | 本计划必须补充的证据 |
| --- | --- | --- |
| IDENTITY-001：旧身份/乱序/重复 | `internal/probe/applier_test.go`、`internal/monitor/health_applier_test.go` | 重复导入×订阅交错、同名换身份、A→B→A 时旧结果不推进恢复 |
| HEALTH-001：事务/投影重试 | `TestReceiptAndMeasurementRollbackTogether`、`TestProjectionRetryAndConditionalAcknowledgment`、`TestPersistentHealthProjectionIsIdempotent` | 重启 epoch、管理 HTTP 关闭仍能重试、生产调用无旁路 |
| DIAGNOSTIC-001：诊断隔离 | `TestManualResultCannotPublishHealth`、`TestNodeCheckLatencyDoesNotMutateHealth` | EVICTED × 延迟/测速/解锁/质量，比较恢复 streak 与全局状态 |
| MEMBERSHIP-001：候选不放宽 | `internal/groupmember`、`internal/nodeview` 测试；`internal/boxmgr/healer_test.go` | 规划后排除/标签/地区改变，执行二次校验；预览/快照一致 |
| CAPACITY-001：开关与新鲜度 | `internal/group/healer_test.go` | `age=0/max/max+ε`、未来时间、Provisional/blacklist；dry-run 不改槽位 |
| HEAL-001：审计、重读、关闭 | `TestHealerDryRunDoesNotConsumeBudgetsOrPerformActions`、`TestHealerRefreshCircuitAndAuditFailure` | planned 与完成审计分别故障；关闭与各阶段竞争，不新增动作 |
| HEAL-002：共享预算/订阅 | `TestHealerSharesDedupAndBudgetAcrossGroups`、`internal/subscription/manager_test.go` | 分钟边界、熔断到期、重启后 LastAttempt；明确内存预算不持久化 |
| RUNTIME-001：隔离、恢复、连接 | `internal/group/runtime_test.go`、`TestHealingPromotesStandbyWithoutRestartingBoxes` | 高频成员变更、旧代际 cleanup、真实长连接、无监听器重启证据 |
| AUDIT-001：保留/去重 | `internal/store/healer_test.go` | 无写入组独立清理、清理任务停止、去重不被误用作动作计数 |
| SCALE-001：调度/慢消费者 | `internal/monitor/probe_scheduler_test.go`、`internal/monitor/health_events_test.go` | 不同 N/M/S、全超时、队列积压/溢出策略、取消、缓存等价性报告 |
| OPS-001：运维闭环 | `frontend/src/components/GroupPoolsPanel.tsx`、`frontend/src/components/GroupHealLog.tsx` | 缺口→原因→变更→确认关闭；UI、API、审计及采样时间一致 |

时间、随机与网络必须可控；固定 now/fixture 测试不用等待真实一分钟或冷却期。race 与确定性回归互补，race 通过不能代替行为断言。

## 2. 执行命令与前提

使用 go.mod 要求的工具链、Node 22+，race 需要可用 C 编译器。Go embed 要先生成真实前端资产；不能用占位页生成发布证据。

```bash
# 仓库根目录：前端校验和嵌入产物
npm --prefix frontend ci
npm --prefix frontend run lint
npm --prefix frontend run build

go test ./...
go test -race ./internal/group ./internal/outbound/pool ./internal/monitor ./internal/probe
go test ./internal/boxmgr ./internal/store ./internal/subscription

# 实施相关变更时额外验证 Linux 两架构生产构建
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o /tmp/easy-proxies-linux-amd64 ./cmd/easy_proxies
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o /tmp/easy-proxies-linux-arm64 ./cmd/easy_proxies
```

压测/benchmark fixture 尚待 P2.5 增加；届时在报告中写准确命令和种子，不在这里提供一个当前不存在的可执行入口。测试使用隔离数据库与回环模拟网络，不使用生产订阅和真实凭据。

## 3. 场景与容量模型

N/M/S 和主机候选值见 [proposal](proposal.md#规模假设与承诺边界)。每组场景先预热，再稳定运行至少 30min；恢复/取消等故障注入分别留时间线，不能只报告全程平均数。

| 场景 | 输入 | 必看结果 |
| --- | --- | --- |
| 正常周期 | 1k/50/20，延迟 20–500ms | 完成率、等待 P95/P99、事件投影滞后、CPU/内存 |
| 扩展与共享 | 5k/200/100，50% 和 100% 共节点组 | 重复探测/刷新、HealingInputs 扫描量、DB 与分配成本 |
| 故障风暴 | 10/50/100% 失败和硬超时 | lease/worker、剩余缺口、延期分布、无无限积压 |
| 慢消费者 | 有序注入健康成功/失败，阻塞某订阅者 | queue_depth/oldest_age、内存斜率、计数和顺序不变 |
| 订阅异常 | 慢响应、失败三次、重启 | 全局预算、LastAttempt、冷却/熔断、无重试风暴 |
| 连续导入/热更 | 身份替换、地区/标签/政策交错 | 旧结果拒绝、候选一致、监听器重启数与连接存续 |
| 容量关闭/回滚 | 执行中关闭、恢复配置、备份恢复 | 后续动作停止、在途结果合法应用、初检重建 |

先判断请求率是否可服务：`λ ≈ Σ(每节点适用最短间隔的倒数)`；粗略服务率 `μ ≈ 有效并发 / 平均单任务总耗时`。总耗时包括配置的重试和退避占用方式，不能只用拨号超时。先记录每种入口的实际共享限额；当 λ≥μ 时必须单独标记过载，不能用放宽健康新鲜度掩盖积压。

## 4. 指标口径与候选门槛

下列为拟新增观测项，不代表已有 `/metrics` 或 API 字段。指标以窗口累计值为准，审计记录有去重，不能反算真实动作次数。

| 指标 | 口径 | 候选验收门槛（P0.4 确认） |
| --- | --- | --- |
| probe completion | 已完成终态 / 已接纳任务；成功、失败、超时分别计，取消与去重另计 | 可服务场景预热后每周期 ≥99%，全部硬超时也必须终结 |
| queue wait / oldest age | 入队至开始、最老未处理任务年龄 | 可服务场景 P99 等待 < 最短适用周期；30min 内无逐周期增长 |
| hard timeout / stale rejection | 触发总预算终结、旧身份/代际拒绝计数 | fixture 预期一致；取消不变成失败，旧结果状态污染为 0 |
| event projection lag | 合法测量提交至对应消费完成 | 每场景记录 P95/P99/最大值；慢消费者恢复后有限时间排空，语义一致 |
| healer action outcomes | 按 kind/status/reason 计数，含窗口消耗和 backoff until | 分钟计数窗口 ≤20 recovery / ≤2 refresh；不能按滑动窗口误判 |
| healer cycle / deferred | 每轮时长、超时、被预算/忙/政策拒绝比例 | 不超过单轮 2min context 预算而继续启动动作；延期分布能解释 |
| CPU / RSS / DB | 同硬件 CPU、RSS、goroutine、写锁等待、读写延迟、WAL 大小 | 记录基线与优化前后；预热后 RSS 不持续线性增长，资源峰值在主机预算内 |
| data-plane stability | 新连接错误率、已有连接中断数、listener/base box 重启数 | 单纯容量/成员微调造成的 listener/base box 重启数和长连接中断数为 0 |

不承诺真实公网 99% 成功率：这里 completion 是任务按时达到终态，healthy success 是不同指标。节点 ID、URI、订阅 URL 不作为无限基数的 metrics 标签；敏感信息不得出现在报告和结构化日志。

## 5. 人工运营验收

1. 用隔离环境创建多组共享节点，保留一组真实转发长连接；先记录策略和成员快照。
2. 设置 `target_size>0`、执行关闭，注入缺口，验证只有 dry_run 审计和正常周期探测，保留动作调用计数。
3. 启用执行，按 promote / probe / refresh 观察实际 active 与观测健康；审计中 planned 不能当 succeeded。
4. 分别制造候选不足、过期、初检、冷却、预算、熔断和审计失败，从 UI/API 判断原因。
5. 关闭执行，标记关开关时刻，区分关前已在途与关后新动作；确认无新额外请求，不要求正常周期探测停止。
6. 完成政策恢复与数据库一致性备份/恢复演练；确认新进程重新初检，旧收据不重复恢复节点。

## 证据模板

```text
里程碑 / 任务 / Requirement：
提交 / 应用版本 / 配置版本：
环境：OS、架构、CPU、内存、磁盘、Go/Node/C 工具链
输入：N/M/S、重叠比例、种子、延迟/失败分布、初始状态
参数：所有并发归属、间隔、阶段/总超时、预算、退避
命令 / 起止时间（带时区） / fixture 位置：
结果：回归、race、性能分位、峰值、队列与动作统计
对照：优化前后同轨迹的计划/状态是否等价
产物：日志、profiles、截图、CI 链接、备份恢复记录
判定：通过 / 失败 / 未执行；关联门槛与偏差原因
限制与后续任务：
```

文档阶段的验证只检查链接、引用和结构，不应为了填表虚构运行结果。
