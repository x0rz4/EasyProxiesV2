# 分组筛查规则页面实现方案

> 状态：设计提案 —— **部分已被 `plane/node-tagging-system.md` 取代并落地**  
> 产品目标：规则筛查 → 自动入组 → 分组独立端口 → 由 Xboard 配置展示名与倍率  
> 前置依赖：现有分组池、节点探测、解锁检测；运行时应用建议衔接 `plane/incremental-hot-reload.md`

> **取代关系**：§1 的三档商品目标已由节点打标系统达成，路径不同 —— 规则挂在**标签**上，分组通过「标签白名单 / 黑名单」间接筛查，没有独立的 `screening_rules` 引擎。已被取代的章节：§7.5（`member_source` → 三个 tag 列）、§8.1（标签来源 → `node_tags.source` + `nodes.tags` 投影）、§10.1（`ResolveEffectiveGroupMembers` → `groupmember.Filter.Allow`）。§6 的规则 JSON 模型与 §9 的引擎语义以 `internal/nodefacts` 的形式落地，本文档其余部分作为「若将来真要做独立筛查规则实体」的设计参考保留。

## 1. 目标

新增“筛查规则”页面和后端规则引擎，让运营人员能够按地区、延迟、节点标签、IP 类型、风险、ASN、解锁结果等条件筛选节点，并将命中节点自动分配到指定分组池。

典型产品映射：

| 规则 | 目标分组 | 入口端口 | EasyProxies 职责 | Xboard 职责 |
| --- | --- | ---: | --- | --- |
| 香港基础 | 香港基础池 | 10001 | 提供符合基础条件的可用出口 | 展示“香港 01 [基础]”，倍率 1.0x |
| 香港 VIP | 香港 VIP 池 | 10002 | 提供低延迟、原生、解锁出口 | 展示“香港 02 [VIP]”，倍率 2.0x |
| 游戏专线 | 游戏专线池 | 10003 | 提供最低延迟及高稳定出口 | 展示“游戏专线 01”，倍率 3.0x |

倍率不进入 EasyProxies 的流量计算。EasyProxies 只负责节点质量事实、规则分类、分组成员和稳定入口。

## 2. 当前仓库基础与缺口

### 2.1 可直接复用

- `group_pools`、`group_node_states` 及完整分组池 CRUD。
- 分组独立监听地址、端口、协议、认证和 fixed/random 调度。
- 节点 `region`、`country`、`tags`、启用状态。
- `node_stats.last_latency_ms`、available、blacklisted 等健康数据。
- `node_unlock_results` 中的服务解锁状态、出口 IP、ASN、组织、IP 类型、风险等级。
- group runtime 的 ALIVE/SUSPECT/EVICTED、失败窗口和当前出口。
- React 19、TanStack Query、Tailwind CSS 4、DaisyUI、Lucide、Sonner 及统一 `PageLayout`。

### 2.2 必须补齐

- 没有筛查规则、规则版本、运行记录和派生成员表。
- ~~当前成员计算是 `regions ∪ explicit_node_ids - excluded_node_ids`，无法区分规则成员与人工成员。~~ **已解决**：成员判定收敛到 `internal/groupmember.Filter.Allow`，并接受标签白/黑名单；规则成员与人工成员的区分落在 `node_tags.source` 上。
- ~~当前 unlock 自动标签会整体覆盖 `node.Tags`，人工标签和探测标签没有来源隔离。~~ **已解决**（打标系统 Phase 3）：`persistUnlockResult` 不再写标签，只入队重算；`node_tags` 主键含 `source`，`ReplaceAutoNodeTags` 只删 `source='auto'`。见 `plane/node-tagging-system.md` §3.2、§6。
- 丢包率、抖动、p50/p95 延迟暂未形成持久化质量事实。
- ~~group 配置变化当前触发完整 box reload；规则重算不能逐节点触发 reload。~~ **已解决**：`boxmgr.ApplyGroupMembershipChanges` 只重建成员集变化的分组 box，base box 永不 reload。
- multi-port/group 路由仍是静态构建，批量成员变化要和增量热重载方案协同。

## 3. 产品边界与首期范围

### 3.1 P1 支持

- 条件：地区、当前延迟、可用状态、黑名单状态、节点名称、人工标签。
- 动作：分配到一个 group。
- 规则按 priority 升序执行，ID 作为同优先级的稳定次序。
- 默认互斥入组：节点只进入首条命中的 assign_group 规则目标。
- 创建、编辑、复制、启停、删除、排序。
- 草稿试跑、命中预览、入组/离组 diff。
- 手动全量重算和指定节点增量重算。
- 显示最近运行结果、命中数和“规则已修改但尚未应用”状态。

### 3.2 P2/P3 扩展

- IP 类型、原生 IP、ASN allow/deny、风险等级。
- Netflix、Disney+、ChatGPT、YouTube 等解锁条件，支持 all/any。
- 事实新鲜度要求，例如解锁结果不超过 24 小时。
- 多组模式、`add_tag`、`exclude` 动作。
- 丢包、抖动、p50/p95、连续可用时长等质量窗口。
- 定时重算和探测完成事件触发的增量重算。

