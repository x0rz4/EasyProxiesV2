# 稳定性审计：订阅、健康事件与热重载

日期：2026-10-23。基线：`8349950`（package.json 2.2.4）；本批工作区目标版本 2.2.5。未提交、合并或发布。

## 结论与已修复问题

| ID / 优先级 | 触发与证据 | 修复与验证 |
| --- | --- | --- |
| S1 / P2 | `internal/subscription/manager.go`：`RefreshOne → commitFetched → RecordSubscriptionNotModified` 失败后只记日志，仍返回 nil。违反 LESSONS L-006 的失败可见性要求。 | 返回包装错误，沿原有路径更新 LastError；`TestRefreshNotModifiedPersistence/write_failure` 修复前实测返回 nil，修复后通过。正常 304 只更新时间，不制造节点或 LastSuccess。 |
| S2 / P1 | 同文件：`Update` 换源并清空成员时，正在抓取的旧结果仍可 `CommitSnapshot`，旧源重新入库；失败/304 也可污染新源元数据。违反订阅子系统的换源清空语义及窄写边界。 | `resultMu` 串行化源编辑/删除与提交，抓取前重读源并捕获进程内版本；URL/Format/UserAgent 修改推进版本，提交拒绝旧版。`TestRefreshRejectsResultAfterSourceChange/url` 修复前实测旧请求成功，修复后通过；扩展覆盖 A→B→A、格式、UA、旧 304、旧失败、删除、取消与仅改名。 |
| S3 / P2 | 同文件：单项刷新先 `recordRunResult(nil)`，再执行可能失败的 `reconcileRuntime`，调用方收到错误但状态仍显示成功。 | 协调结束再记本轮状态；`TestRefreshReportsRuntimeReconcileFailure` 注入 reload 错误，修复前实测 LastError 为空，修复后通过。快照已提交但重载失败时不伪装为 DB 回滚。 |
| R1 / P1 | `internal/boxmgr/reload_rebase.go`：映射保存切片元素指针；先 append 新节点触发扩容，再合并旧节点事实，会更新旧底层数组。违反重载窗口事实保留语义。 | 映射改存索引，写入时定位当前数组；新增节点同步入索引。`TestRebaseKeepsFactsAfterAppendingWindowCreatedNode` 固定 len=cap=1，修复前实测地区 us/标签 old 未更新，修复后通过。 |

S2 的边界：只针对本进程唯一 subscription manager 的生产写入口。网络下载、运行时重载、refresh observer 均在 `resultMu` 外；锁内只校验、准备并提交本地数据。重启没有旧进程的在途结果，因此无需新增数据库版本列。外部直接改库和多个 manager 共用数据库不在此门禁保证内。仅修改名称不会废弃合法结果；显式手动刷新禁用订阅的原有能力保留。

## 健康事件：已复现但本批不改变投递机制

**H1 / P1：4096 容量溢出丢弃会改变健康状态机语义。**

调用链：`internal/monitor/health_applier.go` 投影后发布 → `internal/monitor/health_event_hub.go` 异步队列 → `internal/outbound/pool/pool.go` 的 `handleHealthResult` → `group.RecordHealthSuccess` / `RecordGroupHealthFailure`。生产健康订阅调用点为 pool；全局订阅接口另有测试调用。不能把消费者当成仅刷新 UI 的观察者。

`TestHealthEventOverflowLosesTransitionsAndSnapshotMarker` 用 `testing/synctest` 和 channel 屏障暂停回调，填满 4096 个待处理事件，再发布成功/失败和 SnapshotOnly 标记。测试直接连接 hub 到与 pool 相同的 group 状态入口（不是整个 sing-box 压测）：

- 已接受的事件保持 FIFO；后两条事件均丢失，Dropped=2。
- 节点已 EVICTED、恢复进度 1/2 时，丢失成功事件使其仍停在 1/2；排空后实际投递成功则恢复 ALIVE。
- 丢失失败事件不会重置该恢复进度；排空后实际投递失败则重置为 0。由消费代码可知失败窗口同样依赖事件，而非仅依赖最新快照。
- SnapshotOnly 标记也被丢弃，不能当作可靠投递屏障；SnapshotOnly 本身不得推进计数（HEALTH-001）。

`TestHealthEventCloseDiscardsPendingButAllowsRunningCallback` 确认关闭清除排队事件、拒绝后续入队，但已开始的回调可以在 close 返回后完成。`Dropped()` 仅有内部方法，当前没有生产监控调用；不能称为已接入告警。

上述测试是**未解决缺陷的特征测试**，明确断言当前损失，不表示接受此行为。后续重设计时应替换为无损断言。本批纠正了“lossless”“同步回调”和“丢弃仅多刷新一次”的错误注释，未扩大队列或恢复无界队列。

