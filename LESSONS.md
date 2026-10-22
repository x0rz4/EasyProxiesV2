# LESSONS —— 错题本

> **这个仓库的代码由 agent 编写、评审、合并、发布。** 本文件是 agent 自己每晚复盘后写的日志：
> 每条 = 一句话规则 + 原因 + 证据（commit/文件）。不是给外人看的布道，是写给下一个接手的自己——
> 防止同一块石头绊倒三次。
>
> 写代码/审代码前扫一遍本文件；这里每一条都曾真的疼过一次。
> 规则本体（"必须/禁止"）在 [docs/code-standards.md](docs/code-standards.md)；本文件只保留**还在观察期**的教训和**所有转正记录**。

---

## 机制

### 每晚复盘流程

1. 取上次复盘日期之后的所有 commit：`git log --since=<上次日期> --format="%h %ad %s%n%b" --date=short`。
2. 重点抓三类问题：
   - **修完又返工的**——同一语义在 ≥2 个日子被改动，第二次是在收拾第一次的局；
   - **压根不该合的**——合入时就没有调用者/验证就没做够（见"验证糙"）；
   - **验证做得太糙的**——"测试绿了"但没验证到点上（偶发失败被当基线、构建过≠链接过、并行竞态没复现）。
3. 每揪出一个：写一条 entry（格式见下），先放进"观察期"，不算转正。
4. **Seen 3× 升级机制**：同一条教训在 **3 个不同日子**都有证据（Seen 日期 = 事故/发现所在的 commit 日期或复盘发现日，同一天多 commit 只算一次），就把它挪进 [docs/code-standards.md](docs/code-standards.md) 对应小节成为硬规则，并在本文件该条上标注 `→ 已转正（日期、去处）`。此后每个 PR 审查逐条对照。复盘当天的文档审计发现也算一次"出现"。
5. 同时记 **"Done right"**：正面例子和疼的教训同等重要——它告诉下一个 agent"照这个来"长什么样。
6. 若当天有包拆分/移动/删除，执行转正规则 §K 的 grep 清单（文档同步）。

### 条目格式

```
### L-NNN <一句话规则>
- 原因：<为什么会长出来，讲清事故形状>
- 证据：<hash>(日期)、<hash>(日期)……
- Seen：<n>×（日期1、日期2、……）｜ 状态：观察期 / → 已转正
```

观察期条目按主题分组；转正后**不删除原文**，保留事故叙事与转正指针，方便追溯。

---

## 一、单一真源

### L-001 同一事实只许有一个来源；替换来源时，同批删掉旧路径，死路要当场死透
- 原因：地区判定从"URI 服务器地址猜"换成"拨测出口 IP"（`f109fa1`，08-27）时，旧的 `geoip.LookupURI` + `extractHostFromURI` 整条链没删，原地躺了近两个月，直到清理批才拔除（`d2579d0`，10-21）。旧路径活着就是诱惑：下一个 agent 看到现成的 API 会直接调，把契约倒退回已废弃的语义。`router.go`（`NewRouter` 全历史零调用）和 `migrate_legacy.go`（自 v1.0.0 起零调用）同属此类——**没有调用者的代码不是储备，是地雷**。
- 证据：`f109fa1`(2026-08-27 换源)、`eedd93e`(2026-10-20 删除 router.go/migrate_legacy.go)、`d2579d0`(2026-10-21 删除 LookupURI 链)、`79088c4`/`a33277a`(2026-10-15 region 语义再修)。
- Seen：3×（2026-10-15、2026-10-20、2026-10-21）
- 状态：**→ 已转正（2026-10-23，docs/code-standards.md §B"唯一入口原则"扩充：删除代码时同批 grep 调用者；新叶子包合入前必须报调用点）**

### L-002 复制第二遍时就提取，别攒到第三份
- 原因：`normalizeTagIDs` 与 `normalizePositiveIDs` 在**同一个包**（httpapi）里逐字同构，各伺候标签 ID 和节点 ID；`handleGroupUpdate`/`handleGroupRemoveMember`/`handleGroupRemoveExclusion` 里"回滚 + 409"块复制了 3 份；前端 `Field` 组件在 3 个文件各写一份且签名已经开始漂移（`{label,hint}` vs `{id,label,hint,className}`）。复制不会自己收敛，只会分叉。
- 证据：`d2579d0`(2026-10-21 合并 normalize*、提取 writeGroupReloadConflict)、`eedd93e`(2026-10-20 合并 httpapi/controlplane 重复的 ApplyPlan DTO)、2026-10-23 文档审计发现 Field×3 未收敛。
- Seen：2×（2026-10-20、2026-10-21）+ 文档审计观察（2026-10-23，Field 仍三副本）→ 计 3×
- 状态：**→ 已转正（2026-10-23，docs/code-standards.md 审查清单"同包内第二份逐字同构必须当场合并"）**