## 4. 页面信息架构

在主导航增加：

```text
节点监控
订阅管理
节点管理
节点标签    ← 已实现，打标规则与互斥组（plane/node-tagging-system.md）
筛查规则    ← 新增，策略中心
分组池      ← 执行状态与入口
解锁检测
调试面板
系统设置
```

“筛查规则”和“分组池”保持独立页面：

- 筛查规则页回答：为什么某节点进入某个质量档。
- 分组池页回答：这个质量档当前有哪些运行成员、入口端口是否正常、谁是当前出口。
- 节点质量看板后续回答：一个节点有哪些事实、命中了什么规则、当前属于哪些组。

## 5. 页面布局

页面沿用现有 data-dense dashboard 风格，不引入独立配色或字体体系。使用 DaisyUI 语义色 `base-*`、`primary`、`success`、`warning`、`error`，保证全部主题下可读；图标统一使用 Lucide。

```text
┌──────────────────────────────────────────────────────────────────────┐
│ 筛查规则  自动按质量事实分配节点       [重算全部] [新建规则]          │
├──────────────────────────────────────────────────────────────────────┤
│ 启用规则 5 │ 已分配 128 │ 未分类 23 │ 待应用变更 2 │ 最近运行 3m 前   │
├──────────────────────────────────────────────────────────────────────┤
│ 搜索…  [状态▼] [目标分组▼] [互斥模式]             [运行记录]         │
├──┬────┬────────────┬──────────────────────┬──────────┬──────┬───────┤
│序│启用│规则名称     │条件摘要               │目标分组   │命中数│操作   │
├──┼────┼────────────┼──────────────────────┼──────────┼──────┼───────┤
│↕ │ ●  │游戏专线     │延迟 ≤40ms AND tag=game│游戏 10003 │ 12   │…      │
│↕ │ ●  │香港 VIP    │HK AND 原生 AND 解锁   │VIP 10002  │ 31   │…      │
│↕ │ ●  │香港基础     │HK                    │基础 10001 │ 48   │…      │
└──┴────┴────────────┴──────────────────────┴──────────┴──────┴───────┘
```

### 5.1 顶部摘要

- 启用规则数。
- 当前被规则分配的节点数。
- 未命中任何规则的可用节点数。
- 尚未应用的规则变更数或 ruleset version 差值。
- 最近一次运行状态、耗时和时间。

摘要用于暴露系统状态，不放趋势图；本页核心是规则管理和结果解释。

### 5.2 工具栏

- 搜索：名称、条件摘要、目标组；使用 `useDeferredValue` 或 200–300ms debounce。
- 状态过滤：全部、启用、停用、待应用、错误。
- 目标分组过滤。
- 互斥/多组模式展示；P1 只允许互斥，控件可只读并说明。
- “重算全部”：先展示预计影响范围，再确认执行。
- “新建规则”：打开右侧编辑抽屉。

### 5.3 规则列表

每行显示：

- 拖拽手柄和 priority。
- enabled switch。
- 名称和可选说明。
- 条件摘要，例如 `地区 ∈ HK · 延迟 ≤ 80ms · Netflix=解锁`。
- 目标分组名称、端口、分组启用状态。
- 当前命中数和上次运行相对时间。
- 状态 badge：已应用、待应用、运行失败、目标组停用。
- 操作：编辑、试跑、复制、启停、删除。

拖拽之外必须提供“上移/下移”菜单和键盘操作，不能把拖拽作为唯一排序方式。排序提交失败时恢复原顺序并 toast 提示。

### 5.4 编辑抽屉

桌面端宽度建议 720–800px；移动端全屏。分为四段：

1. 基本信息：名称、说明、启用状态、priority。
2. 匹配逻辑：顶层 ALL/ANY，条件行列表。
3. 动作：assign_group、目标分组、互斥行为说明。
4. 执行保护：事实新鲜度、缺失值行为、是否排除 EVICTED。

底部固定操作栏：

```text
[取消] [保存草稿] [试跑规则] [保存并应用]
```

新规则默认停用。删除、启用一个大范围规则、保存并应用产生大量离组时，必须二次确认。

### 5.5 条件构建器

条件行采用受控表单：

```text
[字段▼] [操作符▼] [值/多选/范围输入] [事实时效] [删除]
```

字段选择后只展示兼容操作符和值控件：

| 字段 | 操作符 | 值控件 | 缺失事实默认行为 |
| --- | --- | --- | --- |
| region | in / not_in | 国家/地区多选 | 不匹配 |
| latency_ms | lt/lte/gt/gte/between | 数字 + ms | 不匹配 |
| available | eq | 开关 | 不匹配 |
| blacklisted | eq | 开关 | 不匹配 |
| name | regex/contains | 文本 | 不匹配 |
| manual_tags | contains_any/all | 标签多选 | 不匹配 |
| ip_type | in/not_in | 枚举多选 | 不匹配 |
| native_ip | eq | 开关 | 不匹配 |
| asn | in/not_in | ASN token input | 不匹配 |
| risk_level | in | Low/Medium/High | 不匹配 |
| unlock.* | eq/in | unlocked/locked/... | 不匹配 |