### 独立改造建议（2.2.6 已实施，见 [健康事件日志提案](changes/health-event-log/proposal.md)）

推荐将权威状态转移与可合并的快照通知分开：

1. 健康测量的持久化提交同时记录可重放的有序事件，携带 NodeID、身份 revision、runtime tag/generation、ProbeID、单调序号；最新一条 receipt 不能替代完整事件日志。
2. 关键 group 投影按序消费，带明确游标及幂等应用；重放不得重复增加恢复计数。规定重启与分组重建时游标/状态如何一致恢复，再设计追加 migration。
3. 内存通知仅负责唤醒，可合并；消费者从日志补读，不能因通知满而丢事实。SnapshotOnly 走独立可合并的“需要刷新”标记。
4. 明确日志容量/保留窗口与背压：达到容量时停止接收新的测量提交并显式报错，不能悄悄丢旧事实。退出要取消等待、定义在途回调与排空边界，禁止持 manager/pool 锁等待。
5. 接入积压、最老事件年龄、失败、重放和消费进度指标。验收包括 >4096、慢/失效消费者、取消、重启重放、身份替换、跨代、失败与连续恢复交错；无损性与内存上界必须同时成立。

此建议涉及持久化接口、迁移及状态恢复，不混入本批局部修复；具体日志/游标事务设计见 [健康事件日志设计](changes/health-event-log/design.md)（Requirement HEALTH-002..007）。

## 热重载核查与未验证边界

- 增量提交：静态追踪 `PrepareUpdate → Commit → reconcileMembersWithPolicyLocked`；最后一个可失败的成员解析在政策修改前完成，`ReconcileSilent` 后发布 pool 快照，解锁后才 `NotifyEvents`。新增 `TestCommitPublishesSnapshotBeforeGroupNotification` 从实际 Commit 回调读取快照；既有 `TestCapacityCommitMissingMemberLeavesPolicyAndStateUnchanged` 验证失败不改政策/分组状态。
- 旧代隔离：`ProjectHealthOutcome` 检查 registry tag、NodeID 映射、receipt/时间；新增 `TestRetiredRuntimeHealthResultCannotAdvanceNewGeneration` 验证旧票据不能推进新代状态。既有身份替换、A→B→A、取消和迟到成功测试一并回归，对应 IDENTITY-001 / HEALTH-001。
- 全量失败：`TestFullReloadFailureRestoresServingBase` 用无效日志配置触发新实例创建失败，验证旧配置和基础/分组监听恢复；既有 forced group rollback 用例覆盖保留监听。未穷举端口冲突十次耗尽、rollback 本身失败等所有故障注入。
- idle：新增 `TestIdleWakeRecreatesUnchangedGroupListener` 验证零节点进入 idle 后，以相同分组配置唤醒会重建监听。
- 事实合并：原有窗口事实测试与 R1 回归均通过；这些是合并函数测试，不能据此声称任意并发 CRUD 下配置、已构建 outbound 和 store 均线性一致。

仍需后续核查的静态风险（本批未完成故障复现，不记为已修复）：

- `UpdateSubscriptionSettings` 与 `ResetSubscriptionNodes` 是两个 DB 操作，后者失败时前者可能已提交；进程内门禁不提供崩溃原子性。
- 健康事件一旦已进入 FIFO，pool 可按 NodeID 映射到当前 tag；需进一步验证跨代排队事件，不能用投影入口的旧代测试替代消费者代际验收。
- 全量 box 构建后才 rebase 配置；新节点事实保留不自动证明已经构建的 outbound 拓扑同步完成，需要专项并发集成验证。

## 验证记录

- 四项产品修复均先运行失败回归，再修改产品代码并确认对应回归通过；新增并发用例用 channel/synctest，无新增 sleep。
- 前端 `npm run lint`、`npm run build`：2.2.5 通过；Vite 仍提示现有单包体积 >500 kB，本批未做前端优化。
- `go test ./internal/subscription ./internal/monitor ./internal/boxmgr ./internal/outbound/pool ./internal/group ./internal/probe ./internal/store`：通过。
- `go test ./...`：通过（含受影响包缓存结果）；未遇到 Windows 应用控制拦截，无需 `-c` 绕行。
- `go build -o <临时 exe> ./cmd/easy_proxies`：主程序实际链接通过，临时二进制已删除；此项为本机默认 tags 构建，不代表 Linux 全 tags 发布矩阵。
- race **未完成**：默认 `CGO_ENABLED=0`，直接执行报 requires cgo；设置 `CGO_ENABLED=1` 后报 C compiler gcc not found。没有将普通测试冒充 race 验证，没有修改依赖或安装工具链。
- 未执行生产规模压测、跨进程/崩溃注入、部署或发布；不据此宣称规模认证或全部风险清零。
