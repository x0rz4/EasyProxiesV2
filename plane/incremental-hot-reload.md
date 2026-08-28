# EasyProxies 增量热重载与双缓冲改造方案

> 状态：设计提案  
> 目标版本：分阶段实施，第一阶段仅覆盖纯 `pool` 节点变更  
> 相关代码：`internal/boxmgr/manager.go`、`internal/builder/builder.go`、`internal/outbound/pool/`

## 1. 背景与问题

当前 `Manager.Reload()` 会关闭并重建完整的 `box.Box`：

```text
等待活跃连接排空
→ oldBox.Close()
→ 固定等待 500ms
→ createBox(newCfg)
→ newBox.Start()
→ 替换 currentBox
```

等待排空期间旧 listener 仍然存在，因此它主要增加重载总耗时；真正的端口不可用窗口从 `oldBox.Close()` 开始，持续到新实例 `Start()` 成功。节点、DNS、outbound 和 inbound 越多，窗口越长。持续有新连接进入时，旧实例还可能一直无法排空，最终等满超时后仍然硬关闭剩余连接。

仓库中已有 `gracefulSwitch()` 和 `drainOldBox()`，但没有调用方。整实例蓝绿切换也无法直接解决问题，因为新旧实例不能同时绑定同一组入口端口。

sing-box v1.12.12 的 `InboundManager` 和 `OutboundManager` 支持运行时 `Create`/`Remove`，但不能直接把当前重载改成若干次 manager 调用：

- 自定义 `poolOutbound` 在启动时把 `Options.Members` 固化为 `p.members`，之后不会感知新注册的 outbound。
- pool 对所有成员声明了静态 `Dependencies()`；sing-box manager 会阻止移除仍被依赖的成员。
- 同 tag 调用 `OutboundManager.Create()` 会关闭旧 outbound，无法让旧连接自然排空。
- multi-port 的 inbound 到 outbound 映射固化在 sing-box route rules 中，Router 没有通用的运行时规则增删接口。
- `ResetSharedStateStore()` 会同时清空健康状态、连接计数和 group runtime，不能用于增量更新。

因此本方案将热切换边界放在 EasyProxies 自定义数据面：sing-box manager 负责运行组件的注册和生命周期，自定义 pool 通过不可变快照和原子指针决定新连接使用哪个运行实例。

## 2. 目标与非目标

### 2.1 目标

1. 节点增删改不再关闭整个 `box.Box`。
2. 未变化节点、listener 和已建立连接不受 reload 影响。
3. 新连接通过一次原子指针读取获得一致的路由成员快照，热路径不持 reload 锁。
4. 被替换或删除的 outbound 停止接收新连接后进入 draining，排空或超时后才关闭。
5. 失败发生在提交前时可以完整回滚；提交后的清理失败可重试且不影响新数据面。
6. 保留稳定端口，只有新增、删除或显式修改端口的节点才触发 bind/unbind。
7. 每次 reload 具备 generation、diff 数量、切换耗时和 drain 结果等可观测信息。

### 2.2 第一阶段非目标

- 不热更新 DNS、route、experimental 等 box 级配置。
- 不热更新 listener 地址、协议或认证。
- 不处理 group listener、group selector 和 group membership 的动态变化。
- 不保证跨进程重启保留 runtime generation。
- 不在第一阶段删除现有全量重载路径；它作为结构性配置变化的回退路径保留。

## 3. 总体架构

```text
订阅抓取/解析/配置归一化
            │
            ▼
      RuntimePlan Builder
  NodeKey + SpecDigest + Diff
            │
            ▼
       Prepare 阶段
  创建新增/新版本 outbound
  注册监控并执行必要校验
            │
            ▼
       Commit 阶段
 atomic.Pointer.Store(snapshot)
            │
       ┌────┴────┐
       ▼         ▼
 新连接使用新快照  旧实例进入 draining
                     │
                     ▼
              排空/超时后 Remove
```

