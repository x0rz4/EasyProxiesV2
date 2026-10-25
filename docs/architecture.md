# 当前架构总览

> 本文件描述**此刻代码的真实形态**，是"改代码前先读"的第一份文档。
> 权威行为契约见 [plane/specs/proxy-pool/spec.md](../plane/specs/proxy-pool/spec.md)（MUST/场景）；
> 变更提案与历史见 [plane/](../plane/)。本文件只讲"现在是怎么搭的"，不写"应该怎么改"。
>
> 基线核对：v2.2.6，migration 至 v24，`internal/` + `cmd/` 约 **39.3k** 行非测试代码、约 **21.3k** 行测试，
> 前端 `frontend/src` 约 **10.1k** 行 TS/TSX。核对日期 2026-10-23。

## 1. 一句话说清这个系统

单一 Go 二进制承载三件事：**数据面**（监听 / 选路 / 拨号）、**控制面**（HTTP API + 内嵌 WebUI）、**持久化**（单个 SQLite 文件）。sing-box **以库的形式内嵌**（不是子进程），本仓库在它之上实现了一个多池代理管理与自愈层。

进程内没有分布式协调，所有"状态"要么在内存快照里、要么在 SQLite 里。理解系统的关键，是理解**同一份事实在三条写路径（启动 / 增量重载 / 全量蓝绿）里必须只有一个来源**。

技术栈：

| 层 | 技术 |
| --- | --- |
| 后端 | Go 1.27（`go.mod`: `go 1.27.0` + `toolchain go1.27.1`）、标准库 `net/http`（Go 1.22+ `ServeMux`） |
| sing-box | `github.com/sagernet/sing-box v1.13.x` + `sing`、`sing-tun`、`sing-quic`、`sing-vmess`，按可选 tags 编译 |
| 存储 | SQLite（`modernc.org/sqlite`，**纯 Go，CGO_ENABLED=0 可交叉编译**）+ 手写 migration |
| JSON | 统一走 `internal/jsonx`（默认 Sonic；需要确定性字节序时用 `MarshalCanonical`） |
| GeoIP | `oschwald/geoip2-golang/v2`（mmdb 首次启动自动下载，可热更新） |
| 前端 | React 19 + TypeScript + Vite 7 + Tailwind CSS 4 + DaisyUI 5 + TanStack Query + lucide-react + sonner |

## 2. 仓库布局

```
cmd/easy_proxies/main.go      入口：加载 config → app.Run
internal/                     全部后端包（见 §4）
frontend/src/                 React 源码（components/ api/ types/ utils/）
internal/httpapi/assets/      前端构建产物，被 //go:embed assets/* 内嵌（gitignore，禁止手改）
docs/                         当前形态、硬规则、子系统地契、贡献者流程（本目录）
LESSONS.md                    复盘错题本（Seen 3× 转正机制）
plane/README.md               规格入口：当前契约 specs/、变更计划 changes/、专题与历史记录
config.example.yaml           配置模板（复制为 config.yaml 使用；config.yaml 不入库）
Dockerfile / Dockerfile.ci    本地全量构建 / CI 只带产物的运行时镜像
docker-compose.yml            GHCR 镜像 + 数据卷（EASY_PROXIES_VERSION 可指定版本）
install.sh                    Linux Docker / 二进制安装及更新入口
tests/test_install.py         安装脚本单测
.github/workflows/            打 tag 触发：多架构二进制 + Docker 镜像发布
```

## 3. 装配根：`app.Run`

`cmd/easy_proxies/main.go` 只做一件事：加载 `config` → `app.Run(ctx, cfg)`。`app.Run` 是唯一的装配点，顺序固定：

```
GeoIP 预热 → 开 SQLite → 从 store 载入 nodes/groups/tags（store 覆盖 config）
  → 建 boxmgr（冷启动 base box + 每启用分组一个 box）
  → 建标签服务 + 重算队列 → 订阅管理器 → 统计落库 → 优雅退出
```

