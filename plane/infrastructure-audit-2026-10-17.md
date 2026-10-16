# EasyProxiesV2 代理池基础设施架构评估与优化修复计划

> 评估日期：2026-10-17 ｜ 方法：四路并行深度代码审查（数据面 / 生命周期 / 控制面与存储 / 订阅与标签链路），全部结论附文件:行号证据。
> 范围：`internal/outbound/pool`、`internal/group`、`internal/groupmember`、`internal/runtimetag`、`internal/boxmgr`、`internal/builder`、`internal/app`、`internal/monitor`、`internal/probe`、`internal/store`、`internal/subscription`、`internal/nodetag`、`internal/nodefacts`、`internal/config`、`internal/nodecodec`、`internal/jsonx`。

---

## 一、总体结论

架构分层（不可变快照选路 / 组状态机 / 两阶段提交 / 单写 SQLite / 蓝绿+增量双路重载）设计整体是认真的，多处关键协议经推演正确：

- 快照读侧 pin 协议（`readers.Add` → 复查 `accepting` → `releaseOnce`）无丢失/双重释放；
- 锁序一致（`p.mu → runtime.mu` 无反向路径），未发现死锁；
- `PrepareUpdate/Commit` 两阶段提交把解析放在最后可失败步骤，无半提交；
- SQLite 单写连接 + WAL + NORMAL 规避了进程内写锁争用；
- 组版 retirement ticket（cancel/once/重挂回）设计严谨。

但审查发现 **13 个高危问题**（可用性、正确性、探测风暴），以及一批在 5000+ 节点规模下会线性放大的写放大与锁持有热点。最严重的三类：

1. **拨号失败记账口径错误**——调用方取消/超时被计为节点失败，造成误拉黑、误踢出（数据面）。
2. **生命周期静默故障**——idle 恢复后组监听器永不重建、端口重试耗尽后死实例被设为 currentBox、回滚失败后管理面永久锁死（boxmgr）。
3. **探测风暴链**——ProbeID 墙钟碰撞 → 结果被判 duplicate 丢弃 → `initialCheckDone` 无法置位 → 收敛循环无限重探（probe/store/monitor 三层叠加）。

---

## 二、关键问题清单（按严重程度）

### 2.1 高危（P0，直接影响可用性/正确性）