控制面允许解析、diff、探测等耗时操作在 manager 主锁外执行。reload 之间使用独立的 `reloadMu` 串行化，`Manager.mu` 只保护配置引用和 box 生命周期等短操作。

## 4. 身份模型

需要区分“逻辑节点”和“某一版可运行 outbound”。

### 4.1 NodeKey

`NodeKey` 表示逻辑节点身份，不包含展示名和 URI fragment：

1. SQLite 节点且 `ID != 0`：使用带来源命名空间的数据库 ID，例如 `store:123`。
2. 没有稳定 ID 的 inline 节点：使用规范化 URI 的 SHA-256 摘要。
3. 规范化 URI 至少需要：scheme/host 大小写归一、移除 fragment、query 参数按 key/value 排序、保留所有影响连接的参数值。
4. VMess 等非普通 URL 格式应基于解析后的协议选项生成规范表示，不能只依赖字符串排序。
5. 日志和 API 只输出摘要后的 key，禁止输出包含凭据的原始身份材料。

同一配置出现重复 `NodeKey` 时，在 plan 阶段报错或按明确的来源优先级去重，不能靠遍历顺序生成 `-2`、`-3` tag。

### 4.2 SpecDigest 与 RuntimeTag

`SpecDigest` 由真正影响 outbound 行为的规范化选项计算，包括 URI 解析结果和适用的全局连接选项。

```text
RuntimeTag = "node-" + short(NodeKey) + "@" + short(SpecDigest)
```

结果：

- 仅改展示名：`NodeKey`、`SpecDigest` 和 `RuntimeTag` 都不变。
- 仅调整参数顺序：均不变。
- 连接参数变化：逻辑身份可保持时生成新 `RuntimeTag`；否则按删除旧节点、增加新节点处理。
- 未变化节点在 reload 前后复用相同 `RuntimeTag` 和 outbound 实例。

不得使用相同 tag 覆盖变更节点，因为 sing-box 会在 `Create()` 内立即关闭旧实例。

## 5. 数据结构

建议新增 `internal/runtimeplan`，避免让 `boxmgr` 继续依赖 builder 的内部 tag 生成细节。

```go
type NodeKey string

type RuntimeNode struct {
    Key        NodeKey
    SpecDigest string
    Tag        string
    Meta       pool.MemberMeta
    Outbound   adapter.Outbound
    Active     atomic.Int32
    Draining   atomic.Bool
}

type PoolSnapshot struct {
    Generation uint64
    Members    []*RuntimeNode
    ByKey      map[NodeKey]*RuntimeNode
}

type Diff struct {
    Added     []NodeSpec
    Removed   []*RuntimeNode
    Changed   []Replacement
    Unchanged []*RuntimeNode
}
```

`PoolSnapshot` 发布后必须完全不可变；slice、map、成员元数据均不得原地修改。已有连接可以继续持有旧 `RuntimeNode`，因此替换快照不会破坏连接计数和 drain 生命周期。

`poolOutbound` 保存：

```go
snapshot atomic.Pointer[PoolSnapshot]
```

每次 TCP/UDP 新建连接只 `Load()` 一次，选择过程始终在同一份 snapshot 上完成。选择成功后先增加该 runtime node 的 active 计数，再调用实际 outbound；连接包装器只负责对同一 runtime node 做一次递减。

## 6. poolOutbound 改造

### 6.1 动态成员接口

在 `internal/outbound/pool` 暴露窄接口，避免 box manager 依赖未导出实现类型：

```go
type SnapshotApplier interface {
    CurrentSnapshot() *PoolSnapshot
    ValidateSnapshot(*PoolSnapshot) error
    ApplySnapshot(*PoolSnapshot)
}
```

`ValidateSnapshot` 必须在发布前确认：

- 至少存在一个可用成员。
- 所有 runtime tag 都已注册到 `OutboundManager`。
- 网络能力、monitor handle 和元数据完整。
- snapshot 中不存在重复 key/tag。

### 6.2 依赖处理

