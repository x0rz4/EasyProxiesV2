# 子系统：事实、地区与成员资格（L0/L2）

改动 `internal/nodecodec`、`internal/noderegion`、`internal/nodeloc`、`internal/groupmember`、`internal/nodeselect`、`internal/nodeview`、`internal/nodefacts`、`internal/nodetag`、`internal/runtimenodes` 之前先读这份。

## 节点身份（nodecodec）

`nodecodec.ParseURI` 是**协议白名单与身份归一的唯一真源**：URI/Clash → `Identity`/`CanonicalJSON`/`Hash`。`config.supportedProxyURISchemes` 必须与它保持一致，否则出现"`IsProxyURI` 放行但 `ParseURI` 拒绝"的矛盾提示。`ssr://`、hysteria v1（`hysteria://`）明确不支持。支持协议：vmess / vless / ss / trojan / hysteria2 / hy2 / anytls / http / socks5。

## 地区语义（noderegion + nodeloc）

- `noderegion`：国家码输入/展示/版本化桶语义。**空 = unknown，`other` 是查询桶、绝不是国家码**。国家码归一唯一落在此包（本轮 geoip 清理后）。
- `nodeloc`：**落地地区判据唯一真源**。规则："一次失败或空观测**绝不抹掉已知地区**"。地区驱动成员资格，误清会把节点踢出其区域组（并进 other 桶）。只有成功的、点名国家的观测才改地区。只依赖 `noderegion`（无持久化/运行时），供所有写路径共享而不成环。
- **落地区域只能来自拨测后的出口 IP**，禁止从 URI 的服务器地址推断（护栏 5）；`builder` 与 `groupmember` 两处归一必须一致。

## 成员判定（groupmember + nodeselect + nodeview）

`nodeselect` 是纯成员谓词（无持久化/运行时依赖）；`groupmember.Filter` 是它对外唯一封装（builder / boxmgr 共用）。判定顺序是契约：

```
组未启用 → excluded_node_ids 无条件排除 → explicit_node_ids 无条件加入（绕过黑白名单）
        → region 命中 + tag 黑名单/白名单（any/all）
```

未解析的 tag ID 变成**永不匹配的哨兵**（宁可缩小，不许悄悄放宽）。预览（nodeview）与实际构建（groupmember）必须共用同一谓词，禁止自写两套条件。

## 运行时节点集合（runtimenodes）

`runtimenodes.Load` 是**运行时节点集合唯一组装点**（护栏 14）：enabled 非订阅 + enabled 订阅可达，按 identity 去重。启动 / 订阅 reconcile / `TriggerReload` 三条路径必须走同一函数。任何"只按 `enabled` 过滤"的本地版本都会让停用订阅的节点重新进入运行时。`TriggerReload` 读 store 失败必须返回错误，不得降级成"只有内联节点"的半套配置。

## 事实引擎与标签（nodefacts + nodetag）

- `nodefacts`：纯事实/条件引擎（store 行 → 类型化 fact、字段注册表 `Registry`、条件 DSL `Condition` 与求值）。**不认 tag、不 import unlock/ipquality**（provider 经 `ProviderInfo` 注入）。
- `nodetag`：tag 语义与调度（规则编译、互斥组仲裁、manual 覆盖 auto、重算队列 2s 防抖 + 10min sweep、落库、内置模板）。

**自动标签不能依赖自动标签**（护栏 7）：条件注册表刻意不含 `tags.auto`（只暴露 `tags.manual`），否则重算非幂等。

标签重算 → 分组增量重建链：

```
检测落库 → enqueueRetag(nodeID) → nodetag.Queue（2s 防抖，10min sweep）
  → Service.Recompute（CompileRules → nodefacts.Loader.Load → Resolve → ReplaceAutoNodeTags）
  → notify（仅"可见 tag 名集合变化"的节点）
  → boxmgr.ApplyGroupMembershipChanges → 只重建成员集合真正变化的组（base box 从不 reload）
```

## 标签语义区分

分组过滤存 **tag ID**（tag 改名不影响分组）；节点身上的标签是**名字**。两者由 `groupmember.NewFilter` 的 `WithTagNames` 桥接。`nodes.tags` 只是 `node_tags` 的**只读投影**（由 `refreshNodeTagProjection` 重写），改标签必须走 store 的 tag API（`SetManualNodeTags` / `ReplaceAutoNodeTags`），旁路写入会被下次重算覆盖。
