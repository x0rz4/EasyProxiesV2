# 写路径审计清单

状态：对 `71d8745` 的静态盘点，**不是“生产无旁路”的验收结论**。目标与约束见 [当前 spec](specs/proxy-pool/spec.md)，整改任务见 [阶段 1 任务](changes/scale-proxy-pool/tasks.md)。

P1 首批新增调用者分类及代码收敛见 [健康边界报告](changes/scale-proxy-pool/p1-health-boundaries.md)：已删除无调用者的 MarkAvailable，修正持久化无目标路径和单节点取消/超时；以下 A1 等条目保留为原始发现，其余入口与规模问题仍需继续验收。

## 权威链与允许写入者

| 事实/状态 | 允许入口及顺序 | 禁止旁路 / 待核查边界 |
| --- | --- | --- |
| 节点身份与 revision | 导入/订阅 → Store 身份事务；同身份保留历史，替换清健康/地区证据 | 显示名当身份、普通更新覆盖地区投影、先通知后提交 |
| 地区 | `store.BeginLandingProbe` / `store.ApplyLandingResult`，经 Applier 和 nodeloc 判证据 | URI host 推国家；失败清空已知地区；解锁服务地区直接当出口地区 |
| 健康测量 | `monitor.beginHealthResult/commitHealthResult` → `probe.Applier` → `ProjectHealthOutcome` → 健康事件 | diagnostic origin 携带 Health；事务失败仍发事件；投影重试重复恢复 |
| 初始/继承状态 | `node_registry` 注册/身份迁移；`RestorePersistedHealth` 标记 provisional、待初检 | 继承健康直接计入容量；旧 tag 写入新 generation |
| 流量失败与黑名单 | pool 的真实拨号失败 → shared state/EntryHandle → `group.RecordFailure` 等既有路径 | 将流量成功或诊断成功当测活收据；健康清除责任不明 |
| 分组剔除/恢复 | `group.Runtime` 失败窗口与合法健康事件；显式人工恢复是独立管理命令 | Healer/HTTP/标签代码直接改成员 Evicted、streak 或 failure history |
| 成员资格 | 共用 groupmember/nodeselect；nodeview 与预览复用规则 | Runtime 健康扩大候选；预览和实际构建自写两套条件 |
| active/standby 和快照 | `boxmgr.ApplyHealingSelection` 重 Plan → 增量 PrepareUpdate/Commit → NotifyEvents | Healer 直接写 pool；快照发布前广播；standby 转换关长连接 |
| 自愈动作 | Healer planned 审计成功 → HealingBackend 二次授权 → 执行 → 结果审计 | 先请求后记 planned；改配置后仍信任旧 plan；审计失败被吞 |
| 订阅补货 | `app.healingBackend` → `subscription.RefreshForHealing` → busy gate / LastAttempt → 网络 → 身份事务 | 绕过共享预算、先联网后写尝试时间、禁用订阅仍刷新 |

## 当前发现与待决事项

| ID | 证据 | 处置与任务 |
| --- | --- | --- |
| A1 | `EntryHandle.MarkAvailable`、`MarkInitialCheckDone`、`RecordSuccessWithLatency`、`Blacklist/ClearBlacklist` 仍公开 | P1.1 必须查全部调用者、对生产/测试/生命周期分类；不能仅凭定义就断言已旁路或没有旁路 |
| A2 | `SetProbeStore(nil)` 保留无 Store 内存路径 | P1.1 验证生产 app 装配总是注入 Store；内存兼容测试不作为持久化路径验收 |
| A3 | `HealthResultEvent`、`GroupStateEvent` 已有，`TopologyChanged` 没有统一类型 | P1.2 先明确现有方法调用顺序；如新增事件则另写 schema/幂等/重放规格 |
| A4 | `healthEventSubscription.queue` 为无界 FIFO | P2.3 慢消费者压测并设计有界投递；保留每次合法失败/成功顺序和恢复计数 |
| A5 | Healer.current 与执行端反复调用全量 `HealingInputs` | P2.4 profile 后决定按组读取/缓存；每次执行不得省略新政策验证 |
| A6 | 审计按状态/动作详情去重，且只在追加时清理 | P3/P4 分开动作计数与审计；补独立清理，未来追踪字段需要 migration |
| A7 | 关闭执行时已在途动作没有统一取消保证 | P1.4/P3.5 明确线性化点与 UI 语义，区分未来动作、在途请求和正常周期 |

## 可复现检索

以下为候选点收集，结果需要人工检查；命名不同的旁路可能漏检，不能把 rg 无输出当安全证明。

```bash
# 可用性、初检与健康时间的直接赋值
rg -n '(available|initialCheckDone|healthCheckedAt|blacklist)\s*=' internal -g '*.go' -g '!**/*_test.go'

# 定义和生产调用都检查，再对照测试调用
rg -n 'MarkAvailable|MarkInitialCheckDone|RecordSuccessWithLatency|ClearBlacklist|RestorePersistedHealth|SetProbeStore' internal -g '*.go'
rg -n 'RecordFailure|RecordHealthSuccess|RecordGroupHealthFailure|RestoreMember|ReconcileSilent|NotifyEvents' internal -g '*.go' -g '!**/*_test.go'

# 结果、地区和审计写入者
rg -n 'HealthProbe|ManualDiagnostic|ProjectHealthOutcome|commitHealthResult|publishHealthResult' internal -g '*.go' -g '!**/*_test.go'
rg -n 'ApplyLandingResult|UpdateNodeLocation|AppendGroupHealEvent|MarkSubscriptionAttempt' internal -g '*.go' -g '!**/*_test.go'

# 候选选择、快照与外部动作
rg -n 'groupMemberNodes|CapacitySelection|PrepareUpdate|Commit|ApplyHealingSelection|RefreshForHealing' internal -g '*.go' -g '!**/*_test.go'
```

P1.1 的最终产物必须逐行给出“调用者 → 入口 → 改写字段 → 身份/代际门禁 → 允许原因/整改任务 → 回归测试”，不是只提交 grep 输出。

## 评审清单

- [ ] 变更引用 Requirement 与任务 ID，并区分事实、规划和执行结果。
- [ ] 诊断不能写健康/剔除/恢复；真实失败与人工管理命令归属明确。
- [ ] 身份 revision、runtime generation、ProbeID 和收据幂等全部保留。
- [ ] 候选集使用统一规则；运行态证据不能放宽候选资格。
- [ ] 自愈 planned 审计失败阻断动作；执行端验证当前政策、规则和预算。
- [ ] dry-run 允许审计但没有额外网络/成员动作；关闭语义说明在途请求。
- [ ] 拓扑和活动选择使用既有增量提交，通知在快照可见后发生。
- [ ] 锁不跨网络；取消释放 lease；有界方案保留失败/恢复语义。
- [ ] 配置默认安全；迁移只追加；API/UI/持久化/热更字段一致。
- [ ] 测试、race、压测和人工演练按实际结果记录，未执行明确标出。
