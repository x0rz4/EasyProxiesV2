# 节点打标系统实现说明

> 状态：已实现（migration 13，`internal/nodefacts`、`internal/nodetag`、`internal/groupmember`、`/api/tags*`、前端「节点标签」页）
> 取代 `plane/group-screening-rules-page.md` 中 §7.5、§8.1、§10.1 的设计，并把它 §3.2 记录的「自动标签覆盖人工标签」缺口关闭
> 相关：`plane/incremental-hot-reload.md`（本方案不依赖其第一阶段，成员增量走自己的路径）

## 1. 目标

让运营人员用**标签**而不是节点 ID 列表描述节点质量档位，让分组池按标签自动纳管成员。

产品映射与 `group-screening-rules-page.md` §1 一致，但实现路径不同：不引入独立的 `screening_rules` 引擎，规则挂在标签上，分组通过「标签白名单 / 黑名单」间接完成筛查。

| 商品 | 分组池 | 标签筛选 | Xboard 职责 |
| --- | --- | --- | --- |
| 香港基础 | 10001 | 地区 `hk` | 展示「香港 01 [基础]」，倍率 1.0x |
| 香港 VIP | 10002 | 地区 `hk` + 白名单 `原生IP`、`Netflix解锁`（all） | 展示「香港 02 [VIP]」，倍率 2.0x |
| 游戏专线 | 10003 | 白名单 `⚡极速`（any） | 展示「游戏专线 01」，倍率 3.0x |

倍率不进入 EasyProxies 的流量计算。

## 2. 分层

```text
检测流水线 (nodecheck / persistUnlockResult)
        │ Enqueue(nodeIDs)
        ▼
internal/nodetag.Queue        去抖 2s / 周期兜底 10min / 单次重算超时 5min
        │
        ▼
internal/nodetag.Service.Recompute(nodeIDs)
        ├─ internal/nodefacts.Loader    固定 7 组批量查询，与节点数无关
        ├─ internal/nodefacts.Evaluate  纯函数，Fact[T] 三态语义
        └─ internal/nodetag.Resolve     互斥组裁决（priority DESC, id ASC）
        ▼
store.ReplaceAutoNodeTags     只删 source='auto'，人工标签零风险
        │ changedNodeIDs（仅投影真正变化的节点）
        ▼
boxmgr.ApplyGroupMembershipChanges  经 internal/groupmember.Filter 判定
        └─ 只重建成员集变化的分组 box；base box 永不 reload
```

分层原则：`nodefacts` 只认「事实 + 条件 + 求值」，不认标签；`nodetag` 只认标签语义；`groupmember` 是成员判定的唯一权威。将来若真要做 `screening_rules`，直接复用前两者。

## 3. 数据模型（migration 13）

`internal/store/migrations.go` 的 `allMigrations()` 第 13 项，描述 `add node tagging system`。此前最大版本为 12，后续新功能从 14 起。

### 3.1 表结构

```sql
CREATE TABLE tag_mutex_groups (
    id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(name));

CREATE TABLE tags (
    id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL,
    color TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
    mutex_group_id INTEGER REFERENCES tag_mutex_groups(id) ON DELETE SET NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    auto_enabled INTEGER NOT NULL DEFAULT 0,
    rule_json TEXT NOT NULL DEFAULT '', rule_version INTEGER NOT NULL DEFAULT 1,
    builtin_key TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(name));
CREATE INDEX idx_tags_auto ON tags(auto_enabled);
CREATE INDEX idx_tags_mutex ON tags(mutex_group_id, priority DESC, id);

CREATE TABLE node_tags (
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    tag_id  INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    source  TEXT NOT NULL DEFAULT 'manual',        -- manual | auto
    rule_version INTEGER NOT NULL DEFAULT 0,
    matched_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (node_id, tag_id, source));
CREATE INDEX idx_node_tags_tag ON node_tags(tag_id, source);

ALTER TABLE group_pools ADD COLUMN tag_whitelist_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE group_pools ADD COLUMN tag_blacklist_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE group_pools ADD COLUMN tag_filter_match   TEXT NOT NULL DEFAULT 'any';
```

迁移末尾用 `json_each(nodes.tags)` 把现有每个标签字符串回填成 `tags` 行 + `node_tags(source='manual')` 行（`json_valid` + `json_type='array'` 双重守卫，非法 JSON 视为空数组），保证升级不丢已有标签，emoji 名称原样保留。

### 3.2 设计取舍

- **互斥组用独立表**，不用 `tags.mutex_group` 文本列：改名不必批量 UPDATE，`ON DELETE SET NULL` 表示删组只解除互斥、不删标签。
- **规则 AST 存在 `tags.rule_json`，与标签 1:1**。产品模型是「标签可以有一条规则」，独立 rules 表只会多一次 join 和一类孤儿数据。`rule_version` 每次改规则自增并写入 `node_tags.rule_version`，便于查「哪些命中早于当前规则」。
- **`node_tags` 主键含 `source`**，这是「解锁检测绝不可能删掉人工标签」的**结构性**保证：`ReplaceAutoNodeTags` 只发 `DELETE ... WHERE node_id=? AND source='auto'`。同一 (node, tag) 允许 manual 与 auto 并存，幂等无害。
- **分组白/黑名单存标签 ID**（JSON int 数组，与 `explicit_node_ids_json` 一致），改名不破坏分组；`DeleteTag` 在同一事务内从两个 JSON 列清除该 ID。
- **`builtin_key`** 用于幂等识别内置模板 —— 名称可被运营改写，不能靠名字匹配。

