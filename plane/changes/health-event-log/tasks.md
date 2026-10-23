# 任务与进度

图例：`[x]` 已实现且有证据；`[ ]` 未开始。证据写测试名或命令，不写“已有某函数”。

## 阶段 1：规格与迁移

- [x] `plane/changes/health-event-log/{proposal,design}.md` 定义 HEALTH-002..007 与边界（2026-10-23）
- [x] migration v24：`health_events`、`health_event_cursors`（`internal/store/migrations.go`）

## 阶段 2：写入路径

- [x] `store`：`AppendHealthEvent` / `ListHealthEventsAfter` / `HealthEventJournalStats` / `GetHealthEventCursor` / `PutHealthEventCursor` / `PruneHealthEvents`（`internal/store/health_events.go`）
- [x] 容量上限与 `ErrHealthEventJournalFull`
- [x] `probe.Applier.Apply` 同事务追加日志行，`Outcome.Seq`
- [x] 容量满时测量整体失败且不落事实

## 阶段 3：投递路径

- [x] `healthEventHub` 改为游标投递 + 可合并刷新标志 + 持久消费者游标（`internal/monitor/health_event_hub.go`）
- [x] `Manager.SubscribeDurableHealthResults` / `HealthEventStats` / 后台清理（`internal/monitor/manager.go`）
- [x] 后台清理任务（retention 30m，周期 1m）
- [x] pool 以 `pool:<groupID>` 持久订阅，pool 事件携带 `Seq`/`ProbeID`
- [x] `group` 按 ProbeID 环形去重（64），重放不重复推进恢复计数
- [x] 无 store 的内存模式保留兼容直投，注释标明非生产路径

## 阶段 4：可观测

- [x] `GET /api/operations/health-events` 会话认证统计接口
- [x] 路由契约测试覆盖新端点

## 阶段 5b：复核修正（2026-10-23）

阶段 2–4 合入后复核发现三处与 design 不符，已修并补回归：

- [x] 清理基准从"全表 `MIN(游标)`"改为"当前注册消费者"，`PruneHealthEvents` 增加 `activeConsumers` 参数 → `TestPruneHealthEventsIgnoresInactiveConsumerCursor`（旧逻辑实测只释放 3/10）
- [x] 已消费行保留窗口 30 分钟 → 5 分钟；容量 8192 下可持续约 27 行/秒。旧值实测约 4.5 行/秒即触顶 → `TestHealthEventJournalSustainsConfiguredProbeRate`
- [x] 游标写失败真正停止投递（`cursorBlocked`），不再只是记一次 `cursor_failures` → `TestHealthEventCursorFailureStopsDelivery`（旧逻辑实测带未落盘游标投递 640 条，去重环仅 64）

## 阶段 5：验收

- [x] 无损/顺序/重启续读/首注册尾部/幂等/事务性/背压/清理边界/关闭 用例（见 `design.md` §9）
- [x] 受影响包全部通过：store / probe / group / monitor / outbound/pool / httpapi / boxmgr / app / subscription（逐包 `-p 1` 串行执行）
- [ ] race：本机 `CGO_ENABLED=0`，无 gcc，未执行（与 2026-10-23 审计口径一致）
- [ ] 未受影响的其余包（builder/nodefacts/nodetag/controlplane/netx/nodecodec/ipquality/nodedetect/nodeview/runtimetag 等）本轮被 Windows 应用控制策略拦截，未取得本轮证据；其中 builder/nodetag 曾单次通过，其余未验证
- [ ] 生产规模压测、跨进程崩溃注入：不在本批

## 遗留（转后续）

- 崩溃窗口内一批（≤64）事件可能重复应用；反向也可能因分组状态异步落库而丢一条状态记录 → 需把游标推进与 `group_node_states` 写入同一事务
- 审计“仍需后续核查的静态风险”三条未动