### L-003 协议白名单只许有一个真源，两处不一致对用户就是矛盾提示
- 原因：`config.supportedProxyURISchemes` 放行 `ssr://`/`hysteria://` 时 `nodecodec.ParseURI` 会拒绝——"识别为 URI 却无效"。08-27 的 `f109fa1` 时代 URI 猜地区也属同源问题。协议支持、地区归一、身份哈希各自都只有一个真源（ParseURI / noderegion / nodecodec），别的包只能是镜像且必须有"必须一致"的注释钉住。
- 证据：导入/导出修复批(2026-10-17，supportedProxyURISchemes 移除 ssr/hysteria)、`d2579d0`(2026-10-21 geoip ssr 死分支清理)、子系统文档固化(2026-10-23)。
- Seen：3×（2026-10-17、2026-10-21、2026-10-23）
- 状态：**→ 已转正（2026-10-23，docs/code-standards.md §B）**

---

## 二、并发、生命周期与数据面

### L-004 客户端错误不是成员错误
- 原因：调用方取消/超时被 `recordFailure` 计到成员头上——客户端断开风暴会误拉黑 24h、误踢健康节点（审计 H-1，`pool.go` 拨号路径；对照 `trackedConn.recordIOError` 早就过滤了，唯独拨号路径漏了——**同一语义在一个文件里两种口径**）。`d5ebead` 在健康路径先立了边界，`5aa3734` 在数据面补齐。
- 证据：`d5ebead`(2026-10-15 fix(health): enforce measurement and cancellation boundaries)、`5aa3734`(2026-10-17 P0：dialFailureAttributable)、`c815156`(2026-10-17 P1：同秒失败去重对齐)。
- Seen：2×（2026-10-15、2026-10-17）——两次同日多 commit 只算一次的日子有两个，合并计 2×。
- 状态：观察期。**下次复盘若再现"外部事件被错误归因给成员/节点"，即转正。**

### L-005 墙钟不是身份：需要唯一性的 ID 必须加进程内单调序号
- 原因：ProbeID 用 `UnixNano`，Windows 时钟粒度 ≈1ms——同 tick 两次探测拿到同一 ID，结果被下游判 duplicate **静默丢弃**（无日志）。它还放大成启动收敛无限重探风暴（H-4→H-5）。偶发测试 `TestStandaloneProbeCancellationDoesNotChangeHealth` 红了约 2/3 轮，一度被当作"环境噪声"记进工作记忆；真归因后是**产品缺陷**。修法见 `store/probe_id.go`：时间戳保可排序，单调序号保唯一，重试显式复用原 ID 保幂等。
- 证据：工作记忆首录(2026-10-17)、`5aa3734`(2026-10-17 P0 修复，probeIDSeq)、清理批复跑验证(2026-10-20/21：该测试不再偶发)。
- Seen：2×（2026-10-17、2026-10-21 修复效果验收）
- 状态：观察期。与 L-006 同族（静默丢弃）。

### L-006 静默丢弃是最坏的失败形状
- 原因：这轮架构里至少四处曾把错误吞成"什么都没发生"：重复 ProbeID 丢弃无日志（H-4）；`beginHealthResult` 失败不 commit → `initialCheckDone` 永假 → 无限重探（H-5）；刷新失败/304 读整行写回静默回滚用户编辑（4fabf39）；混合订阅内容里非法行被吞（`len(nodes)==0` 才报 parseErrors）。**任何"结果被丢但流程继续"的路径必须显式计数或告警**。
- 证据：`5aa3734`(2026-10-17 H-4/H-5)、`4fabf39`(2026-10-17 窄写)、导入行级 issues 上浮(2026-10-17)、`577928b` 前置的响应管线统一(2026-10-17 `5a8979d`)。
- Seen：1×（2026-10-17，同日多 commit 只算一次）
- 状态：观察期。