### 3.3 `nodes.tags` 是投影列

`nodes.tags` 保留为 `manual ∪ auto` **名称**的去重排序投影，由标签层在同一事务内改写。这样 `store.Node.Tags`、`ManagedNode.Tags`、`mergeNodeMetadata` 的标签并集以及所有现有读点零改动，运行期分组筛选也不用 join。

**约定：标签层之外任何对 `nodes.tags` 的写入都会被下一次重算覆盖。** 这条写在三处：migration 13 的 SQL 注释、`store.Node.Tags` 的字段注释、以及本文档。

投影刷新**不走 `UpdateNode`**，而是直接 `UPDATE nodes SET tags=?`：否则标签抖动会刷新 `nodes.updated_at`，污染节点身份对账与订阅 diff。

会刷新投影的方法：`SetManualNodeTags`、`BatchUpdateManualNodeTags`、`ReplaceAutoNodeTags`、`DeleteTag`。**`UpdateTag` 不刷新** —— 改名后的投影由调用方触发 `Recompute` 补齐（见 §8 的 `PUT /api/tags/{id}`）。

### 3.4 两处必须联动的地方

缺任何一处，标签都会静默消失：

- `internal/store/node_identity.go` 的 `mergeNodeReferences`：`reconcileNodeIdentities` 在**每次 `store.Open()`** 都会跑，必须像其它 node 维度表一样把 `node_tags` 从被去重节点迁移到胜出节点（`INSERT OR IGNORE ... SELECT` 后 `DELETE`），并重算胜出者的投影。
- `clearNodeDetectionCache`：清检测事实的同时删 `source='auto'` 行并把投影改写为 manual-only，再由调用方 `Enqueue` 该节点重算。auto 标签不能比它的证据活得更久，manual 必须留下。

### 3.5 Store 接口

`internal/store/store.go` 的 `// --- Node tags ---` 段（实现集中在 `internal/store/sqlite_tags.go`）：

```go
ListTags / GetTag / GetTagByName / CreateTag / UpdateTag / DeleteTag
ListTagMutexGroups / CreateTagMutexGroup / UpdateTagMutexGroup / DeleteTagMutexGroup
ListNodeTags(ctx, NodeTagFilter{NodeIDs, TagIDs, Source}) ([]NodeTag, error)
CountNodesByTag(ctx) (map[int64]int, error)
SetManualNodeTags(ctx, nodeID int64, tagIDs []int64) error
BatchUpdateManualNodeTags(ctx, nodeIDs, addTagIDs, removeTagIDs []int64) error
ReplaceAutoNodeTags(ctx, assignments []NodeAutoTagAssignment) error
```

`// --- Batch fact reads ---` 段，每个方法都接受节点 ID 切片，`nil` 表示全量，内部按 900 分块规避 `SQLITE_MAX_VARIABLE_NUMBER`：

```go
ListNodeStats / ListNodeDetectionResultsByIDs / ListNodeIPQualityResultsByIDs
ListUnlockResultsByIDs / ListNodeSubscriptionIDs
```

原有的 `ListNodeDetectionResults`/`ListNodeIPQualityResults`/`ListUnlockResults` 改为传 `nil` 的一行委托。

## 4. 事实与规则引擎（`internal/nodefacts`）

```text
fact.go       Fact[T] 三态、NodeFacts、Kind 常量、时效判定
schema.go     字段/操作符注册表与 Schema 快照
condition.go  条件 AST、JSON 编解码、Validate、DefaultLimits
evaluate.go   Evaluate(cond, facts, now) —— 纯函数，不碰 store
loader.go     Loader：批量事实装载
```

### 4.1 Fact 三态

```go
type Fact[T any] struct { Value T; Known bool; CheckedAt time.Time }
```

`Known=false` 即「未检测」。求值语义（`evaluate.go`）：

1. 叶子取对应 `Fact`；
2. 若 `MaxAgeSeconds > 0 && !CheckedAt.IsZero() && now.Sub(CheckedAt) > MaxAge`，强制 `Known=false`；
3. `!Known` 时，除 `is_unknown`（true）/`is_known`（false）外**一律 false**，**包含 `ne`、`not_in`、`not_contains` 这类否定操作符**；
4. `Negate` 在未知短路**之后**施加 —— 否则一个未知事实会让 `negate` 条件意外命中，未知质量的节点就会漏进 VIP 池；
5. `Match: "none"` = `!any`，只作用于子节点。

这条「未知不命中」是整套系统最重要的安全语义，`internal/nodefacts/evaluate_test.go` 对 12 个非三态观测操作符逐个钉住。

### 4.2 条件 AST 与限制

```go
type Condition struct {
    Match    string      `json:"match,omitempty"`    // all | any | none
    Children []Condition `json:"children,omitempty"`
    Field    string      `json:"field,omitempty"`
    Op       Operator    `json:"op,omitempty"`
    Value    any         `json:"value,omitempty"`
    Values   []any       `json:"values,omitempty"`
    MaxAgeSeconds int64  `json:"max_age_seconds,omitempty"`
    Negate   bool        `json:"negate,omitempty"`
}
```

`DefaultLimits()`：`MaxConditions=50`、`MaxDepth=3`、`MaxValueItems=100`、`MaxRegexLength=200`、`MaxRuleBytes=16384`。