三个 `load*FromStore` 是"数据库优先于 config.yaml"这一规则的落点：config 里的内联节点 / 分组 / 标签会被库里的同键数据覆盖，`yaml:"-"` 字段则**只来自 store**。

## 4. `internal/` 包分层（30 个可导入包，按数据流分组）

> 本节是 AGENTS.md 路由表指向的"模块地图"。上一版地图漏了 5 个已存在的包：`nodeloc`、`noderegion`、`nodeselect`、`nodeview`、`probe`——它们都从旧的 `monitor`/`boxmgr` 里拆出来承载单一职责，本文件补全它们。

### 装配与基础（叶子之上）

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `app` | 唯一装配点、优雅退出、`groupStatePersister`（把 `GroupStateEvent` 落库）、成员对账合并队列、Healer 后端 | `Run()`、`load*FromStore`、`membership_queue.go` |
| `config` | YAML 加载/校验、节点 URI 与订阅内容解析、`NodeConfig.NodeKey()`、重载路径 `LoadForReload` | `Load` / `LoadForReload` / `ParseSubscriptionContent` / `ParseImportContent` |
| `jsonx` | JSON 编解码唯一出口（Sonic + `MarshalCanonical`） | `Marshal` / `Unmarshal` / `MarshalCanonical` |
| `netx` | 经节点 outbound 拨号的 HTTP client/transport 唯一构造点；**叶子包，不 import 本项目任何包** | `NewClient(dial, Options)` / `NewTransport` / `CloseIdleConnections` |

### 节点身份与视图（L0 事实）

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `nodecodec` | **协议感知的节点身份唯一来源**：URI/Clash → `Identity`/`CanonicalJSON`/`Hash`；协议白名单真源是 `ParseURI`，`config.supportedProxyURISchemes` 必须与它一致 | `ParseURI` → `Result`、`FromClashProxy`、`FallbackHash` |
| `noderegion` | 国家代码输入/展示/版本化桶语义。**空 = unknown，`other` 是查询桶、绝不是国家码**（本轮 geoip 清理后，国家码归一唯一落在此包） | `Normalize` / `Name` / `Codes` / `Version` |
| `nodeloc` | **落地地区判据唯一真源**：一次失败或空观测**绝不抹掉已知地区**（地区驱动成员资格，误清会把节点踢出其区域组）。无依赖，供所有写路径共享 | `Observation` / `Location` / `Normalize`、`state.go` |
| `runtimenodes` | **运行时节点集合唯一组装点**：enabled 非订阅节点 + 经 enabled 订阅可达的 enabled 节点，按 identity 去重 | `Load(ctx, store)` / `Convert` / `IdentityKey` |

### 成员资格与标签（L0→L2）

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `nodeselect` | **纯成员谓词**（无持久化/无运行时依赖）：查询视图与 `groupmember` 共用同一套判定 | `Filter.Allow` / `Spec` / `Decision` |
| `groupmember` | 成员判定的**唯一对外实现**（builder 与 boxmgr 共用），内部即 `nodeselect.Filter` 的封装 + region 归一 | `NewFilter().Allow()` / `Nodes()` / `NormalizeRegion()` |
| `nodeview` | 在**一个不可变快照**上用同一成员谓词查询管理视图；不做任何网络/SQL；诊断预设走事实 DSL | `Spec` / `View` / `DiagnosticCondition` |
| `nodefacts` | 纯事实/条件引擎：store 行 → 类型化 fact、字段注册表、条件 DSL 与求值。**不认 tag、不 import unlock/ipquality**（provider 经 `ProviderInfo` 注入） | `Registry` / `Condition` / `Loader` / `Operators` |
| `nodetag` | tag 语义与调度：规则编译、互斥组仲裁、manual 覆盖 auto、重算队列（2s 防抖 + 10min sweep）、落库、内置模板 | `Service.Recompute` / `Queue` / `Resolve` |
| `runtimetag` | 出站 tag 的唯一天命名空间：`<16位hex>@v<generation>` | `Format(nodeKey, version)` / `InitialVersion` |