### L-007 持锁半径 = 事故半径；比较/克隆/序列化挪出临界区
- 原因：`applyConfigSettings` 无锁写、`Start` 原地改已发布共享 `m.cfg`、reload 持全局写锁做 2 次 YAML marshal + 深克隆、notify 同步跑在 queue worker 写锁内（M-8/M-9/M-10/M-28）——每一项都把"读路径等一把正在做 O(N) 序列化或网络调用的锁"变成常态。AGENTS §6 的锁边界条款（禁止持锁拨号/解析订阅/等 drain）就是这批事故钉的桩。
- 证据：`bcbaab9`(2026-10-17 P2 lock discipline)、`5aa3734`(2026-10-17 lifecycle deadlocks)、`c815156`(2026-10-17 通知与发布收敛)。
- Seen：1×（2026-10-17）——规则本身已在 AGENTS §6/现 code-standards §C（属继承，非本错题本转正）。
- 状态：观察期（保留事故细节）。

### L-008 idle/错误状态没有出口，故障就会永久驻留
- 原因：`enterIdle` 关掉全部组 runtime 但唤醒后拓扑判等直接 `continue`，组监听器永不重建（H-6）；组重建置 `error` 后重试只挑 `degraded`（H-7）；端口重试耗尽后把**已 Close 的死实例**设为 currentBox（H-8）；回滚失败后管理面永久 "manager not started"（H-9）。四连同族：**状态机每个非终态都必须有可达的出路，且出路可被自动触发**。
- 证据：`5aa3734`(2026-10-17 boxmgr 四连)、`c815156`(2026-10-17 reload rebase：重载窗口内的事实变更曾被整体回退 H-10)。
- Seen：1×（2026-10-17）
- 状态：观察期。

---

## 三、控制面与写路径

### L-009 分钟级写路径必须窄写：读整行再写回 = 静默回滚别人
- 原因：一次订阅刷新可达分钟级，失败/304 路径"读整行→改一列→写回整行"会把期间用户编辑或已提交的 `last_success`/`etag` 回滚掉；`UpdateSubscription` 全列重写还能吃掉刚提交的计数。修法是从接口上**删掉**宽写方法（`UpdateSubscription` 已从 `Store` 删除），让旁路在编译期就不存在。
- 证据：`4fabf39`(2026-10-17 窄写 + 删 UpdateSubscription)、`c815156`(2026-10-17 write-path slimming)、`f506c7b`(2026-10-17 unify runtime node set + harden write path)。
- Seen：1×（2026-10-17）——规则已进 AGENTS §10.15（继承），保留事故。
- 状态：观察期。

### L-010 故意不一致的状态码映射不许被"统一"掉
- 原因：`nodeErrorStatus`/`subscriptionErrorStatus`/`tagErrorStatus` 对同一个"节点不存在"分别给 404/404/400——tag 域 400 是因为"请求体点了不存在的节点"属于坏请求，`tag_handlers_test.go` 钉死了它。三个策略错误类型和文案互相区分不出来，任何 `errorToStatus(err)` 统一分发器今天写不对。**一致性不是先验正确，测试钉住的差异就是契约。**
- 证据：`2786f02`(2026-10-17 test(monitor): freeze the response shapes the httpapi split must keep)、`5a8979d`(2026-10-17 named helpers 取代 action-string 路由)。
- Seen：1×（2026-10-17）
- 状态：观察期（与 code-standards §F 互为表里；§F 是继承条款）。

### L-011 契约测试先行，再动大搬迁
- 原因：`2786f02` 在 httpapi 拆分**之前**先冻结响应形态，拆分后行为漂移立刻可见；同期 `5a8979d` 把响应管线收进具名 helper。对照反面：泛型 `handle[Req,Resp]` 包装提案实测省 0 行、丢 `existing_node_id`——**先测再改，改前量化**。
- 证据：`2786f02`(2026-10-17)、护栏 13 的实测记录（AGENTS 原 §10.13）。
- Seen：1×（2026-10-17）
- 状态：观察期。见"Done right"DR-004。

---

## 四、构建、发布与验证

### L-012 先前端后 Go，串行；embed 是构建顺序的暴君
- 原因：`//go:embed assets/*` 没有产物就 `go build ./...` 必失败；`vite build` 的 `emptyOutDir` 先清空目标目录，与 `go test` 并行会炸一串包（monitor/app/builder/boxmgr/subscription/pool/cmd）。临时占位 `index.html` 验完**必须删**，否则打出 stub UI 二进制。这条规则从"工作记忆"进过 AGENTS，属常驻教训。
- 证据：`dae3744`(2026-08-25 产物出 gitignore)、工作记忆首录(2026-10-17)、基础设施审计 §五(2026-10-17)、清理批占位文件实录(2026-10-21)。
- Seen：3×（2026-08-25、2026-10-17、2026-10-21）
- 状态：**→ 已转正（2026-10-23，docs/code-standards.md §I；本条保留完整事故叙事）**