`Condition.Validate(registry, limits)` 是唯一的校验入口，覆盖：规则序列化后的字节数、深度、条数、group 的 `match` 只能是 `all/any/none`、叶子必须有字段、操作符元数（`OperatorArity`）、字段-操作符兼容性（`Field.SupportsOperator`）、`MaxAgeSeconds >= 0` 且只能出现在 `SupportsMaxAge` 字段上、值列表长度、正则长度与 `regexp.Compile`。**HTTP 层不需要重复做体积校验**，只需 `MaxBytesReader` + `Validate`。

`MarshalRule` 用 canonical JSON（键排序），因此未改动的规则序列化字节恒定，`rule_version` 只在真实编辑时前进。`ParseRule` 把空/纯空白载荷视为「无规则」而非错误；`MarshalRule` 对空 `Condition` 返回 `nil` —— 所以**清空规则的方式是提交空对象 `{}`，不是 `null`**。

### 4.3 操作符

14 个，带中文标签与值元数（`ArityOne` / `ArityMany` / `ArityTwo` / `ArityNone`）：

| 操作符 | 标签 | 元数 |
| --- | --- | --- |
| `eq` / `ne` | 等于 / 不等于 | one |
| `in` / `not_in` | 属于 / 不属于 | many |
| `contains` / `not_contains` | 包含 / 不包含 | one |
| `gt` / `gte` / `lt` / `lte` | 大于 / 大于等于 / 小于 / 小于等于 | one |
| `between` | 介于 | two |
| `regex` | 正则匹配 | one |
| `is_unknown` / `is_known` | 未检测 / 已检测 | none |

Go 的 regexp 是 RE2，无灾难性回溯，但仍限长 200 并在保存/预览时提前编译。

### 4.4 字段注册表

`DefaultRegistry(options...)` 注册顺序即 UI 展示顺序，分 5 组：`基础`、`性能`、`IP 质量`、`解锁`、`来源`。provider 相关字段通过 `WithUnlockProviders` / `WithIPQualityProviders` 注入，`nodefacts` 因此**不导入任何 checker 注册表**（否则会把 HTTP 客户端拖进纯求值包）。

| 字段 | 类型 | 来源 |
| --- | --- | --- |
| `node.name` | string | `nodes.name` |
| `node.region` | enum | `nodes.region`（空值归一为 `other`） |
| `node.country` | string | `nodes.country` |
| `node.protocol` | enum | `nodes.uri` 的协议头 |
| `node.port` / `node.enabled` / `node.source` | int/bool/enum | `nodes.*` |
| `tags.manual` | set | `node_tags (source=manual)` |
| `node.subscription_ids` / `node.subscription_names` | set | `subscription_nodes` / `subscriptions.name` |
| `latency_ms` | int + 时效 | `node_detection_results.latency_ms` 优先，回退 `node_stats.last_latency_ms`（`-1` 视为未检测） |
| `available` | bool + 时效 | `node_stats.available`（仅在 `initial_check_done=1` 时视为已检测） |
| `blacklisted` / `failure_count` / `success_count` | bool/int | `node_stats.*` |
| `speed_bps` / `speed_peak_bps` | int + 时效 | `node_detection_results.average/peak_bytes_per_second` |
| `exit_country_code` / `exit_ip_family` | enum + 时效 | `node_detection_results.*` |
| `unlock.ip.pure` | bool + 时效 | `node_unlock_results.ip_pure` ← **「原生IP」的真正来源** |
| `unlock.ip.risk_level` / `.ip_type` / `.usage_type` / `.fraud_score` / `.asn` / `.org` | enum/string/int + 时效 | `result_json` 内 `ip.*` |
| `unlock.<provider>.status` / `.region` / `.detail` | enum/string + 时效 | `result_json` 内 `services[]` |
| `unlock.unlocked_count` / `unlock.unlocked_providers` | int/set + 时效 | `services[]` 归约 |
| `ipq.<provider>.fraud_score` / `.is_residential` / `.proxy` / `.hosting` / `.mobile` / `.is_broadcast` / `.asn` / `.org` / `.isp` / `.country_code` | int/bool/string + 时效 | `node_ip_quality_results`（provider ∈ `ippure`、`ip-api`） |
| `ipq.max.fraud_score`、`ipq.any.{is_residential,proxy,hosting}`、`ipq.all.{...}` | int/bool + 时效 | **显式声明的跨 provider 归约** |

跨 provider 归约的名字本身就写明归约方式，`Source` 字段还会列出参与的 provider（`参与提供商: ip-api, ippure`）。这是有意的：`internal/ipquality` 明确「never synthesizes a combined score」，隐式合并会让规则含义随「当时启用了哪些 provider」漂移，显式命名则确定、可审计，且只启用一个 provider 时自然退化。

**`tags.auto` 故意不是规则字段。** 允许自动标签依赖自动标签会引入求值顺序依赖与非幂等。`tags.manual` 是运营输入而非引擎输出，所以开放，支撑「人工标记 VIP 且延迟 < 200ms」这类规则。若将来确有「标签的标签」需求，需要显式的、带环检测的分层求值，不能靠扩字段表悄悄加。

### 4.5 Loader

`Loader.Load(ctx, nodeIDs)`（`nil` = 全量）固定 **7 组批量查询**，与节点数无关：nodes / node_stats / detection / ip_quality / unlock /（subscription_nodes + subscriptions）/（manual node_tags + tags）。`internal/nodefacts/loader_test.go` 用计数假 Store 断言 1、10、1000 个节点的查询次数一致，作为 N+1 守卫。