动态 pool 不再把成员 tag 作为 sing-box 静态 `Dependencies()` 返回，否则 manager 的 `dependByTag` 永远无法随 snapshot 更新。成员启动顺序和存在性由 RuntimePlan 的 Prepare 阶段负责。

第一阶段仅修改默认 `proxy-pool`。group selector 等仍保留原静态依赖并触发全量重载。

### 6.3 监控状态

建议逐步拆分：

- `LogicalNodeState`，以 `NodeKey` 为 key：健康状态、黑名单、累计流量、monitor entry。
- `RuntimeState`，以 `RuntimeTag` 为 key：active、draining、创建时间和 drain deadline。

最低风险的第一阶段也可以保留现有 store，但必须增加：

```go
ActiveConnectionsForTag(tag string) int32
DeleteSharedStateIfIdle(tag string) bool
```

热更新路径禁止调用 `ResetSharedStateStore()`。只有完整关闭 box 或进入 idle 且所有连接均已结束时才允许全量 reset。

## 7. Reload 事务

### 7.1 配置分类

在 node diff 前先比较结构性配置。第一阶段仅在以下条件全部满足时进入增量路径：

- 旧、新模式均为 `pool`。
- 主 listener 地址、端口、协议和认证未变化。
- DNS、route、experimental、GeoIP 行为配置未变化。
- pool 策略参数未变化，或已被纳入 snapshot。
- group 配置未变化且不存在需要新增/删除的 group listener。
- 当前 box 和默认动态 pool 均处于运行状态。

不满足时调用保留的 full reload，并输出明确的 fallback reason。

### 7.2 Prepare

1. 获取当前 snapshot 和配置版本，不长时间持有 `Manager.mu`。
2. 生成规范化 node specs，计算 diff。
3. 对 Added 和 Changed 创建唯一的新 runtime tag。
4. 调用：

   ```go
   currentBox.Outbound().Create(
       boxContext,
       currentBox.Router(),
       runtimeLogger,
       spec.Tag,
       spec.Type,
       spec.Options,
   )
   ```

5. manager 需要在创建 box 时保留可用于动态组件的 context 和 sing-box logger。不能让动态组件统一使用无日志的临时 logger。
6. 从 manager 取回已启动的 `adapter.Outbound`，构建候选 snapshot。
7. 注册或刷新 monitor entry；根据 `MinAvailableNodes` 执行必要的健康校验。
8. 调用 `ValidateSnapshot()`。

Prepare 任一步失败时，删除本事务创建的所有唯一 tag，恢复 monitor 临时状态，不修改当前 snapshot 和 `m.cfg`。

### 7.3 Commit

Commit 临界区只做：

1. 确认当前 generation/config version 与 Prepare 起点一致。
2. `ApplySnapshot(candidate)`，内部执行一次 `atomic.Pointer.Store()`。
3. 更新 `m.cfg`、generation、last applied 字段和 monitor server 配置引用。
4. 复制 listener 回调列表后解锁。

提交后再通知 config listeners、触发探测和启动异步 drain。不得在锁内等待连接或执行外部回调。

### 7.4 Drain 与清理

对 Removed 和 Changed 的旧 runtime node：

1. 新 snapshot 发布后设置 `Draining=true`。
2. 等待该 tag 的 active 连接变为 0，最长等待 `drainTimeout`。
3. 排空后调用 `OutboundManager.Remove(oldTag)`。
4. 删除 runtime shared state 和不再需要的 monitor 绑定。
5. 超时后记录剩余连接数并强制 Remove；这只影响该旧版本节点，不影响 listener 和其他节点。
6. Remove 失败时进入有界重试队列并暴露指标，不能回滚已经提交的新 snapshot。

TCP、UDP 和可能复用底层会话的协议都必须通过集成测试确认 Close 行为。仅统计入口连接数量不能证明某个协议内部没有共享 mux 会话。

## 8. 并发与一致性