除显式 `is_unknown`/`not_exists` 外，缺失或过期事实一律不命中，包括 `not_in`。这样未知质量节点不会因为否定条件意外进入 VIP 池。

### 5.6 试跑与预览

试跑不保存、不改成员、不 reload。返回：

- 扫描节点数、命中数、事实缺失数。
- 相对当前已应用 assignments 的 `would_enter`、`would_leave`、`would_stay`。
- 与更高优先级规则冲突、因此最终不会入组的数量。
- 最多 100 条节点明细，支持分页或服务端游标。
- 每个节点的 matched conditions、failed conditions、missing/stale facts。

预览表不得只显示“匹配/不匹配”，必须能解释原因：

```text
香港-A：命中（region=HK；latency=38ms）
香港-B：未命中（Netflix 结果已过期 31h）
香港-C：条件命中，但已被优先级 10“游戏专线”占用
```

### 5.7 页面状态

- 首次加载：表格 skeleton，不使用整页 spinner。
- 空状态：解释规则用途，并提供“从模板创建基础/VIP/游戏规则”。
- 请求错误：页内 alert + 重试；不能只 toast。
- 预览中：保留旧预览并显示 updating，避免内容跳空。
- 后台运行：按钮显示进度，允许离开页面，轮询 run 状态。
- 目标分组被删除/停用：规则行显示错误 badge，规则不执行。
- 页面有未保存表单时关闭抽屉或切页需确认。

## 6. 规则 JSON 模型

使用版本化条件 AST，而不是把每种条件固定成数据库列：

```json
{
  "id": 12,
  "name": "香港 VIP",
  "description": "原生、低延迟且 Netflix 解锁",
  "enabled": true,
  "priority": 20,
  "condition": {
    "version": 1,
    "logic": "all",
    "items": [
      { "field": "region", "operator": "in", "value": ["hk"] },
      { "field": "latency_ms", "operator": "lte", "value": 80, "max_age_seconds": 1800 },
      { "field": "native_ip", "operator": "eq", "value": true, "max_age_seconds": 86400 },
      { "field": "unlock.netflix", "operator": "eq", "value": "unlocked", "max_age_seconds": 86400 }
    ]
  },
  "action": {
    "type": "assign_group",
    "group_id": 2
  },
  "version": 4,
  "created_at": "...",
  "updated_at": "..."
}
```

服务端维护字段注册表，定义类型、允许操作符、值校验和事实读取器。前端可以从静态 TypeScript 定义开始，后续增加 `GET /api/screening-rules/schema` 以避免前后端枚举漂移。

Go 的 regexp 使用 RE2，可避免灾难性回溯，但仍需限制表达式长度，并在保存/预览时提前编译验证。

## 7. 数据库设计

追加 migration，建议包含以下表和字段。

> **版本纠正**：本文档写作时的最大 migration 版本是 8，写成「migration 9」。此后仓库演进到 **12**，节点打标系统占用 **13**（`add node tagging system`）。若将来真要建独立的 `screening_rules` 实体，从 **14** 起。

### 7.1 screening_rules

```sql
CREATE TABLE screening_rules (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    enabled        INTEGER NOT NULL DEFAULT 0,
    priority       INTEGER NOT NULL,
    condition_json TEXT NOT NULL,
    action_json    TEXT NOT NULL,
    version        INTEGER NOT NULL DEFAULT 1,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE INDEX idx_screening_rules_order
ON screening_rules(enabled, priority, id);
```

priority 不要求唯一；执行顺序固定为 `priority ASC, id ASC`。拖拽重排时服务端在事务内重新写成 10、20、30……，为后续插入留空间。

### 7.2 screening_assignments

规则成员属于派生数据，不能写入 `explicit_node_ids_json`：

```sql
CREATE TABLE screening_assignments (
    node_id         INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    group_id        INTEGER NOT NULL REFERENCES group_pools(id) ON DELETE CASCADE,
    rule_id         INTEGER NOT NULL REFERENCES screening_rules(id) ON DELETE CASCADE,
    ruleset_version INTEGER NOT NULL,
    matched_at      TEXT NOT NULL,
    PRIMARY KEY (node_id, group_id)
);

CREATE INDEX idx_screening_assignments_group
ON screening_assignments(group_id, node_id);
```

P1 互斥模式由 engine 保证每个 node 只有一条规则 assignment；未来多组模式允许一个 node 对多个 group 各有一条。

### 7.3 screening_rule_runs