## 5. 标签语义（`internal/nodetag`）

```text
model.go      Rule / TagMeta / Decision / ShadowNote / CompileRules
resolver.go   Resolve —— 互斥裁决，纯函数
service.go    Recompute / RecomputeAll / Preview / SeedTemplates / ValidateRule
templates.go  内置模板规则
queue.go      去抖合并的重算队列
```

`CompileRules(tags)` 返回 `(rules, meta, skipped)`：`meta` 覆盖**所有**标签（人工标签也要参与互斥裁决），`rules` 只含 `auto_enabled` 且规则可解析非空的标签。解析失败的行进 `skipped` 并被跳过而不是让整次重算失败 —— 规则在保存时已校验，存不下去的 JSON 只可能是绕过应用直改数据库，一行坏数据不该让所有节点停止打标。

### 5.1 互斥裁决

`Resolve(nodeID, matched, manualTagIDs, meta) Decision`：

1. 命中集按 `priority DESC, id ASC` 排序；
2. 每个互斥组只保留序列中的**第一个**命中，其余记 `ShadowNote{Reason: ReasonLowerPriority, WinnerTagID}`；
3. **人工标签在互斥组内优先**：该组若已有 manual 标签，组内所有 auto 候选一律不应用，记 `ReasonManualOccupiesGroup`。互斥组的不变量是「同节点最多一个」，运营意图高于规则；
4. 无互斥组的标签不受限制；
5. `Decision.TagIDs` 按 tag id 排序 —— 事实不变时重算产出字节一致，`ReplaceAutoNodeTags` 因此天然幂等。

### 5.2 Service

```go
Registry() / Limits() / ValidateRule(condition)
Recompute(ctx, nodeIDs) ([]int64, error)   // 返回投影真正变化的节点
RecomputeAll(ctx) ([]int64, error)
Preview(ctx, PreviewRequest) (*PreviewResult, error)
SeedTemplates(ctx) (*SeedResult, error)
```

`Store` 接口只声明服务真正需要的那一片：`nodefacts.Source` + `ListTagMutexGroups` / `CreateTagMutexGroup` / `CreateTag` / `ReplaceAutoNodeTags`。**没有任何写人工标签的方法** —— 服务结构上不可能改 manual 行。

`WithMembershipNotifier(func([]int64))` 只在 `projectionChanged` 为真时收到节点 ID：auto 集合变了但去重后的名称投影没变（同名标签换 ID、优先级调整后仍是同一批名字），分组成员不可能变，就不该惊动运行时。其它选项：`WithRegistry` / `WithLimits` / `WithUnlockProviders` / `WithClock` / `WithLogf`。

`Preview` 零写入。`PreviewRequest{Condition, TagID, MutexGroupID, Priority, NodeIDs, Limit}` 带上目标标签的互斥组与优先级，所以预览显示的是**保存后真会得到的**遮蔽结果。未保存规则用 `draftTagID = 1<<62` 参与裁决 —— 它比任何真实 ID 大，平局时按 `id ASC` 必输，宁可把遮蔽显示出来也不隐藏。`PreviewResult` 给全量口径的 `total_nodes` / `match_count` / `applied_count` / `shadowed_count` / `unknown_count`（未命中且缺至少一个规则读到的事实，这是「规则看起来坏了」的最常见原因），样本默认 50、上限 200，`PreviewNode` 只含 `node_id` / `name` / `region` / `matched` / `applied` / `shadowed` / `facts`，**没有 URI、用户名、密码**。

### 5.3 Queue

`DefaultDebounce = 2s`、`DefaultSweepInterval = 10min`、`DefaultRecomputeTimeout = 5min`、缓冲 1024。

- **非阻塞发送**：事件来自探测 goroutine。channel 满时置 `overflow`，下一次 flush 升级为全量重算 —— 降级而不是丢事件。
- **去抖 timer 而非固定 ticker**：每个事件重置 2s 静默期，一次 200 节点的检测扫描收敛成一次重算。
- **10 分钟周期兜底 `RecomputeAll`**：带 `max_age_seconds` 的规则会随时间改变答案，必须有时间驱动的东西；顺带覆盖不挂钩的常规健康探测。
- `EnqueueAll()` 用哨兵 `allNodes = 0` 走同一条 channel，保证与单节点请求的先后顺序。

### 5.4 内置模板

| `builtin_key` | 名称 | 规则 | 互斥组（priority） |
| --- | --- | --- | --- |
| `native_ip` | 原生IP | `unlock.ip.pure eq true` | — |
| `risk_high` | 高风险 | `unlock.ip.risk_level in ["High","Medium"]` | 风险等级（20） |
| `risk_low` | 低风险 | `unlock.ip.risk_level in ["Low"]` | 风险等级（10） |
| `latency_fast` | ⚡极速 | `latency_ms lte 100` | 延迟档（30） |
| `latency_ok` | ✅正常 | `latency_ms lte 300` | 延迟档（20） |
| `latency_slow` | 🐌较慢 | `latency_ms gt 300` | 延迟档（10） |
| `unlock_<provider>` | `<Label>解锁` | `unlock.<provider>.status eq "unlocked"` | — |

provider 列表来自 `unlock.ListProviderMetas()`，新增 checker 自动出现在模板与规则字段里。