| # | 问题 | 证据 | 影响 |
|---|------|------|------|
| H-1 | 调用方取消/超时仍 `recordFailure`：3 次即拉黑 24h、组路径计入滑动窗 | `pool.go:1057-1064, 1097-1104`（`trackedConn.recordIOError` 已正确过滤，拨号路径没有） | 客户端断开风暴 → 误拉黑/误踢出健康节点 |
| H-2 | `Close()` 后 `acquireSnapshot` 无限 `runtime.Gosched()` 忙等（`p.closed` 无检查） | `pool.go:472-485, 593-609` | 排空/重载后到达的拨号烧 CPU，永不返回 |
| H-3 | 组模式每次拨号成功无条件 `SetCurrentTag` → 组级互斥锁 + 同步事件扇出（含持久化 observer，可能写 SQLite） | `pool.go:1128/1143 → runtime.go:443-468, 192-208` | 所有连接在组 `runtime.mu` 串行；数据面每连接一次磁盘写风险 |
| H-4 | ProbeID 用墙上时钟 `UnixNano` 生成，Windows 粒度 ≈1ms（可达 15.6ms），碰撞后：health 结果被判 `duplicate` 整体作废（无日志）；landing 观测被 `invalid_replay`/`ambiguous_order` 拒绝；同模式第二处在 `manual:` 探测 | `store/landing_probe.go:26-30`、`store/sqlite.go:436`、`probe/applier.go:186-199`、`nodeloc/state.go:103-115` | 合法探测结果静默丢弃；启动/手动探测紧邻定时探测时丢结果 |
| H-5 | 启动收敛对"永远无法 commit 的节点"无限重探：`beginHealthResult` 失败不 commit → `initialCheckDone` 永假 → 收敛循环重新入队，每轮 2 次探测无限循环；被 H-4 直接放大 | `probe_scheduler.go:708-736, 492-496, 592-607`；触发条件如 `health_applier.go:36-38` | 探测风暴：500ms+探测时长的持续循环，还占住租约拖累周期轮 |
| H-6 | idle→resume 后组监听器永不重建：`enterIdle` 关全部组 runtime，但 reload 时 `groupRuntimeTopologyEqual` 判等成立 → `continue`，`slot.box` 永久 nil | `manager.go:525, 2355-2399, 2383` | 空闲唤醒后组订阅入口全部 "stopped"，需手工触碰 |
| H-7 | 组重建失败置 `error` 后无自动重试：重试逻辑只挑 `"degraded"`，且拓扑判等继续跳过 | `manager.go:1151-1178, 490` | 组持续下线直到人工编辑 |
| H-8 | 端口冲突重试循环耗尽后"跌落"继续执行：`instance` 是已 Close 且 Start 失败的死实例，仍被设为 `currentBox` 并继续启动组，无错误上报 | `manager.go:177-199, 225, 362-382` | 数据面静默全下线 |
| H-9 | 回滚失败后管理面永久锁死：`rollbackToOldConfig` 失败仅打日志，`currentBox=nil` 未恢复，此后所有 reload 报 "manager not started" | `manager.go:902-910, 332, 298-301` | 只能重启进程 |
| H-10 | reload 快照窗口内的事实变更被整体回退：reload（可达数十秒）期间 `ApplyGroupMembershipChanges`/节点 CRUD 写入的新 Tags/Region 在提交时被旧快照覆盖，运行时拓扑回退 | `manager.go:418-420 vs 1572-1591, 2008, 2068` | retag/落地国家变更丢失，直到下次 reload |
| H-11 | 订阅刷新后无条件全量 runtime 重载：`CommitSnapshot` 前后无 membership diff，即使全部拉取失败也执行 `reconcileRuntime` → 全量重建 | `subscription/manager.go:577-636, 388` | 每订阅到期（默认 1h）一次全量 sing-box 重建，与 H-6/H-10 叠加 |
| H-12 | `ReplaceAutoNodeTags` 无 diff 全量落库：每节点 DELETE+逐 tag INSERT+UPDATE projection，单事务 ≈15000 条 SQL/5000 节点；`projectionChanged` 已算出 changed 集合却只用于 notify | `nodetag/service.go:173-186`、`sqlite_tags.go:512-530, 589-599` | 10min sweep + 防抖批 + overflow 全量反复付出；单写连接长事务阻塞一切并发写 |
| H-13 | 正则规则每次求值重新 `regexp.Compile`（保存时已编译校验过） | `nodefacts/evaluate.go:316-321` | 5000 节点 × 1 条正则 = 每次重算 5000 次编译，CPU 放大一个数量级以上 |

### 2.2 中危（P1，稳定性与性能）

**数据面（pool/group）**

| # | 问题 | 证据 |
|---|------|------|
| M-1 | 组模式每次失败触发 ≥2 次全量快照发布（`recordFailure` + `handleGroupState`），每次分配 1 快照+3 map+2~3 切片并持 `p.mu` 逐成员查 monitor/group | `pool.go:1532-1548, 1348-1376, 503-562` |
| M-2 | 非组路径失败计数无时间窗，`Options.FailureWindow` 被忽略，三天零星 3 次失败即拉黑 24h | `shared_state.go:86-108` |
| M-3 | 组路径同秒拨号失败不去重（探测路径有），3 个并发失败 1 秒内即 EVICTED+全局传播 | `runtime.go:470-519` vs `623-637` |
| M-4 | `globalEvictedNodes` 永不清理：节点被移出所有组后条目永久滞留，重新添加即强制 evicted | `runtime.go:134, 268, 515, 355-356` |
| M-5 | `notifyGroupState` 同步扇出无背压/panic 隔离，慢订阅者（SQLite）阻塞拨号失败路径 | `runtime.go:192-208` |
| M-6 | `p.options`/`p.mode` 形式化 data race（Commit 写 vs 回退读） | `pool.go:941-953 vs 1575-1581, 1229-1231` |
| M-7 | 长连接 pin 整份快照：被退役成员 `Drained()` 长期为 false，`runtimeByTag` 代际堆积 | `pool.go:1068-1069, 169-173` |