```sql
CREATE TABLE screening_rule_runs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    trigger          TEXT NOT NULL,
    status           TEXT NOT NULL,
    ruleset_version  INTEGER NOT NULL,
    scanned_count    INTEGER NOT NULL DEFAULT 0,
    matched_count    INTEGER NOT NULL DEFAULT 0,
    entered_count    INTEGER NOT NULL DEFAULT 0,
    left_count       INTEGER NOT NULL DEFAULT 0,
    unchanged_count  INTEGER NOT NULL DEFAULT 0,
    missing_count    INTEGER NOT NULL DEFAULT 0,
    error            TEXT NOT NULL DEFAULT '',
    started_at       TEXT NOT NULL,
    finished_at      TEXT NOT NULL DEFAULT ''
);
```

运行历史保留最近 N 条，具体值可配置，默认 100。详细节点理由只在 preview 返回；运行记录只存汇总和最终 rule/group，避免数据库无限增长。

### 7.4 screening_settings

单行配置：

```sql
CREATE TABLE screening_settings (
    id                  INTEGER PRIMARY KEY CHECK (id = 1),
    assignment_mode     TEXT NOT NULL DEFAULT 'exclusive',
    ruleset_version     INTEGER NOT NULL DEFAULT 0,
    applied_version     INTEGER NOT NULL DEFAULT 0,
    auto_apply          INTEGER NOT NULL DEFAULT 0,
    updated_at          TEXT NOT NULL
);
```

任何规则 CRUD、启停或重排都递增 `ruleset_version`。成功提交 assignments 后更新 `applied_version`。两者不一致时 UI 显示“待应用”。

### 7.5 group_pools.member_source

> **已被取代。** migration 13 没有加 `member_source`，而是加了三列：
>
> ```sql
> ALTER TABLE group_pools ADD COLUMN tag_whitelist_json TEXT NOT NULL DEFAULT '[]';
> ALTER TABLE group_pools ADD COLUMN tag_blacklist_json TEXT NOT NULL DEFAULT '[]';
> ALTER TABLE group_pools ADD COLUMN tag_filter_match   TEXT NOT NULL DEFAULT 'any';
> ```
>
> 没有 `manual` / `rules` / `hybrid` 三态：成员始终是 `region ∪ explicit − excluded`，标签白/黑名单只是在 region 分支上再加一道过滤，空白名单等于不过滤。这样默认值天然向后兼容，也不需要在三种模式之间做语义解释。白/黑名单存标签 **ID**（改名安全），`tag_filter_match ∈ {any, all}`。判定顺序见 `plane/node-tagging-system.md` §7。
>
> 「人工覆盖导致多组」这层担忧同样换了形式：`explicit_node_ids` **绕过**白/黑名单（强制加入就该是强制加入），互斥只发生在标签层（`tag_mutex_groups`），且互斥组内人工标签压过所有自动标签。

以下为原设计，保留作参考：

增加：

```sql
ALTER TABLE group_pools
ADD COLUMN member_source TEXT NOT NULL DEFAULT 'manual';
```

值定义：

- `manual`：保持现状，`regions ∪ explicit - excluded`。
- `rules`：`assignments ∪ explicit - excluded`。
- `hybrid`：`regions ∪ assignments ∪ explicit - excluded`。

其中 explicit 始终表示人工强制加入，excluded 始终表示人工强制排除。默认 `manual` 保证升级后行为不变。

互斥规则只约束规则生成的 assignments。若人工强制加入导致一个节点出现在多个规则组，UI 必须标记“人工覆盖导致多组”，不能伪装成规则冲突。

## 8. 质量事实模型

新增 `internal/screening.Facts`，一次批量加载所有评估输入：

```go
type Fact[T any] struct {
    Value     T
    Known     bool
    CheckedAt time.Time
}

type NodeFacts struct {
    NodeID       int64
    Name         string
    Region       Fact[string]
    LatencyMs    Fact[int64]
    Available    Fact[bool]
    Blacklisted  Fact[bool]
    ManualTags   Fact[[]string]
    IPType       Fact[string]
    NativeIP     Fact[bool]
    ASN          Fact[string]
    RiskLevel    Fact[string]
    Unlock       map[string]Fact[string]
}
```

FactLoader 必须批量读取 nodes、node_stats、node_unlock_results 和必要的 group states，禁止按节点 N+1 查询。

### 8.1 标签来源修正

> **已定论并落地（方案一的变体）。** 最终做法：
>
> - `node_tags(node_id, tag_id, source, ...)`，主键含 `source ∈ {manual, auto}`。这是**结构性**保证 —— `ReplaceAutoNodeTags` 只发 `DELETE ... WHERE node_id=? AND source='auto'`，一次解锁检测在物理上不可能删掉人工标签。
> - `nodes.tags` 保留，但降级为 `manual ∪ auto` **名称**的去重排序**投影列**，由标签层在同一事务内改写。所以 `store.Node.Tags`、`ManagedNode.Tags`、`mergeNodeMetadata` 以及所有现有读点零改动，运行期分组筛选也不用 join。
> - `persistUnlockResult` 里那段 `node.Tags = newTags` + `UpdateNode` 已删除，改为 `enqueueRetag(nodeID)`。原逻辑还顺带修掉三个 bug：依赖从不被填充的 `unlock.IPInfo.Pure`/`RiskLevel`（两个分支是死代码）、标签字符串与 `DisplayName` 耦合、真实 IP 质量事实其实在 `node_ip_quality_results`。
> - 投影刷新**不走 `UpdateNode`**，避免标签抖动刷新 `nodes.updated_at` 污染身份对账与订阅 diff。
>
> 细节见 `plane/node-tagging-system.md` §3.2、§3.3、§6。以下为当时的两个候选：