### 数据面（L3，代理运行）

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `builder` | config+store → sing-box `option.Options`：出站（池/多端口/分组/GeoIP）、入站、route | `BuildVersion` / `BuildBaseVersion` / `BuildGroupVersion` / `BuildNodeOutbound` |
| `boxmgr` | sing-box 实例生命周期与"代"管理；冷启动 / 增量 / 全量蓝绿三条路径；旧实例排空；实现 `monitor.NodeManager`/`GroupRuntimeManager`；持有 `*httpapi.Server` | `Start` / `Reload` / `reloadNodesIncrementally` / `ApplyGroupRuntime` / `ApplyGroupMembershipChanges` |
| `outbound/pool` | 代理池调度：不可变快照上的无锁选路、成员可用性、黑名单、延迟排序、失败记账与两阶段提交 | `newPool` / `PrepareUpdate`/`Commit` / `publishSnapshotLocked` / `shared_state.go` |
| `group` | 分组**成员状态机**：滑动失败窗口、ALIVE/SUSPECT/EVICTED、踢出全局传播与自动恢复、状态事件；Healer 规划与容量选择 | `Register` / `RecordFailure` / `RecordHealthSuccess` / `ReconcileSilent` / `PlanHealing` / `CapacitySelection` |

### 运行时与健康平面（L1）

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `monitor` | **运行时注册表 + 探测调度 + 健康事件 hub + 流量采样**。对外暴露**具体类型**（`Manager`/`EntryHandle`/`NodeInfo`/`Snapshot`/`HealthResultEvent`）供 pool 与 boxmgr 直接用；**不含 HTTP/路由/Session/DTO**（已迁 httpapi），**不 import boxmgr/pool**（反向允许）。跨层消费方接口**不在这里**，在 `controlplane/ports.go` | `node_registry` / `probe_scheduler` / `health_event_hub` / `health_applier` / `speedtest` / `tag_providers` |
| `probe` | **测量事件的唯一应用链**：网络执行留在各 scheduler/nodedetect，这里只做判据与落库前的收据/幂等/身份门禁；只有 `HealthProbe` 来源携带健康语义 | `Applier`、`Kind`/`Origin`/`HealthResult`；`applier.go` |

### 能力检测（L0/L4 输出）

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `noderesult` | 检测结果负载**唯一**定义（叶子包）。store 嵌入这些类型加元数据，`unlock`/`ipquality` 用 `type` 别名指回 → 出 API 不需逐字段转换 | `Quality` / `Unlock` / `Service` / `IPInfo` |
| `nodedetect` | 手动触发的路由无关诊断：延迟/测速/出口 IP。**绝不发布健康事件** | `DialFunc` + 诊断入口 |
| `unlock` | 流媒体与 AI 服务解锁检测，`init()` 注册式。结果类型是 `noderesult` 别名 | `Checker` / `RegisterChecker` |
| `ipquality` | IP 质量 provider 适配（ippure / ip-api），每个 provider 结果独立、不做综合评分 | `Provider` / `IPPureProvider` / `IPAPIClient` |
| `geoip` | mmdb 下载/更新 + 地区查询（`AllRegions` / `RegionName` / `RegionEmoji` 供 builder 与标签 schema 复用）。国家码本身委托 `noderegion` | `Lookup.LookupIP` / `EnsureDatabase` / `DownloadDatabase` |

### 订阅与输出（L4）

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `subscription` | 上游订阅拉取与刷新、节点归属变更；实现 `monitor.SubscriptionRefresher` | `Refresh` / `CommitSnapshot` / `RefreshForHealing` |
| `subrender` | 把节点 URI 渲染成 Clash/Mihomo 订阅文档与入口代理（无运行时依赖，供 `/sub/*`） | `Render` |

### 控制面（L4）

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `controlplane` | **领域服务与跨层契约**：`Auth`（会话/登录限流/API Key 凭据）、`Settings`（配置读写、校验、持久化、ApplyPlan，是所有配置写入的唯一序列化点）、领域错误。**只依赖 config/store**，不 import monitor/httpapi | `Auth` / `Settings` / `ports.go` |
| `httpapi` | **管理面 HTTP 层**：Server 生命周期、路由表、全部 handler/DTO、响应管线、会话认证、SSE、静态资源与 `//go:embed assets/*`。import monitor 读运行态、直连 store 做持久化 | `routes.go` / `response.go` / `server*.go` / `*_handlers.go` |