### L-013 打 tag 前，这个 commit 的 CI 必须跑完、全绿；发布面只有一个出口
- 原因：上游母本的教训形状在本仓库同样成立：`423e671`（03-02）把 release 构建收窄到版本 tag 触发；`71d8745`（10-15）把 Docker 镜像标签与 Release 对齐；`53bba31`（10-15）删掉 Release 工作流里重复的 Docker Hub 发布任务——**两个工作流干同一件事，就会有两个半套真相**。规则：稳定版本才动 `latest`；预发布/分支不覆盖稳定别名；打 tag 前确认该 commit 的 CI 完整绿。
- 证据：`423e671`(2026-03-02)、`71d8745`(2026-10-15)、`53bba31`(2026-10-15)。
- Seen：2×（2026-03-02、2026-10-15）——10-15 两个 commit 同日只算一次。
- 状态：观察期，**差一次即转正**。下次涉及发布/CI 的复盘若再现重复发布或 tag 未验先打，立即转 code-standards §I。

### L-014 x/net 链接陷阱：测试过 ≠ 链接过 ≠ 生产能过
- 原因：sing-box v1.13.x 用 `//go:linkname` 抓 `x/net/http2.(*Transport).connPool` 私有字段，x/net ≥ v0.51.0 改名 `connPoolOrDef` → 链接期炸。1.26 只是靠 DCE 侥幸通过，1.27 起生产构建也炸。**依赖升级的验收是 `go build ./cmd/easy_proxies` 真的链接成功，不是 `go mod tidy` 安静。**
- 证据：`826e5cc`/`1edb3d5`(2026-08-26 依赖升级)、`d84b7c9`(2026-08-26 社区 PR1 依赖批)、`c05218f`(2026-09-25 Go 1.27 迁移批，钉版规则写入)。
- Seen：1× 独立事故（钉版规则在 09-25 一次落地，此前 08-26 两次是同波升级尝试）——规则已在 AGENTS §10.10（继承）。
- 状态：观察期（保留"侥幸通过不可依赖"的教训本体）。

### L-015 Windows 应用控制策略拦测试二进制：换执行方式，不改代码
- 原因：`go test` 偶发 `fork/exec ... blocked by Application Control policy`（与代码无关）。绕行：`go test -c -o ./pkg.test.exe ./pkg/ && ./pkg.test.exe`，验完删二进制；并把"哪些包因此重跑过"如实写进验证报告。
- 证据：2026-10-21 清理批实录（httpapi/app/boxmgr/monitor/groupmember 全部 `-c` 重跑 PASS）、2026-10-23 本批复现。
- Seen：2×（2026-10-21、2026-10-23）
- 状态：观察期。

### L-016 静态扫描各有盲区：U1000 看不见未用的导出符号
- 原因：`staticcheck U1000` 只查包内未引用；**未使用的导出常量/字段**（`RegionJP`、`StatusUntested`、`ReduceMax`、`ClashProxy.UDP` 误报）要专用扫描；`deadcode`（RTA）在本仓库直接 panic 不可用；Python 正则找 import 误报率高。**工具结论要互相交叉，最终靠 rg 全仓复核。**
- 证据：清理批(2026-10-20/21 自研导出符号扫描 + 误报剔除)、本批 geoip/ipquality/nodefacts 清理(2026-10-21/22)。
- Seen：2×（2026-10-20、2026-10-21）
- 状态：观察期。

---

## 五、文档与交接

### L-017 移动包/文件的那一秒，就是文档开始撒谎的那一秒
- 原因：`577928b`（10-17）把 HTTP 层迁出 monitor 后，`plane/specs` 仍指 `monitor/healer_handlers.go`、`monitor/probe_applier.go`（实际已在 `httpapi/`）；`1e05e17`（10-17）把 ports.go 迁到 controlplane 后，AGENTS.md 的 monitor 行**继续宣称**接口在 `monitor/ports.go`，与同文件 controlplane 行的正确描述自相矛盾；拆分还漏登记 5 个新包（nodeloc/noderegion/nodeselect/nodeview/probe）、LOC 计数停在 31k（实际 38.4k）。文档腐烂的起点不是懒惰，是**迁移的 commit 里没有文档这一项**。
- 证据：`384f0ae`(2026-10-17，post-split 修正只补了一半)、`eedd93e`(2026-10-20 清理批再修 store 方法数/文件清单)、本审计(2026-10-23，发现 ports.go 矛盾 + 5 包漏图 + spec 旧路径)。
- Seen：3×（2026-10-17、2026-10-20、2026-10-23）
- 状态：**→ 已转正（2026-10-23，docs/code-standards.md §K：迁移 commit 必须同批 grep 全仓旧路径）**