`SeedTemplates` 幂等靠 `tags.builtin_key`，返回 `SeedResult{Created, Skipped, Conflicts}`。`Conflicts` 是名字被非模板标签占用的情况，**保持不动**：把运营手建的标签悄悄变成规则驱动的，会重打所有节点的标签。

**模板只由 `POST /api/tags/templates` 显式种入，迁移绝不自动建。** 升级时创建自动规则等于未经同意地改写所有节点的可见状态；migration 13 只负责保住已有标签。

## 6. 触发链路

`Server` 持有可选的 `retag RetagQueue`（`nil` 表示关闭自动打标，离线测试用），入口是两个永不阻塞、`nil`-safe 的方法：`enqueueRetag(nodeIDs...)`、`enqueueRetagAll()`。

**先拆掉的东西**：`persistUnlockResult` 原来在每次解锁检测后构造 `newTags`（原生IP / 高风险 / `<DisplayName>解锁`）再 `node.Tags = newTags` + `UpdateNode`，整表替换标签。这行是人工标签被删的根因，现在只落库解锁结果并 `enqueueRetag(nodeID)`（`server.go:1750`）。顺带修掉三个坏掉的地方：原逻辑依赖 `unlock.IPInfo.Pure`/`RiskLevel` 而 `probeExitIP` 从不填这两个字段（两个分支是死代码），标签字符串直接拼 `DisplayName`（与展示名耦合），真实 IP 质量事实其实在 `node_ip_quality_results`。

`internal/monitor/nodecheck.go` 的钩点，按事实落库的位置挂：

| 位置 | 事实 |
| --- | --- |
| `runLatency` 的 `UpsertNodeDetectionResult` 之后（:330） | `latency_ms` |
| `runSpeed` 之后（:387） | `speed_bps` / `speed_peak_bps` |
| `runExitIP` 之后（:456） | `exit_country_code` / `exit_ip_family` |
| `saveQuality`（:563，**所有** ipq 写入的唯一漏斗） | `ipq.*` |
| `run()` 末尾 `task.publish(done)` 之前（:285） | 整批兜底 |

`run()` 末尾那次整批 `enqueueRetag(task.nodeIDs()...)` 覆盖「某个阶段早早失败」的节点，而且不额外花钱 —— 队列会把它与已 pending 的合并。

`enqueueRetagAll()` 的入口：标签与互斥组 CRUD、`SeedTemplates`、订阅刷新成功（`server.go:2183`）、节点增删改与批量启停（`server.go:2418`–`2604`）。

**不挂常规健康探测**：`HealthResultEvent` 只有 `Tag` 没有 `NodeID`，为它加字段不值得；常规探测频率远高于标签需要变化的频率，其延迟本来也会经 `periodicStatsFlush`（30s）落到 `node_stats`，再由 10 分钟兜底扫到。

`internal/app/app.go` 的装配（§5b）：`newTagService(dataStore, notify)` → `nodetag.NewQueue(tagService)` → `server.SetTagService` / `server.SetRetagQueue` → `retagQueue.EnqueueAll()`。启动扫一遍是必要的：进程停着的时候事实也会变（节点被编辑、规则被导入）。`notify` 回调直接进 `boxMgr.ApplyGroupMembershipChanges`。unlock provider 列表从 `monitor.TagUnlockFactProviders()` 取，所以 `app.go` 不 import `unlock` 也不 import `nodefacts`，注册一个新 checker 不需要改这个文件。

## 7. 分组集成（`internal/groupmember`）

新包只依赖 `internal/config` 与 `internal/geoip`，无环。

```go
type Node struct { ID int64; Region string; Tags []string }
func NewFilter(groupCfg config.GroupPoolConfig, opts ...Option) Filter   // WithTagNames(map[int64]string)
func (f Filter) Allow(node Node) bool
func Nodes(cfg *config.Config, groupCfg config.GroupPoolConfig, opts ...Option) []config.NodeConfig
func NormalizeRegion(region string) string
```

`Allow` 的判定顺序，这是成员语义的唯一权威定义：

1. 分组停用 → 一律 false；
2. `excluded_node_ids` → 强制排除；
3. `explicit_node_ids` → 强制加入，**绕过白名单与黑名单**；
4. 否则须命中 region，再过白名单（`any`/`all`）与黑名单。

第 3 条是定案：`explicit` 本就是「强制加入」、`excluded` 是「强制排除」，若让黑名单压过 explicit，运营就没有开例外的手段了。空 region 归一为 `geoip.RegionOther`。

设计成**谓词**而不是「成员列表构造器」：builder 按 `memberTags` 顺序迭代（顺序决定 `members[0]`，即分组默认出口），boxmgr 按 `cfg.Nodes` 迭代。谓词让判定只有一份实现，同时不改变 builder 的成员顺序 —— 改顺序会静默改掉分组默认出口。

白/黑名单存的是标签 **ID**，节点携带的是标签**名**，所以要经 `config.Config.TagNames` 映射。ID 查不到名字时 `resolveTagName` 返回 `"\x00unresolved-tag-<id>"`：名字里带 NUL 字节，任何真实标签名都不可能等于它，于是**未知标签 ID 在白名单里永不命中、在黑名单里永不误伤**，而不是被静默忽略。`TagNames` 的填充点：启动时 `app.loadTagNamesFromStore`，运行期 `boxmgr.loadTagNames`。