### 持久化（L5）

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `store` | **唯一持久化层**：`Store` 接口（当前 **108** 个方法）+ 手写 migration（append-only，当前最新 **v24**）+ 单写连接 / WAL / 四读连接 | `Open` / `Migrate`、`sqlite.go`、`sqlite_tags.go`、`sqlite_location.go`、`health_events.go`、`node_identity.go`、`probe_id.go` |

## 5. 依赖方向（2026-10-23 由 AST 级 import 扫描实测，修正了旧文档多处失实）

```
cmd → app

app → boxmgr config geoip group monitor nodecodec nodetag probe runtimenodes store subscription
boxmgr → builder config controlplane geoip group groupmember httpapi monitor netx nodecodec
         nodedetect nodeloc outbound probe runtimenodes runtimetag store
httpapi → config controlplane geoip group ipquality jsonx monitor nodecodec nodedetect nodefacts
          nodeloc noderegion noderesult nodeselect nodetag nodeview probe store subrender unlock
monitor → config nodecodec nodedetect nodefacts probe store unlock        ← unlock/ipquality→nodefacts 的桥接层
builder → config geoip group groupmember nodecodec outbound runtimetag    ← 不 import store（旧文档错）
config → group jsonx netx nodecodec
store → group jsonx nodecodec nodeloc noderesult                          ← group 值对象是持久化词汇表；group 不 import store
subscription → boxmgr config controlplane group runtimenodes store        ← 对 boxmgr 是具体 import（listener 类型）
controlplane → config store
groupmember → config noderegion nodeselect
nodeview → nodefacts noderegion nodeselect；nodeselect → noderegion；nodeloc → noderegion
nodefacts → jsonx noderegion store；nodetag → nodefacts store；probe → jsonx nodeloc store
outbound → group monitor
unlock → geoip jsonx netx noderesult；ipquality → jsonx netx nodedetect noderesult
nodedetect → netx；geoip → noderegion；runtimenodes → config store；subrender → nodecodec；nodecodec → jsonx

真叶子（不 import 项目内其它包）：jsonx netx noderesult noderegion runtimetag
```

**判据**：跨层接口由 `controlplane` 定义，`boxmgr` / `subscription` import 它并实现，`httpapi` import 它并消费。`monitor` 只持有运行态具体类型（`Manager`/`EntryHandle`/`Snapshot`/`HealthResultEvent`），**不**定义这些接口——历史上曾在 `monitor/ports.go`，`1e05e17`（2026-10-17）迁至 `controlplane/ports.go`，旧 AGENTS 未跟改（见 LESSONS L-017）。`monitor` 从不 import `httpapi`/`boxmgr`/`pool`。

## 6. 三条热重载路径（`boxmgr`）——必须共享同一节点集合

要重建的**节点集合只有一个来源**：`runtimenodes.Load`。config 内联节点按 `NodeKey` 优先并回填库内元数据，匹配不到的才追加。**任何路径都不许另写 `enabled` 过滤**（历史 bug：`TriggerReload` 只过滤 `enabled`，导致停用订阅的节点在任意节点增删后被拉回运行时——AGENTS 护栏 14）。

1. **冷启动** `Start`：base box + 每个启用分组一个独立 box。
2. **增量** `reloadNodesIncrementally`：URI 未变则复用旧 tag/outbound，变更节点用新 generation 生成新 tag → `pool.CreateMember` → `PrepareUpdate`/`Commit` → 旧成员异步排空。仅在"未 multi-port、未 GeoIP、其余配置一致"时可用。
3. **全量蓝绿** `reload`：`waitForConnectionsToDrain` → 关旧 box → 等端口释放 → `generation+1` 重建全部 → `SweepStaleNodes`。

tag 代际不可覆盖：同一 tag 重复 `Create` 会当场关掉旧 outbound、无法排空，故变更节点必须换新 `@vN`。