当前 unlock 检测会用自动标签整体覆盖 `node.Tags`。引入规则前应二选一：

1. 推荐：将 nodes.tags 明确为人工标签，新增 derived tags 表或直接从 unlock result 动态计算探测标签。
2. 过渡：标签规则 P1 仅匹配人工管理 API 写入的 tags，unlock 条件直接读取 unlock result，不依赖自动标签字符串。

不能继续让一次 unlock 检测删除运营人员手工添加的 `game`、`isp` 等标签。

### 8.2 缺失与过期语义

- `Known=false`：除 `is_unknown` 外均为 false。
- `max_age_seconds` 超时：按 Unknown 处理。
- latency `-1`：Unknown。
- unlock `failed` 是已知失败结果，不等于 Unknown，也不等于 unlocked。
- 目标 group 的 EVICTED 可作为 terminal filter，但不能当作节点在其他 group 中永久不可用。

## 9. 规则引擎

新增包：

```text
internal/screening/
  model.go       规则、条件、动作、Facts
  schema.go      字段与操作符注册表
  evaluator.go   单节点纯函数匹配
  planner.go     priority、互斥、多组及 diff
  service.go     preview/apply/run orchestration
  facts.go       批量事实加载
```

核心接口：

```go
type Evaluator interface {
    Evaluate(rule Rule, facts NodeFacts, now time.Time) Evaluation
}

type Service interface {
    Preview(ctx context.Context, draft Rule, filter PreviewFilter) (Preview, error)
    Apply(ctx context.Context, req ApplyRequest) (Run, error)
}
```

### 9.1 互斥匹配伪代码

```go
sort rules by priority ASC, id ASC

for each enabled node {
    facts := factsByNode[node.ID]
    for _, rule := range enabledRules {
        result := evaluator.Evaluate(rule, facts, now)
        if !result.Matched {
            continue
        }
        switch rule.Action.Type {
        case AssignGroup:
            assignments[node.ID] = Assignment{
                NodeID: node.ID,
                GroupID: rule.Action.GroupID,
                RuleID: rule.ID,
            }
            goto nextNode
        case Exclude:
            goto nextNode
        }
    }
nextNode:
}
```

P1 不让 `add_tag` 参与同一轮事实输入，避免规则 A 写标签后改变规则 B 的结果。未来支持时也应将动作统一在评估完成后提交。

### 9.2 原子应用

1. 读取 ruleset version、规则、groups 和 facts 快照。
2. 在数据库事务外完成纯计算，得到完整候选 assignments 和 diff。
3. 开启事务，重新检查 ruleset version。
4. version 未变化时，以 staging/replace 方式提交 assignments，并写 run 结果和 applied version。
5. version 已变化则放弃提交，记录 superseded，不覆盖新规则。
6. 事务提交后只触发一次 group runtime reload/update。

规则执行失败不能留下部分 assignments。运行时 reload 失败时数据库 assignments 已是期望状态，应将 run 标记为 `applied_runtime_failed`，保留重试入口，而不是悄悄回滚数据库。

### 9.3 自动触发与合并

事件来源：

- 订阅刷新完成：对新增、删除和参数变化节点重算。
- latency probe 完成：仅重算对应节点。
- unlock 检测完成：仅重算对应节点。
- 规则或目标 group 修改：全量重算。
- 周期任务：处理事实过期和恢复。

所有事件进入有界队列，按 2–5 秒窗口合并 node IDs；全量事件覆盖同窗口内增量事件。任意时刻最多一个 apply run，后来的请求标记 pending，不并发写 assignments。

## 10. 分组池集成

### 10.1 Effective membership

> **已被取代。** 「统一成员判定」这条原则保留并落地了，但形态是**谓词**而不是「按 groupID 返回成员列表」：
>
> ```go
> // internal/groupmember
> func NewFilter(groupCfg config.GroupPoolConfig, opts ...Option) Filter
> func (f Filter) Allow(node Node) bool
> func Nodes(cfg *config.Config, groupCfg config.GroupPoolConfig, opts ...Option) []config.NodeConfig
> ```
>
> 为什么是谓词：builder 按 `memberTags` 顺序迭代（顺序决定 `members[0]`，即分组默认出口），boxmgr 按 `cfg.Nodes` 迭代。谓词让判定只有一份实现，同时不改变各调用方的成员顺序 —— 改顺序会静默改掉分组默认出口。它也不需要 `ctx`/`store`，可以在纯内存的配置快照上跑，`ApplyGroupMembershipChanges` 的「旧成员 vs 新成员」比较就是这么做的。
>
> 判定顺序（定案）：分组停用 → `excluded` 强制排除 → `explicit` 强制加入且**绕过白/黑名单** → 否则 region + 白名单(any|all) + 黑名单。空 region 归一为 `geoip.RegionOther`。替换掉的三处重复实现：`builder.go`、`boxmgr.groupMemberNodes`、以及 `monitor/group_subscription.go`（后者消费 `GroupRuntimeSnapshots()`，自动继承）。见 `plane/node-tagging-system.md` §7。
>
> EVICTED 语义不变：它是组内运行状态，不影响成员身份。