替换掉的三处重复集合逻辑：`internal/builder/builder.go`（原 223-252）与 `internal/boxmgr/manager.go` 的 `groupMemberNodes`（原 456-483）改为经 `Filter.Allow` / `groupmember.Nodes`；`internal/monitor/group_subscription.go` 消费 `GroupRuntimeSnapshots()`，自动继承。这同时修掉一个既存分歧：builder 把空 region 视为 `other`、`groupMemberNodes` 不视为，导致 `regions:["other"]` 的分组漏重建。

### 7.1 成员增量落到运行时

`applyGroupRuntime` 的布尔 `force` 改成三值 `applyMode`。关键区别不是「重不重建」，而是**失败时怎么办**：

| 模式 | 语义 | 使用者 |
| --- | --- | --- |
| `applyModeDelta` | 分组定义未变则跳过重建 | 运营编辑单个分组 |
| `applyModeForceNoRollback` | 必重建，失败留停止态 | `syncGroupRuntimesAfterBaseReload` —— base box 已前进，旧 group box 就算想恢复也挂不上去 |
| `applyModeForceWithRollback` | 必重建，失败恢复旧 box | `ApplyGroupMembershipChanges` —— 分组定义没变，运行中的 listener 仍然有效，退回它严格优于丢掉它 |

`ApplyGroupMembershipChanges(ctx, changedNodeIDs)` 流程：去重 ID → **先读库再取锁**（新事实是判定依据，查询不该阻塞 reload）→ 取 `m.mu` 克隆 `before` → 对受影响节点刷新 `Tags`、`Region`、`Country` → 写 `cfg.TagNames` → 克隆 `after` → 释放锁 → 逐分组比较 `groupMemberRuntimeShape(before)` vs `(after)`，只对形状变化的分组以 `applyModeForceWithRollback` 重建。

三个要点：

- **base box 永不 reload**。全量 reload 会重建进程内所有 listener，掐掉没变化的分组的活连接，而节点事实的变化频率远高于分组定义。
- 刷新的是 `Tags` **和** `Region`/`Country` —— 落地地区也是成员事实，`nodecheck` 的地区变化走的就是这条路径，只刷 Tags 会让地区增量静默失效。
- 即使没有分组受影响也刷新缓存配置，否则下一次 reload 看到的还是旧标签。

`groupMemberRuntimeShape` 在拓扑比较里剥掉 `Tags`：成员集相同、只有标签文本变化的分组不该重建。

### 7.2 配置与读写贯通

`config.NodeConfig` 增 `Tags []string`；`config.GroupPoolConfig` 增 `TagWhitelist`/`TagBlacklist []int64` 与 `TagFilterMatch string`（均 `yaml:"-"`）；`config.Config` 增 `TagNames map[int64]string`。

必须同步的点，漏一个就出静默 bug：`GroupConfigsFromStore` / `storeGroupFromConfig` 双向搬运；**`groupRuntimeEqual` 必须比较这三个新字段**，否则只改白名单不会触发重建；`monitor.cloneGroupPool` 深拷贝两个新切片；`monitor.ManagedNodeConfig` 增 `ID` 与 `Tags` 并在 `boxmgr.ListConfigNodes` / `managedNodeConfig` 填充 —— 前端 `NodeInfo.tags` 那条自始至终拿到 `undefined` 的死分支，因此才活。

`groupFromInput` 的 `groupPoolInput` 增 `TagWhitelist *[]int64` / `TagBlacklist *[]int64` / `TagFilterMatch string`（指针可选，与 `ExcludedNodeIDs` 一致）。归一化：丢弃 `<=0`、保序去重、校验标签存在、白黑名单交集报错「标签白名单与黑名单不能包含同一标签」、`tag_filter_match` 缺省 `any` 且只接受 `any|all`。

## 8. HTTP API（`internal/monitor/tag_handlers.go`、`tag_schema.go`）

路由在 `server.go` 注册两条并各包 `s.withAuth`：`/api/tags`（精确）与 `/api/tags/`（前缀）。`handleTagItem` **先**分派字面子资源（`schema` / `preview` / `recompute` / `templates` / `assignments` / `mutex-groups` / `nodes`）**再** `ParseInt({id})` —— 否则 `/api/tags/schema` 会被当成标签 ID。

| 方法与路径 | 说明 |
| --- | --- |
| `GET /api/tags` | `{tags: tagView[], mutex_groups: []}` |
| `POST /api/tags` | 201 + `{tag}` |
| `GET /api/tags/schema` | 字段 / 操作符 / 枚举 / 限制 |
| `POST /api/tags/preview` | 试跑未保存规则，零写入 |
| `POST /api/tags/recompute` | `{node_ids?}` → `{changed_node_ids}` |
| `POST /api/tags/templates` | 幂等种入内置模板 → `SeedResult` |
| `GET /api/tags/assignments` | `?node_ids=1,2,3` → 每节点 manual/auto ID |
| `GET\|POST /api/tags/mutex-groups`、`PUT\|DELETE /api/tags/mutex-groups/{id}` | 互斥组 CRUD |
| `PUT /api/tags/nodes/{node_id}` | `{tag_ids}` 覆盖该节点人工标签集 |
| `POST /api/tags/nodes/batch` | `{node_ids, add_tag_ids, remove_tag_ids}` |
| `GET\|PUT\|DELETE /api/tags/{id}` | 单标签；`DELETE` 支持 `?force=1` |
| `PATCH /api/tags/{id}/auto` | `{auto_enabled}` |