- `reloadMu`：串行化 Start/Reload/Stop/enterIdle 等生命周期操作。
- `Manager.mu`：只保护管理字段；禁止在持锁时解析订阅、探测、Start/Close 或 drain。
- `atomic.Pointer`：只保护已发布的不可变数据面快照。
- reload 请求携带配置版本；Prepare 完成时版本已过期则丢弃候选并清理资源。
- Stop 必须先阻止新 reload，再取消 drainer，最后统一关闭剩余 runtime nodes 和 box。
- snapshot 内不能引用 reload 后还会被原地修改的 `config.Config`、map 或 slice。

## 9. multi-port 第二阶段

multi-port 不能只增加 `Inbound().Create()`。当前 builder 为每个 inbound 生成静态 route rule，并将其指向 per-node pool outbound；动态新增 inbound 后 Router 不会自动出现对应规则。

推荐先引入一个长期存在的 EasyProxies dispatcher outbound：

```text
inbound metadata.Inbound
        │
        ▼
MultiPortSnapshot.ByInboundTag
        │
        ▼
对应 RuntimeNode.Outbound
```

所有 multi-port inbound 通过一条稳定路由进入 dispatcher，dispatcher 根据 inbound tag 从原子快照选择唯一节点。这样节点增删不再要求动态修改 route rules。

新增顺序：

```text
Create outbound
→ Create inbound（尚未发布或 dispatcher 尚无映射）
→ 原子发布包含 inboundTag → runtimeNode 的快照
```

删除顺序：

```text
从 dispatcher 快照移除映射，停止新流量
→ Remove inbound，停止 accept
→ 等 runtime node 排空
→ Remove outbound
```

端口分配必须改用新的 `NodeKey`，不能继续用原始 URI。已有节点保持端口；新增节点从 base port 向上寻找空闲端口；显式端口变化视为 inbound replacement。修改 outbound 参数但端口不变时只替换 runtime outbound，不 rebind listener。

hybrid 和 group listener 在 dispatcher 设计稳定并通过测试后再接入。

## 10. 全量重载回退

保留现有 full reload 作为迁移期 fallback，但应做以下修正：

- 日志分别记录 drain 等待时间与真实端口中断时间。
- 评估删除固定 500ms sleep，改为对 bind 错误进行短退避重试。
- 不要在等待 drain 前把 `currentBox` 清空；使用显式 lifecycle state 表示 reloading。
- full reload 失败后的 rollback 要恢复 lifecycle state，并避免 shared state 与实际连接脱节。
- 动态路径稳定后删除无调用方的 `gracefulSwitch()` / `drainOldBox()`，避免形成错误暗示。

## 11. 文件级实施计划

### Phase 0：测试基线与可观测性

- `internal/boxmgr/manager.go`
  - 增加 reload generation、路径选择和各阶段耗时日志。
  - 将 reload 串行化与普通状态锁分离。
- 增加持续请求测试工具，测量 reload 期间失败数和最大延迟。

### Phase 1：纯 pool 增量热更新

- `internal/config/`
  - 实现 canonical NodeKey 和 SpecDigest。
  - 将端口映射迁移到 NodeKey。
- `internal/runtimeplan/`（新增）
  - 生成 node spec、runtime tag 和 diff。
  - 分类增量变化与结构性变化。
- `internal/builder/`
  - 导出构建单节点 outbound spec 的窄接口。
  - 初始启动与动态更新复用同一套 tag/spec 生成逻辑。
- `internal/outbound/pool/`
  - 用 immutable snapshot 替代固定 `p.members`。
  - 增加按 runtime tag 的 active/drain/state API。
  - 移除动态成员的静态 dependency 声明。
- `internal/boxmgr/manager.go`
  - 新增 `reloadIncrementalPool()`。
  - 实现 Prepare/Commit/Rollback/Drain。
  - 结构性变化继续进入 full reload。

### Phase 2：multi-port

- 新增长期 dispatcher outbound 和 `MultiPortSnapshot`。
- 改造初始 route，使新增 inbound 无需新增 route rule。
- 实现单 inbound 的 Create/Remove、端口保留和失败回滚。

### Phase 3：hybrid 与 groups