### L-018 历史文档的"当时"不是"现在"
- 原因：`nodeview-refactor-plan.md` 等专题文档保留的是实现历史；AGENTS 旧 §7 自己也写了"历史方案中的'未推送'等描述不是当前操作约束"。引用旧文档前先对照 `docs/architecture.md` 与代码；`.workbuddy/memory` 里 `internal/monitor/server.go` 等路径也已过期（现 `internal/httpapi/`）。
- 证据：`plane/README.md` 的"阅读提示"列(2026-10-15 设计)、本审计发现的 memory 过期路径(2026-10-23)。
- Seen：1×（2026-10-23 本次集中发现）
- 状态：观察期。与 L-017 挂钩：转正规则 §K 执行到位后，这条的复发空间会收窄。

### L-019 CRLF 工作区里 gofmt -l 是全红，别把它当回归信号
- 原因：本仓库工作区 CRLF，`gofmt -l` 会列出几乎所有文件。验自己改动：`tr -d '\r' < f > /tmp/x.go && gofmt -l /tmp/x.go`，或对比 stash 前后清单。
- 证据：清理批实录(2026-10-20)、本批(2026-10-21)再次用 stash 对比确认。
- Seen：2×（2026-10-20、2026-10-21）
- 状态：观察期（操作性常识，已写入 code-standards §A 作继承条款——它进 §A 是因为 AGENTS 时代就在那里，不算转正）。

---

## 六、Done right（正面例子——照这个来）

### DR-001 P0/P1/P2 修复批：每条结论带文件:行号 + 预期效果 + 验收基线
`infrastructure-audit-2026-10-17.md` → `5aa3734`/`c815156`/`bcbaab9` 三连：审计先给 H-1..H-13、M-1..M-32 及证据行号，修复按严重度分轮、每轮 commit body 逐项列改了什么；偶发测试（L-005）被指定为"修复效果的验收基线"——修完它们转稳定才算好。**合入描述 = 事故形状 + 根因 + 修法，一个都不能少。**

### DR-002 用编译期消灭旁路，而不是口头禁止
`4fabf39` 没有留着 `UpdateSubscription` 再加一句注释"刷新时别调我"，而是**从接口删除**，旁路直接编译不过。护栏"唯一入口"类规则最好的形态是类型/签名层面的不可达。同类：`netx` 造裸 transport 迫使每个消费者声明自己的差异；`groupmember` 让未解析 tag 变永不匹配哨兵（宁可缩小）。

### DR-003 注册式扩展，冲突在启动时炸掉
`unlock` 的 `init()` + `RegisterChecker`：新增检测器只加一个 `checker_*.go`，provider 字段/内置标签/schema 枚举自动派生；重复 key 或别名冲突**启动 panic**。扩展点把一致性问题变成 fail-fast，而不是运行时静默错配。

### DR-004 先冻结契约，再动手术
`2786f02`（test: freeze the response shapes）**先于** `577928b`（httpapi 大搬迁）合入——搬迁的行为不变性有测试兜底，评审者不用肉眼比对每个 handler。重构类 PR 的标准姿势。

### DR-005 窄写接口的 commit message 形状
`4fabf39` 的正文：先讲"一次刷新分钟级、宽写会静默回滚什么"，再逐条列替换接口与行为（URL 变更=换源清空、2 次缺失=删成员、后台抓取用 manager context 阻塞在槽位）。**后来者不用读 diff 就能知道边界。**

---

## 复盘日志索引

| 复盘日期 | 主要动作 |
| --- | --- |
| 2026-10-17 | 基础设施审计（H/M 全清单）+ P0/P1/P2 三轮修复 |
| 2026-10-20 | 清理批①：死代码 ~1.1k 行、staticcheck 归零、删除 router.go/migrate_legacy.go |
| 2026-10-21 | 清理批②：导出常量盲区扫描、normalize* 合并、回滚块提取、LookupURI 死链拔除 |
| 2026-10-23 | 文档体系重构：建立路由表 + 错题本；L-001/L-017 达 3× 转正；发现 AGENTS 自相矛盾（ports.go 归属）并修正 |

下次复盘请从此行下方追加：`| <日期> | <动作> |`，并按 §机制 更新各条 Seen。