节点人工标签挂在 `/api/tags/nodes/...` 而不是 `/api/nodes/...`，避免动 `handleNodeAction` 的前缀解析。

`tagView` = `store.Tag` 内嵌 + `rule` + `node_count` / `manual_count` / `auto_count` + `used_by_groups`。

### 8.1 校验

请求体统一 `http.MaxBytesReader(w, r.Body, 64<<10)`（`maxTagRequestBytes`）+ `decodeTagJSON`（严格字段 + 单对象）。规则再经 `Service.ValidateRule` → `Condition.Validate`，所以 HTTP 层不重复做体积/深度检查。其余：标签名 1..64 runes（`utf8.RuneCountInString`，允许 emoji）、去空格、重名 → **409**（`tagConflictError`）、`priority` 0..1000、互斥组必须存在、`node_ids`/`tag_ids` 必须为正且实体存在。

`if tag.AutoEnabled && tag.RuleJSON == ""` → 拒绝：启用自动打标却没有规则是无意义状态。

`DELETE /api/tags/{id}` 被分组引用时返回 409 + `{"error": "标签正被分组引用", "used_by_groups": [...]}`；`?force=1` 才删，并在同一事务内从两个 JSON 列清除该 ID，随后逐分组走 `applyGroupRuntimeMutation`（**不是** `TriggerReload`），失败信息收进 `runtime_errors`。

### 8.2 三个容易踩的实现细节

1. **清空规则要提交 `"rule": {}`，不是 `null`。** `tagInput.Rule` 是 `*nodefacts.Condition`，`nil` 表示「本次请求没带这个字段」；空对象经 `MarshalRule` 得到 `nil` 字节，才是「没有规则」。
2. **`tagEnumDefinition` 只有 `{options, free_input}`**，没有 `note` 字段。
3. **`GET /api/tags/assignments` 不带 `node_ids` 返回全部**（`ListNodeTags` 的 `NodeIDs: nil` = 全量），不是空集。

还有一处时序：`POST /api/tags` 与改规则的 `PUT /api/tags/{id}` 触发的是**异步** `enqueueRetagAll()`，响应里的 `node_count` 因此是旧值；而**改名**走同步 `Recompute` + `refreshGroupMembership`（投影里存的是名字，必须立刻改），返回的计数是准的。

### 8.3 Schema 端点

`GET /api/tags/schema` → `{version, limits, operators, field_groups, fields[], enums{}}`。枚举来源如实标注：unlock provider/status 取 `unlock.ListProviderMetas()` / `ListStatusMetas()`（与 `/api/nodes/unlock-meta` 同源，新增 checker 不必改代码）；ipquality provider 固定 `ippure` + `ip-api`；protocol = 固定 8 种 ∪ 节点 URI 里实际出现的 scheme；region = `geoip.AllRegions()` ∪ `nodes.region` 去重；country_code 取检测结果里的 `exit_country_code`；subscription 取 `ListSubscriptions`。

`region`、`country_code`、`risk_level` 标 `free_input: true` —— `geoip.AllRegions()` 只有 `[jp kr us hk tw other]`，没有完整国家表，前端必须允许自由输入 ISO code。

## 9. 前端

`frontend/src/components/TagsPanel.tsx` + `components/tags/`：`TagList`、`TagEditorDrawer`、`ConditionBuilder`、`ConditionRow`、`conditionDefaults.ts`、`MutexGroupManager`、`TagPreviewPanel`、`TagBadge`、`TagMultiSelect`、`NodeTagPicker`。

`App.tsx` 四处 + 一个 import：`TabId` 增 `'tags'`、`MENU_ITEMS` 在 `manage` 之后插入「节点标签」（`lucide-react` 的 `Tags`）、`VALID_TABS` 增 `'tags'`、`renderContent()` 增 `case 'tags'`。

`ConditionBuilder` 完全由 schema 驱动：字段下拉按 `field_groups` 分组、操作符按该字段的 `operators` 过滤、值控件按 `kind` 切换（enum → 下拉，region/country 允许自由输入；int → 数字 + 单位；bool → 开关；string → 文本），`supports_max_age` 的字段额外给事实时效输入，嵌套深度不超过 schema 的 `max_depth`。常量放 `conditionDefaults.ts` 而不是组件文件 —— 组件文件同时导出非组件会触发 `react-refresh/only-export-components`。

查询键沿用仓库的扁平约定：`['tags']`、`['tagSchema']`、`['tagMutexGroups']`、`['tagAssignments']`。**Preview 不进缓存**：按 draft 变化去抖 400ms 直接调用，用 `AbortController` 取消旧请求。写操作沿用 `GroupPoolsPanel` 的 `run(key, task, message)` + `busy` + `sonner`，每次标签写入失效 `['tags']`、`['nodes']`、`['groupPools']`。

集成改动：`GroupPoolsPanel` 的 `emptyPayload()`/`payloadFromGroup()` 必须带上 `tag_whitelist`/`tag_blacklist`/`tag_filter_match`（漏了直接 TS 编译失败），新增「标签筛选」字段与 `GroupCard` 的标签徽章；`ManagePanel` 每行 `TagBadge` + `NodeTagPicker`；`UnlockPanel` 只加一行说明「原生IP / 解锁 标签现由标签规则产生」。`types/index.ts` 的 `NodeSnapshot`/`ConfigNodeConfig` 需要 `tags?: string[]`（`ManagedNodeConfig` 补 `ID`+`Tags` 就是为了它）。注意 `verbatimModuleSyntax`：类型导入必须 `import type`。