以下为原设计：

新增 store/service 层方法统一计算有效成员，builder、API 和 runtime 禁止各自重复实现集合逻辑：

```go
ResolveEffectiveGroupMembers(ctx, groupID) ([]int64, error)
```

计算顺序：

```text
manual base 或 rule assignments 或二者并集
→ 加 explicit 强制成员
→ 减 excluded 强制排除
→ 减 disabled/不存在节点
→ 保留 EVICTED 成员身份，但 runtime 不选用
```

EVICTED 是组内运行状态，不应删除 assignment；恢复后无需等下一次规则重算即可重新参与。

### 10.2 API 展示

`GET /api/groups` 的 member response 增加：

```json
{
  "membership_source": "rule",
  "matched_rule_id": 12,
  "matched_rule_name": "香港 VIP",
  "manual_override": false
}
```

分组编辑表单增加 member_source。选择 rules 时：

- 地区条件区域折叠为“旧版兜底条件”，默认不参与。
- 节点多选改名为“人工强制加入”。
- 排除列表继续作为“人工强制排除”。
- 提供跳转到“为此分组创建规则”。

### 10.3 Runtime 应用

短期可由规则 apply 成功后批量触发一次现有 reload；这会继承当前完整 box reload 的中断问题。

推荐在 `incremental-hot-reload.md` 的 group snapshot 阶段完成后，将 assignment diff 原子发布给 group pool，仅对成员增删进行热更新。无论采用哪种路径，都禁止每匹配一个节点就 reload 一次。

## 11. HTTP API

建议新增 handler 文件 `internal/monitor/screening_handlers.go`，避免继续扩大 `server.go`。

### 11.1 规则 CRUD

```text
GET    /api/screening-rules
POST   /api/screening-rules
GET    /api/screening-rules/{id}
PUT    /api/screening-rules/{id}
DELETE /api/screening-rules/{id}
PATCH  /api/screening-rules/{id}/enabled
POST   /api/screening-rules/reorder
GET    /api/screening-rules/schema
```

列表响应同时返回 summary、settings、groups 摘要和 latest_run，减少页面首次加载 waterfall。

### 11.2 Preview 与 Apply

```text
POST /api/screening-rules/preview
POST /api/screening-rules/apply
GET  /api/screening-rules/runs
GET  /api/screening-rules/runs/{id}
```

Preview 接受未保存 draft；Apply 接受 `expected_ruleset_version`，版本不一致返回 409。

apply 任务超过普通 HTTP 时限时返回 202 和 run ID：

```json
{
  "run_id": 81,
  "status": "queued",
  "ruleset_version": 17
}
```

前端通过 TanStack Query 轮询运行状态。已有 SSE 基础也可复用，但首期轮询更简单可靠。

### 11.3 Preview 响应示例

```json
{
  "scanned_count": 231,
  "matched_count": 31,
  "missing_fact_count": 8,
  "would_enter": 5,
  "would_leave": 2,
  "would_stay": 26,
  "shadowed_count": 4,
  "nodes": [
    {
      "node_id": 101,
      "name": "香港-A",
      "matched": true,
      "final_assignment": true,
      "reasons": ["region=hk", "latency_ms=38 ≤ 80"],
      "failed": [],
      "missing": []
    }
  ]
}
```

所有写 API 使用现有 auth middleware、严格 JSON 单对象解析、请求大小限制和统一错误结构。

## 12. 前端文件计划

```text
frontend/src/
  App.tsx
  api/client.ts
  types/index.ts
  components/
    ScreeningRulesPanel.tsx
    screening/
      RulesTable.tsx
      RuleEditorDrawer.tsx
      ConditionBuilder.tsx
      ConditionRow.tsx
      RulePreviewPanel.tsx
      RuleRunHistory.tsx
      RuleTemplates.tsx
```

### 12.1 App.tsx

- `TabId` 增加 `rules`。
- 菜单加入“筛查规则”，图标使用 `ListFilter` 或 `GitBranch`。
- hash route 为 `#rules`。
- 不使用 emoji 作为新页面功能图标。

### 12.2 React Query

建议 query keys：

```ts
['screeningRules']
['screeningRules', 'runs']
['screeningRules', 'run', runId]
['screeningRules', 'preview', draftHash]
```

Mutation 成功后精确失效规则、groups 和相关 nodes 查询。排序使用 optimistic update；规则保存、apply 不做盲目 optimistic assignment 更新。

编辑器使用受控 state；搜索使用 deferred value。preview 请求加 AbortController 或 request sequence，后返回的旧请求不能覆盖新 draft 的结果。