**生命周期（boxmgr/builder/app）**

| # | 问题 | 证据 |
|---|------|------|
| M-8 | `applyConfigSettings` 无锁写 `drainTimeout/minAvailableNodes`，多处无锁读，race detector 可稳定命中 | `manager.go:414, 784, 1818-1828, 1366` |
| M-9 | Start 路径无锁原地修改已发布的共享 `m.cfg`（Region/Country/Port） | `landing_location.go:91, 232`、`manager.go:2501` |
| M-10 | reload 持全局写锁做 2 次 YAML marshal + 2 次全配置深克隆（O(N) 序列化） | `manager.go:297-333` |
| M-11 | base 版 `drainRetiredMember` 无取消通道，ref 泄漏即 goroutine+outbound 永久滞留 | `manager.go:838-855` |
| M-12 | `waitForConnectionsToDrain` 统计全局连接（含组 pool/探测连接），任一长连接即白等满 drainTimeout | `shared_state.go:64-71` + `manager.go:346` |
| M-13 | 回滚恢复不完整：runtimeGeneration/monitor generation/config listeners/SweepStaleNodes 均未恢复 | `manager.go:896-916` |
| M-14 | `enterIdle` 不走 `reloadMu`，与并发 Reload 竞争 | `manager.go:2277, 2355` |
| M-15 | `BuildGroupVersion` 为单组解析全部 N 节点（O(G×N)）；`printProxyLinks` 每次 reload 把全部节点明文凭据打进日志（凭据泄漏+O(N) 开销） | `builder.go:420-434, 144, 1254-1308` |
| M-16 | `groupStatePersister.Observe` 缓冲 256 满则阻塞，背压传导进组状态路径并卡住 Close | `app.go:240-247` |

**控制面与存储（monitor/probe/store）**

| # | 问题 | 证据 |
|---|------|------|
| M-17 | detached 探测 `runningAttempts` 条目在 outbound 无视 ctx 时永不回收 → 该节点所有后续探测 "previous probe call is still running" | `probe_scheduler.go:534-590` |
| M-18 | 健康提交 5s 预算 vs 单写连接排队（大订阅快照合并逐行 upsert 2 万+ 条、身份 reconcile 逐候选点查、批量 stats），排队击穿即静默丢结果并喂给 H-5 | `health_applier.go:58`、`sqlite.go:999-1027, 866-970, 1170-1229` |
| M-19 | `CleanupTimeline` 全表窗口函数 DELETE，`node_timeline` 无按时间清理线性增长，持写锁时长线性上升 | `sqlite.go:1332-1346` |
| M-20 | `decodeJSON` 无请求体上限（auth/settings/groups/订阅 create、`/api/public/subscriptions` 等），内存 DoS 面 | `server.go:2584-2595`（仅 import 12MB、tag 64KB 有限制） |
| M-21 | health event hub 每订阅无界 FIFO，慢消费者内存无界增长 | `health_event_hub.go:8-40` |
| M-22 | `flushStatsToStore` name 兜底匹配可错配节点（重名时 A 的统计写进 B）；进程 kill 丢最近 ≤30s 增量 | `app.go:489-562` |
| M-23 | 会话认证路径 `context.Background()` 直连 DB 无超时；登录无失败速率限制；会话数无上限；Cookie `Secure:false`；API Key 走 URL query 入日志 | `server.go:4019-4098, 1899, 1920, 1752` |
| M-24 | SSE `onResult` 同步网络写在调度 actor 内，慢客户端冻结调度器（缓冲填满 → 周期调度与 reload 的 enqueueInitial 全卡） | `probe_scheduler.go:517-519, 431-433`、`server.go:1319-1345` |

**订阅/标签/解析**