## 10. 安全与数据保护

- **任何标签响应都不含节点 URI、用户名、密码。** `PreviewNode` 只回 `node_id` / `name` / `region` / `matched` / `applied` / `shadowed` / `facts`，且 `facts` 只包含规则真正引用到的字段。
- 正则限长 200 并在保存与预览时 `regexp.Compile`；值列表 ≤100 项；AST ≤3 层 / ≤50 条 / ≤16KiB；请求体 ≤64KiB。
- 日志不记录带凭据的 URI；`Service` 的 `logf` 只输出节点 ID 与标签 ID。

## 11. 测试

| 文件 | 钉住的东西 |
| --- | --- |
| `internal/nodefacts/evaluate_test.go` | 每个操作符的已知值；**未知对 12 个观测操作符一律 false**；`max_age_seconds` 把已知转未知；`latency_ms == -1` 未知；`initial_check_done=0` 时 `available` 未知；unlock `failed` 是**已知**值；`ipq.max` 全未知才未知；`negate` 在未知短路之后 |
| `internal/nodefacts/condition_test.go` | 拒绝超限/不可编译正则/未知字段/字段-操作符不兼容；JSON 往返不变 |
| `internal/nodefacts/loader_test.go` | 1/10/1000 节点查询次数固定（N+1 守卫）；>900 分块；无检测行 → 全未知 |
| `internal/nodetag/resolver_test.go` | `priority DESC, id ASC`；互斥组内 manual 抑制全部 auto 且原因为 `manual_occupies_group`；同输入多次运行输出一致；幂等 |
| `internal/nodetag/service_test.go` | **全量重算不动 manual 行**（防覆盖回归）；auto 行整体替换；`nodes.tags` = manual ∪ auto 排序并集；只算子集不影响其它节点；`notify` 只收真正变化的 ID |
| `internal/nodetag/queue_test.go` | 同 ID 入队 100 次收敛成一次；`EnqueueAll` 覆盖 pending；channel 打满升级全量而不丢；`Close()` 后 `Enqueue` 不 panic；无事件也会兜底 |
| `internal/store/tags_test.go` | **12→13 回填**（emoji 完整），同时是 `json_each` 可用性闸门；`ReplaceAutoNodeTags` 失败整体回滚；同对双来源；`DeleteTag` 级联并清两个分组 JSON 列；`DeleteTagMutexGroup` 只置空 `mutex_group_id` |
| `internal/store/node_identity_test.go` | 去重时 `node_tags` 迁移到胜出节点并重建投影 —— **缺这条标签会在每次 `Open()` 消失** |
| `internal/groupmember/filter_test.go` | 与旧 builder / 旧 `groupMemberNodes` 的等价性对照；`excluded > explicit > region`；白名单 any/all；黑名单不拒 explicit |
| `internal/boxmgr/membership_test.go` | 只重建成员集变化的分组；`applyModeForceWithRollback` 失败时恢复旧 box；base box 永不 reload |
| `internal/monitor/tag_handlers_test.go` | 路由分派；超大 body / 非法规则 / 未知字段 → 400；重名 → 409；schema 覆盖每一个 provider；preview 无写入；响应不含凭据 |
| `internal/monitor/unlock_tagging_test.go` | `persistUnlockResult` 不再写 `nodes.tags` 而是入队；人工标签在解锁检测后存活 |

闸门命令：`gofmt -l`、`go vet ./...`、`go test ./... -count=1`、`go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" ./cmd/easy_proxies`、`cd frontend && npm run lint && npm run build`。`-race` 需要 cgo，本环境不可用，用 `-count=2` 代替。

## 12. 运维与升级说明

1. **升级后不会有任何自动标签**，直到有人调 `POST /api/tags/templates` 或自己写规则。这是刻意的（见 §5.4），但必须写进发布说明。已有的 `nodes.tags` 字符串会被 migration 13 转成人工标签，不会丢。
2. **解锁检测不再写标签**。原来那三类自动标签（原生IP / 高风险 / `<name>解锁`）现在由模板规则产生，种入模板后行为等价且更准（`原生IP` 终于读的是 `ip_pure` 而不是从不填充的字段）。
3. **`nodes.tags` 是投影列**，标签层之外的写入会被下一次重算覆盖。
4. 想立刻看到效果：`POST /api/tags/templates` → `POST /api/tags/recompute`（不带 `node_ids` 即全量）。
5. `regions: ["other"]` 的分组会在升级后重建一次 —— 空 region 归一修掉了 boxmgr 侧的漏重建（§7）。

## 13. 已知边界

- **没有「标签的标签」**：`tags.auto` 不是规则字段（§4.4）。
- **Preview 成本**是一次全节点事实装载，样本上限 200（计数仍覆盖全部节点），前端去抖 400ms。节点数上到数千再考虑给事实加短 TTL 快照缓存。
- **改标签名**对分组安全（引用 ID），但会改 `nodes.tags` 投影，所以 `UpdateTag` 在名称变化时同步全量重算，标签多且节点多时这个请求会明显变慢。
- **`json_each` 依赖** SQLite JSON1（`modernc.org/sqlite` 具备），由 `internal/store/tags_test.go` 的 12→13 回填测试守住。