- 将 group membership、selector 和 group runtime 纳入独立快照。
- 支持 group listener 的增删改和持久化状态迁移。
- 决定 pool 策略、健康阈值等字段是 snapshot 热更新还是结构性 fallback。

### Phase 4：清理

- 删除死代码和不再使用的全量 shared-state reset。
- 缩小 full reload 的触发范围。
- 更新 README、配置说明和运维指标。

## 12. 测试计划

### 12.1 单元测试

- NodeKey 忽略展示名、fragment 和 query 顺序。
- 不同凭据、服务器、端口及协议参数产生不同 SpecDigest。
- diff 正确分类 Added/Removed/Changed/Unchanged。
- snapshot 发布后旧 snapshot 不被修改。
- changed 节点生成新 runtime tag，不覆盖旧 tag。
- 按 tag active 计数不会被其他节点或全局 reset 干扰。
- reload 版本冲突时候选资源被完整清理。

纯内存的 timer/channel/WaitGroup 并发场景使用 Go 1.25 `testing/synctest`：以虚拟时间验证候选重建合并窗口、黑名单到期和 group gate 阻塞顺序，禁止用固定 `Sleep` 猜测 goroutine 状态。真实 socket、SQLite、sing-box 生命周期和系统调用不放进 synctest bubble，继续使用显式屏障、业务超时与 race detector。

### 12.2 集成测试

1. 在固定 pool listener 上持续发起短连接，同时添加 100 个节点：请求失败数必须为 0。
2. 保持长连接传输，同时删除它使用的节点：新连接不再选择旧节点，长连接在 drain deadline 前继续工作。
3. 修改一个节点参数：只创建一个新 runtime outbound，其余实例地址不变。
4. Prepare 中注入 Create/Start/健康检查失败：旧 snapshot 和旧配置继续服务，无资源泄漏。
5. Commit 后注入 Remove 失败：新 snapshot 正常服务，清理任务可重试。
6. 并发触发多次 reload：只有最新合法 generation 被提交。
7. `go test -race ./...` 无数据竞争；synctest 负责确定性调度断言，不能替代 race detector。

### 12.3 multi-port 验收

- 新增节点只新增一个 listener，已有端口没有 accept 中断。
- 删除节点后只有对应端口关闭。
- 修改 outbound 参数但保留端口时 listener 文件描述符不变。
- inbound 创建失败时不发布 dispatcher 映射，且新 outbound 被回滚。

## 13. 验收标准

第一阶段完成需同时满足：

- 纯 pool 节点增删改不调用 `oldBox.Close()`、`createBox()` 或固定 sleep。
- 未变化节点的 outbound 对象身份在 reload 前后保持不变。
- listener 在增量 reload 期间持续可连接。
- 被删除/替换节点的既有连接能够排空，超时只影响该节点。
- reload 前后的健康状态和统计不会被全量清零。
- 所有失败路径均有自动化测试，race detector 通过。
- 日志能区分 incremental、full fallback、rollback 和 drain timeout。

## 14. 风险与决策点

1. **sing-box manager 依赖表是静态的**：动态 pool 必须自行管理成员生命周期，不能继续依赖 manager 的成员 dependency bookkeeping。
2. **动态组件 logger/context**：实现前要确定长期持有的 sing-box context logger 创建和关闭方式。
3. **mux 协议排空语义**：需要用真实协议验证 Remove 是否关闭共享底层会话。
4. **inline 节点身份**：若希望修改 URI 后仍严格保留逻辑身份和端口，配置格式需要增加可持久化的显式 node ID；仅凭 URI 无法可靠判断“同一节点参数变化”还是“删除后新增另一个节点”。
5. **monitor generation**：未变化 entry 必须在新 generation 中重新标记为有效，避免 `SweepStaleNodes()` 误删。
6. **资源上限**：大量频繁变更可能形成多个 draining 版本，需要限制并发 drainer 数量、最长生命周期和重试队列大小。

上述决策在 Phase 1 编码前固定下来，避免热重载逻辑与身份、监控和端口模型再次分叉。