| # | 问题 | 证据 |
|---|------|------|
| M-25 | ETag/Last-Modified 存而不发，上游 304 被判失败；条件请求可省整次解析+落库 | `subscription/manager.go:514-535, 550` |
| M-26 | 刷新统计做两次全量 `ListNodes`（含 canonical_json 大字段）纯为计数 | `subscription/manager.go:442-503` |
| M-27 | 防抖队列 flush 失败即丢事件（仅日志，无重排），只能等 10min sweep；持续事件流下 timer 每事件重置退化为 sweep | `nodetag/queue.go:239-266, 207-215` |
| M-28 | notify 同步执行在 queue worker 内：写锁内 2×O(n) cfg Clone + 逐组重建，期间重算队列停摆 | `nodetag/service.go:187-189`、`app.go:143-148`、`manager.go:1566-1612` |
| M-29 | `upsertSubscriptionNodes` 无条件 bump `updated_at`（无内容变化守卫），污染身份对账/订阅 diff 语义，全表误判变更 | `sqlite.go:1006-1012` |
| M-30 | `LoadForReload` 边界：`database_path` 变更进 newCfg 但 Store 仍是旧库；reload 模式双重端口探测；YAML 非法组在 reload 时报错（即使被 store 覆盖不生效） | `config.go:655-661, 636-686, 757-771`、`manager.go:2259-2264` |
| M-31 | Clash→URI 丢 vmess `alterId`/`cipher`、hysteria2 `obfs/up/down`：alterId≠0 节点身份错误且不可用 | `config.go:1743-1774, 1883-1900` |
| M-32 | 订阅 auto/base64 解析丢弃行级 issues，部分行失败对用户不可见 | `config.go:1046-1049` |

### 2.3 低危（P2，卫生项，摘要）

- 死代码：`pickMember`/`wrapConn`/`memberByTag`/`isBase64` 等（pool.go:1148/1597/1287、config.go:1340）。
- `runtimetag.Format` 大小写折叠碰撞 + 仅 64bit 身份（tag.go:27）；http 节点 path 转义差异产生重复身份（identity.go:142-144）。
- `groupSlots` 永不清理已删组（manager.go:962）；`applyCapacitySelection` map 无序截断（pool.go:2005）。
- `Healer.Tick` 持锁网络 I/O + `runGroup` N+1 `HealingInputs`（healer_coordinator.go:62-170）。
- `recordAPIKeyUse` 每请求 spawn goroutine 无界（server.go:1791）；`Start()` 100ms 假同步探测（server.go:1010）。
- Sonic：`ConfigDefault` 不排序 map 键——未来若有人拿 `jsonx.Marshal` 输出做哈希/比较会踩坑；非 amd64/arm64 回退 encoding/json 输出不一致；W^X 加固容器下 JIT 崩溃风险。
- 进程级陷阱（工作记忆已录）：`internal/monitor/assets` embed 与 `go build` 并行必炸；CRLF 工作区需去 CR 后再 gofmt。

---

## 三、优先级排序与依据

排序依据：① 数据正确性/可用性 > ② 静默故障（无日志不可观测）> ③ 放大效应（规模线性放大）> ④ 修复成本。

```
P0（立即，正确性与可用性，~1-2 天/项）
  1. H-4  ProbeID 单调序号          ——一处小改，消除整条探测风暴链的根因
  2. H-1  取消/超时不记账            ——数据正确性，直接决定黑名单/踢出可信度
  3. H-2  Close 后忙等              ——一处守卫，消除 CPU 烧死
  4. H-6/H-7/H-8/H-9  boxmgr 四连  ——组监听器恢复、error 重试、死实例、锁死
  5. H-5  收敛退避                  ——风暴止损（依赖 1 先行）
  6. H-13 正则编译缓存              ——几行改动，收益极大

P1（本周内，稳定性与规模化，~2-5 天/项）
  7. H-3/M-1/M-3 组路径事件与快照发布收敛（合并通知、失败去重、发布节流）
  8. H-10/H-9 reload 提交前 rebase（以 m.cfg 当前值二次 diff）+ 回滚完整性
  9. H-12/H-13+H-2(订阅) 写路径瘦身：ReplaceAutoNodeTags 只写 changed、
     CommitSnapshot 批量/prepared、订阅 diff 无变化跳过重载、updated_at 守卫
 10. M-24 SSE 解耦 + M-20 body 上限中间件 + M-21 hub 有界
 11. M-18/M-19 存储层：CleanupTimeline 按时间窗删除、健康提交预算与排队治理
 12. H-11+M-25 订阅条件请求（ETag/304）与 diff 短路

P2（迭代内，卫生与加固）
  M-2/M-4/M-5/M-6/M-7（数据面语义统一与泄漏）、M-8/M-9/M-10/M-11/M-12/M-14/M-15
  （锁纪律与凭据泄漏）、M-17/M-22/M-23（探测回收/统计错配/会话加固）、
  M-27~M-32、P2 卫生项清单
```