### 12.3 组件复用

- 页面容器复用 `PageLayout`、`PageHeader`、`PageContent`、`surfaceClass`、`controlClass`。
- 表单、badge、drawer、modal、skeleton 使用 DaisyUI。
- toast 只报告瞬时成功/失败；持续错误和运行状态留在页面内。
- 目标分组选择项显示 `名称 · 端口 · 启用状态 · member_source`。

## 13. 可访问性与响应式

- 所有输入均使用真实 label，placeholder 不能替代 label。
- switch 同时显示文本状态，不依赖颜色。
- 抽屉打开后 focus trap，关闭后焦点回到触发按钮。
- 条件删除按钮有包含字段名的 aria-label。
- drag handle 支持键盘上移/下移并通过 live region 宣布新位置。
- hover 不使用缩放造成布局跳动；交互过渡保持 150–300ms。
- 支持 `prefers-reduced-motion`。
- 375px：规则列表变为卡片，编辑器全屏，固定底栏按钮允许换行。
- 768px：摘要 2×2/2×3，preview 在编辑器下方。
- 1024px 以上：表格 + 右侧抽屉；最大内容宽度沿用 1600px。
- 明暗及 DaisyUI 全主题使用语义色，正文对比度至少 4.5:1。

## 14. 安全与数据保护

- preview/apply 不返回节点 URI、用户名或密码，只返回 node ID、展示名和质量事实摘要。
- name regex 限长并在服务端编译验证。
- ASN、tag 和枚举数组限制项目数；整个 condition AST 限制深度和条件数，例如最多 3 层、50 条。
- apply 使用 ruleset version 乐观并发控制。
- reorder、save、enable、delete 与 apply 共用规则 mutation lock 或数据库版本检查。
- 目标 group 删除建议使用 RESTRICT：存在规则引用时提示先迁移或停用规则；不能静默级联删除规则。
- 日志不记录带凭据 URI，也不记录完整 condition 中可能存在的敏感人工标签。

## 15. 实施阶段

> **实际执行的是打标系统的 8 个阶段**，P0–P5 的对应关系如下。**三档商品目标在 Phase 7 结束即达成，不需要 `screening_rules` 引擎。**
>
> | 本文档 | 打标系统实际阶段 | 说明 |
> | --- | --- | --- |
> | P0 模型与纯引擎 | Phase 1 store 基座（migration 13 + `sqlite_tags.go` + 批量事实读） / Phase 2 引擎（`internal/nodefacts` + `internal/nodetag`，离线） | 引擎包名是 `nodefacts`/`nodetag` 而不是 `screening`；`Fact[T]` 三态与未知语义原样落地 |
> | P1 规则 API 与派生成员 | Phase 6 HTTP API（`/api/tags*`） / Phase 4 统一成员判定（`internal/groupmember`） | 没有 `assignments` 表与 `ruleset_version`/`applied_version`：规则挂在标签上，`node_tags` 就是派生成员，`rule_version` 承担版本追踪 |
> | P2 页面 | Phase 7 前端（`TagsPanel` + `components/tags/*`） | 导航项叫「节点标签」；三档商品在分组池页面用标签白/黑名单配置 |
> | P3 质量维度 | Phase 2（unlock / ipq / ASN / risk / freshness 字段） / Phase 3 触发（探测完成后按 node ID 增量重算） | 来源隔离在 Phase 1 就靠 `node_tags.source` 结构性解决，不是 P3 的补救项 |
> | P4 稳定性与热更新 | Phase 3 队列（去抖 2s / 兜底 10min） / Phase 5 成员增量（`applyMode` 三值 + `ApplyGroupMembershipChanges`） | 成员变化不再触发完整 box reload，且不依赖 `incremental-hot-reload.md` 的第一阶段 |
> | P5 高级质量规则 | **未做** | 丢包 / 抖动 / p50 / p95 仍未持久化；`add_tag`/`exclude` 动作在标签模型里不存在（标签本身就是「加标签」这个动作） |
>
> Phase 8 是文档：新增 `plane/node-tagging-system.md` 并修订本文档。

### P0：模型与纯规则引擎

- migration 9、store 接口与 SQLite 实现。
- `internal/screening` AST、schema、facts、evaluator 和 planner。
- region/latency/available/blacklisted/name/manual_tags 条件。
- preview 和 evaluator 单元测试，不接运行时。

### P1：规则 API 与派生成员

- CRUD、reorder、preview、apply、runs API。
- assignments 原子替换、ruleset/applied version。
- group `member_source` 和统一 effective membership resolver。
- 手动 apply 后只触发一次 group reload。

### P2：页面

- 导航、规则列表、编辑抽屉、条件构建器、preview、运行记录。
- 基础/VIP/游戏三个模板。
- 分组池页面显示成员来源和命中规则。
- 引入 Vitest + React Testing Library，覆盖表单、排序、预览竞态和错误状态。

### P3：质量维度

- 修复 manual/derived tags 来源隔离。
- 接入 unlock、IP 类型、native、ASN、risk 条件和 freshness。
- 探测完成后按 node ID 增量重算。
- 增加事实缺失/过期看板。