## 7. 一次连接的数据流（L3）

```
inbound → pool 取 PoolSnapshot（不可变）→ 按 mode 选成员 → 拨号 → 成功/失败回写
失败：普通模式 sharedMemberState（阈值→临时黑名单）
      分组模式 group.RecordFailure（滑动窗口→SUSPECT/EVICTED，按 NodeID 全局传播到所有分组）
被踢成员仍在周期探活；冷却 + 连续成功 N 次后 RecordHealthSuccess 自动恢复（恢复同样全局）
```

**调用方取消/超时不得计入成员失败**（`dialFailureAttributable`），否则客户端断开风暴会误拉黑健康节点——见 LESSONS L-003。

## 8. 控制面 API 与认证

- `/api/*` 需认证（`management.password` 为空则免密；Cookie `session_token` 或 `Authorization: Bearer`，会话 24h 存 SQLite）。
- `/sub/{groupID}` 公开，但用分组 `SubscriptionToken` 常量时间比较。
- 免登录公共 API（`POST /api/public/subscriptions`、`/api/public/import`）由 `withAPIKey` 保护，接受 `X-API-Key`/`Bearer`/`?api_key=`，**故意不接受会话 Cookie**；路由见 `internal/httpapi/routes.go`。
- 错误→状态码是**三个策略不是一个**（见 AGENTS 护栏 12）：`nodeErrorStatus`（类型驱动）、`subscriptionErrorStatus`（消息驱动，未识别→500）、`tagErrorStatus`（消息驱动，未识别→400），故意不一致。

## 9. 前端（React 19 + TS + Vite 7 + Tailwind 4 + DaisyUI 5）

源码 `frontend/src/`（约 10k 行），`components/` `api/` `types/` `utils/`。构建产物直接落到 `internal/httpapi/assets/`，被 `//go:embed assets/*` 内嵌进二进制 → 单文件运行。**没有产物时 `go build ./...` 必失败**——先跑前端构建（AGENTS 构建顺序陷阱）。无单测框架，验收标准 = `npm run lint` + `npm run build` 通过。

## 10. 默认端口与关键常量

共享池入口 `2323`、管理面板 `9888`、multi-port 起始 `24000`。标签重算防抖 `2s` + 全量 sweep `10min`。Healer 预算见 [spec.md](../plane/specs/proxy-pool/spec.md) Requirement HEAL-002。

## 11. 构建与开发命令

```bash
# 后端（生产 tags：uTLS / QUIC / gRPC / WireGuard / gVisor）
go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies

# 本地运行（先 cp config.example.yaml config.yaml）
go run -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" ./cmd/easy_proxies --config ./config.yaml

# 全量测试
go test ./...

# 前端
cd frontend && npm ci
cd frontend && npm run dev                     # Vite 开发服务器
cd frontend && npm run lint && npm run build    # ESLint + tsc -b + vite build（产物直接落到 internal/httpapi/assets）

# 容器
docker compose up -d --build
```

交叉编译与构建顺序陷阱见 [code-standards.md §I](code-standards.md)。

## 12. 下一步读哪份

| 要动哪层 | 子系统文档 |
| --- | --- |
| pool/group/boxmgr/monitor/probe/builder 选路与自愈 | [subsystems/data-plane.md](subsystems/data-plane.md) |
| httpapi/controlplane 路由、认证、DTO | [subsystems/control-plane.md](subsystems/control-plane.md) |
| nodecodec/noderegion/nodeloc/groupmember/nodeview/nodefacts/nodetag 身份与成员 | [subsystems/facts-and-membership.md](subsystems/facts-and-membership.md) |
| unlock/ipquality/nodedetect/noderesult 检测 | [subsystems/detection.md](subsystems/detection.md) |
| subscription/config/nodecodec/subrender 订阅与解析 | [subsystems/subscription.md](subsystems/subscription.md) |
| store/migrations/jsonx 持久化 | [subsystems/persistence.md](subsystems/persistence.md) |
| frontend 前端 | [subsystems/frontend.md](subsystems/frontend.md) |