---

## 四、修复实施步骤与预期效果

### 阶段一：P0 正确性止血

**1. ProbeID 追加进程内单调序号**
- 步骤：`internal/store/landing_probe.go` 与 `internal/store/sqlite.go:436` 引入包级 `atomic.Uint64`，ProbeID 改为 `fmt.Sprintf("landing:%d:%d:%d", nodeID, now.UnixNano(), seq)`（`manual:` 同理）。重试路径复用同一 ProbeID 的幂等契约不变（序号只在"新建观测"时递增）。
- 验证：单测并发同节点同 tick 两次 Begin → ProbeID 必不相同；复跑 `TestStandaloneProbeCancellationDoesNotChangeHealth` 与 `TestProbeBatchRetriesOnceAndCommitsOneHealthResult`（这两个测试的偶发失败应消失）。
- 预期：合法探测结果不再被 duplicate/invalid_replay 作废；消除 H-5 的主要放大器。

**2. 拨号取消/超时不记账**
- 步骤：`pool.go` 的 `DialContext`/`ListenPacket` 失败分支，在 `recordFailure` 前判断 `errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)` 且 `ctx.Err() != nil` 则跳过（与 `trackedConn.recordIOError` 的过滤口径对齐）；仅当超时来自拨号自身 deadline 时才记账。
- 预期：客户端断开/上层超时不再污染黑名单与组滑动窗。

**3. Close 后快照获取快速失败**
- 步骤：`acquireSnapshot` 循环与 `loadSnapshot` 检查 `p.closed`，返回 `net.ErrClosed`。
- 预期：排空/重载窗口内到达的拨号立即失败而非烧 CPU。

**4. boxmgr 生命周期四修**
- H-6：`syncGroupRuntimesAfterBaseReload` 跳过条件追加 `slot.box == nil`（或 idle 恢复路径强制 applyMode=ForceNoRollback）。
- H-7：重试挑选条件 `!= "degraded"` 改为 `Status == "degraded" || Status == "error"`。
- H-8：端口重试循环耗尽后返回错误（或至少检查最后实例 Start 成功），杜绝死实例提交为 currentBox。
- H-9：`rollbackToOldConfig` 失败时保留可观测的 `panic` 级告警并置 `Status:"error"` 全局标志，允许带降级状态继续 reload 尝试而非永久拒绝。
- 预期：idle 唤醒组入口自愈；组重建失败可自动恢复；不再静默全下线/永久锁死。

**5. 收敛循环退避（依赖 1）**
- 步骤：`refreshInitialQueue` 连续 N 轮（如 3）无新增 commit 时退出收敛并告警；`beginHealthResult` 失败的节点进入指数退避。
- 预期：异常节点不再驱动无限重探。

**6. 正则编译缓存**
- 步骤：`nodefacts` 规则编译期（`condition.go` 已有校验点）缓存 `*regexp.Regexp`（规则对象持有或 sync.Map keyed by pattern）。
- 预期：重算期 CPU 由 O(N×compile) 降为 O(N×match)。

### 阶段二：P1 稳定性与规模化

**7. 组路径事件与发布收敛**：`SetCurrentTag` 仅在 fixed/lowest_latency 同步（sequential/balance/random 的 currentTag 无持久语义）或做 1s 合并通知；`RecordFailure` 补同秒去重（对齐 `RecordGroupHealthFailure`）；组模式失败只走一条发布路径（去掉 `recordFailure` 内的 `rebuildCandidatesNow`，统一由 StateChanged 触发并节流）。预期：失败风暴下快照发布次数由 2×/失败降为 ≤1×/秒，组锁不再是每连接串行点。