### P4：稳定性与热更新

- 定时重算、事件合并队列、失败重试和运行指标。
- 对接 group immutable snapshot，消除成员变化导致的完整 box reload。
- 增加 Xboard 三档商品配置示例文档。

### P5：高级质量规则

- 丢包、抖动、p50/p95、稳定在线时长。
- 多组模式、exclude、add_tag。
- 规则模板导入导出和审计历史。

## 16. 测试计划

### 16.1 后端单元测试

- ALL/ANY、每种 operator、Unknown/Expired 语义。
- priority + ID 稳定排序及互斥首命中。
- unlock failed 不会当作 unlocked 或 unknown。
- `not_in` 遇到 Unknown 不会错误命中。
- disabled 节点不产生 assignment。
- explicit/excluded override 优先级。
- EVICTED 保留成员身份但不被 runtime 选择。
- ruleset version 变化时 apply 被拒绝且不产生部分写入。
- 目标 group 停用、删除、非 rules source 时的校验。

### 16.2 Store 与 API 测试

- ~~migration 8 → 9 保留原 group 行为，member_source 默认为 manual。~~ 实际是 **12 → 13**：三个 tag 列的默认值（`'[]'`/`'[]'`/`'any'`）保证原 group 行为不变，并额外用 `json_each(nodes.tags)` 回填已有标签为 `node_tags(source='manual')`。见 `internal/store/tags_test.go`。
- CRUD、reorder、409 version conflict、preview 无副作用。
- apply 事务失败后旧 assignments 完整保留。
- 删除 rule/group/node 时外键行为符合设计。
- API 不泄露 URI 和凭据。

### 16.3 前端测试

- 新建规则默认停用、必填校验和条件控件切换。
- preview 的旧响应不能覆盖新 draft。
- 拖拽/键盘排序失败能回滚。
- 保存草稿与保存并应用行为区分明确。
- loading、empty、error、pending apply、runtime failed 状态均可见。
- 375/768/1024/1440px 无横向溢出。
- 键盘完成创建、编辑、试跑和应用流程。

### 16.4 端到端验收

1. 创建三个分组池和基础/VIP/游戏规则。
2. 导入 HK 普通、HK VIP、低延迟 game、未知质量四类节点。
3. 试跑准确显示各节点命中/缺失/被高优先级遮蔽原因。
4. 保存并应用后，三个端口的 effective members 与 preview 一致。
5. 更新某节点 latency/unlock 后只重算该节点，产生正确入组/离组 diff。
6. 手工排除一个规则成员后，后续重算不会把它重新加入。
7. 规则运行时失败不影响旧 assignments 和已有入口服务。

## 17. 验收标准

- 运营人员无需编辑 YAML 或节点 ID 即可创建、排序、试跑和应用规则。
- 任何入组结果都能追溯到规则和事实，未知/过期事实不会进入高质量池。
- 互斥模式下每个节点最多有一个规则 assignment。
- 规则派生成员与人工强制加入/排除互不覆盖。
- 一次规则运行只产生一次 runtime 更新，不按节点 reload。
- 规则保存和应用具备版本控制，不会被并发请求静默覆盖。
- 旧数据库升级后现有 group membership 行为不变。
- 页面在现有全部主题和目标响应式宽度下可用，键盘操作完整。

## 18. 需要在编码前确认的决策

1. P1 是否只允许互斥模式；本方案建议是。
2. 目标 group 的 `member_source` 是否由绑定规则时自动切成 rules；建议要求用户确认，不静默修改。
3. 保存启用规则是否自动 apply；建议默认不自动，明确使用“保存并应用”。
4. inline 节点无数据库 ID 时如何进入 assignments；当前实际 reload 会把 SQLite ID 合并回配置，规则引擎应只对已持久化节点工作。
5. 人工标签的管理入口和 derived tags 拆分方案。
   **已定论**：拆分靠 `node_tags.source`（`manual` / `auto` 可并存于同一 (node, tag)），`nodes.tags` 降级为两者名称并集的投影列。人工标签的管理入口是 `PUT /api/tags/nodes/{node_id}`（覆盖该节点人工标签集）与 `POST /api/tags/nodes/batch`（批量增删），前端在「节点管理」页每行的 `NodeTagPicker`。自动标签只由规则产生，运营不能手改；互斥组内人工标签压过所有自动标签。见 `plane/node-tagging-system.md` §3.3、§5.1、§8。
6. 第一版 runtime 是暂时完整 reload，还是等 group 增量 snapshot 一起上线；为避免新增功能放大中断，建议至少做到一次规则运行只 reload 一次，并尽快接增量路径。

以上决策确认后，可以按 P0 → P1 → P2 并行拆分后端模型、API 与前端静态页面，但 assignments 接入 group runtime 必须等 effective membership resolver 完成后再合并。

参考https://github.com/ZeroDeng01/sublinkPro/blob/main/docs/features/tags.zh-CN.md 的打标系统。