**8. reload 提交前 rebase**：全量 reload 提交 `m.cfg = newCfg` 前以运行时当前 `m.cfg` 为基准做二次 diff，把窗口内新增的节点/事实变更合并进 newCfg 再提交；`rollbackToOldConfig` 补齐 runtimeGeneration/monitor/config listeners/SweepStaleNodes 恢复。预期：retag/落地检测/节点 CRUD 不再被重载窗口覆盖。

**9. 写路径瘦身（5000 节点专项）**
- `ReplaceAutoNodeTags` 增加 diff 参数，只写 `projectionChanged` 的 changed 子集（已有现成集合）。
- `upsertSubscriptionNodes` 改 prepared 批量 + `ON CONFLICT ... WHERE` 内容守卫；`reconcileSubscriptionLogicalNodes` 的逐候选点查改一次性 `WHERE identity_hash IN (...)` 批查。
- `refreshSubscription` 提交前后 diff membership，无变化跳过 `ReloadWithPortMap`（对齐 nodetag 链路的差异重建）。
- `updated_at` 加内容变化守卫。
- 预期：单次订阅刷新/10min sweep 的写事务量降 1-2 个数量级；单写连接不再被长事务占死，健康提交 5s 预算击穿率显著下降。

**10. 控制面背压治理**：SSE `onResult` 改投递有界 channel（满则丢/断开）；`decodeJSON` 统一包 `http.MaxBytesReader` 中间件（默认 1MB，import 保持 12MB）；health event hub 改有界+丢弃计数指标。预期：慢 UI 客户端不再能冻结探测调度器与重载路径。

**11. 存储层治理**：`CleanupTimeline` 改为按 `created_at < now-保留窗` 的索引删除并补索引；评估健康提交预算按排队深度自适应。预期：写锁持有时长有界，随时间不再恶化。

**12. 订阅条件请求**：拉取时带 `If-None-Match/If-Modified-Since`，304 视为"无变化"短路后续解析与落库。预期：上游未变更时整条链路（解析 3-4×body 内存峰值 + 2 万条 SQL + 全量重载）全部省略。

### 阶段三：P2 卫生加固（要点）

- 数据面语义统一：非组路径启用 `FailureWindow`；`globalEvictedNodes` 挂节点删除清理钩子；`notifyGroupState` 加 panic 隔离；长连接只 pin 被选成员；删除 `p.options` 回退读。
- 锁纪律：`applyConfigSettings`/Start 改写共享 cfg 为 Clone-后-替换；比较逻辑移出写锁。
- `printProxyLinks` 移除明文凭据输出（改脱敏摘要）；base 版 `drainRetiredMember` 补 cancel ticket；drain 统计口径改为仅本实例 pool。
- 会话加固：登录失败速率限制、会话数上限、Cookie Secure 开关、认证 DB 调用带超时。
- `LoadForReload`：`database_path` 变更显式拒绝并告警；Clash vmess 补 alterId/cipher 输出；订阅解析 issues 上浮到 UI。
- 死代码清理与 `groupSlots` 生命周期管理。

---

## 五、风险与实施注意

1. **构建顺序陷阱**（每次改动都适用）：先 `cd frontend && npm run build`，再 `go test ./...`；两者绝不并行（embed 清空竞态）。
2. **CRLF 工作区**：格式检查用 `tr -d '\r' < f > /tmp/x.go && gofmt -l /tmp/x.go`。
3. 阶段一第 4 项（boxmgr）改动面最大，建议每修一项独立 commit 并补对应回归测试（idle 恢复、error 重试、端口耗尽、回滚失败各一个）。
4. H-10 的 rebase 逻辑涉及 reload 提交事务性，需配合并发注入测试（reload 期间并发 retag/CRUD）。
5. 协议白名单唯一真源仍是 `nodecodec.ParseURI`，M-31 修复（Clash vmess 补字段）必须同步核对 identity 归一化不受影响。
6. 已知偶发失败测试（ProbeID/探测时序两类）在阶段一第 1 项落地后应转稳定——它们本身就是修复效果的验收基线。
