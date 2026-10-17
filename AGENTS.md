# Repository Guidelines

EasyProxiesV2 是一个基于 [sing-box](https://github.com/SagerNet/sing-box)（**以库的形式内嵌，不是子进程**）的代理池与订阅管理工具。
单一 Go 二进制同时承载数据面（监听 / 调度 / 拨号）、控制面（HTTP API + 内嵌 WebUI）与持久化（SQLite 文件）。

> 二开自 [jasonwong1991/easy_proxies](https://github.com/jasonwong1991/easy_proxies)；V2 重构了前端与工程化流程，并新增分组池、标签体系与解锁检测。

---

## 1. Tech Stack

| 层 | 技术 |
| --- | --- |
| 后端 | Go 1.27（`go.mod`: `go 1.27.0` + `toolchain go1.27.1`）、标准库 `net/http`（Go 1.22+ `ServeMux`） |
| sing-box | `github.com/sagernet/sing-box v1.13.x` + `sing`、`sing-tun`、`sing-quic`、`sing-vmess`，按可选 tags 编译 |
| 存储 | SQLite（`modernc.org/sqlite`，**纯 Go，CGO_ENABLED=0 可交叉编译**）+ 手写 migration |
| JSON | 统一走 `internal/jsonx`（默认 Sonic；需要确定性字节序时用 `MarshalCanonical`） |
| GeoIP | `oschwald/geoip2-golang/v2`（mmdb 首次启动自动下载，可热更新） |
| 前端 | React 19 + TypeScript + Vite 7 + Tailwind CSS 4 + DaisyUI 5 + TanStack Query + lucide-react + sonner |

代码规模参考：`internal/` + `cmd/` 约 31k 行非测试代码、约 13k 行测试。

---

## 2. Repository Layout

```
cmd/easy_proxies/main.go      入口：加载 config → app.Run
internal/                     全部后端包（见 §3）
frontend/src/                 React 源码（components/ api/ types/ utils/）
internal/httpapi/assets/      前端构建产物，被 //go:embed assets/* 内嵌（gitignore，禁止手改）
plane/README.md               文档入口：当前规格 specs/、变更计划 changes/、专题与历史记录
config.example.yaml           配置模板（复制为 config.yaml 使用；config.yaml 不入库）
Dockerfile / Dockerfile.ci    本地全量构建 / CI 只带产物的运行时镜像
docker-compose.yml            GHCR 镜像 + 数据卷（EASY_PROXIES_VERSION 可指定版本）
install.sh                    Linux Docker / 二进制安装及更新入口
.github/workflows/            打 tag 触发：多架构二进制 + Docker 镜像发布
```

---

## 3. Module Map（`internal/`，29 个包）

### 装配与基础

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `app` | 唯一的装配点：GeoIP 预热 → 开库 → 从 store 载入 nodes/groups/tags → 建 boxmgr → 建标签服务与重算队列 → 订阅管理器 → 统计落库 → 优雅退出 | `Run()`；`loadNodesFromStore` / `loadGroupsFromStore` / `loadTagNamesFromStore`；`groupStatePersister`（把 `GroupStateEvent` 落库） |
| `config` | 配置结构与解析：YAML 加载/校验、节点 URI 与订阅内容解析（clash/v2ray/base64）、`NodeConfig.NodeKey()` | `Load` / `LoadForReload`（重载路径，允许部分字段变化）、`ParseSubscriptionContent`、`ParseImportContent` |
| `store` | 唯一的持久化层（`Store` 接口 86 个方法）：nodes / subscriptions / stats / sessions / unlock / detection / group pools / tags | `Open()` 内部跑 `Migrate`；`migrations.go`、`sqlite.go`、`sqlite_tags.go`、`node_identity.go`、`migrate_legacy.go` |
| `jsonx` | JSON 编解码统一出口（Sonic + `MarshalCanonical`） | `Marshal` / `Unmarshal` / `MarshalCanonical` |
| `noderesult` | 检测结果负载的**唯一**定义（叶子包，不 import 本项目任何包）：`Quality`（ipquality）/ `Service`+`IPInfo`+`Unlock`（unlock）。`store` 嵌入这些类型加元数据字段，`unlock`/`ipquality` 用 `type` 别名指回来，因此检测结果落库/出 API 不需要逐字段转换 | `noderesult.Quality` / `Unlock` / `Service` / `IPInfo` |
| `netx` | 经节点 outbound 拨号的 HTTP 客户端/transport 的**唯一**构造点（叶子包，不 import 本项目任何包）。各消费方的差异（超时、TLS 校验、连接复用、重定向）变成显式 `Options`，而不是在每处重拼 `http.Transport` | `NewClient(dial, Options)` / `NewTransport(dial, Options)`、`CloseIdleConnections` |

### 数据面（代理运行）

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `builder` | 把 config+store 数据翻译成 sing-box `option.Options`：出站（池/多端口/分组/GeoIP）、入站、route | `BuildVersion`（主函数）、`BuildBaseVersion`、`BuildGroupVersion`、`BuildNodeOutbound` |
| `boxmgr` | sing-box 实例生命周期与"代"管理：冷启动、增量更新、全量蓝绿重载、旧实例排空；同时实现 `monitor.NodeManager` / `GroupRuntimeManager` | `Start` / `Reload` / `reloadNodesIncrementally` / `ApplyGroupRuntime` / `ApplyGroupMembershipChanges` |
| `outbound/pool` | 代理池调度：不可变快照上的无锁选路、成员可用性、黑名单、延迟排序 | `newPool`、`PrepareUpdate`/`Commit`（两阶段提交）、`publishSnapshotLocked`、`shared_state.go` |
| `group` | 分组**成员状态机**：滑动失败窗口、ALIVE/SUSPECT/EVICTED、踢出的全局传播与自动恢复、状态事件 | `Register` / `RecordFailure` / `RecordHealthSuccess` / `ReconcileSilent` + `NotifyEvents` / `GroupRuntimeSnapshots` |
| `groupmember` | 分组选成员的**唯一**判定实现（builder 与 boxmgr 共用） | `NewFilter().Allow()`、`Nodes()`、`NormalizeRegion()` |
| `runtimetag` | 出站 tag 的唯一天命名空间：`<16位hex>@v<generation>` | `Format(nodeKey, version)`、`InitialVersion` |
| `nodecodec` | 协议感知的节点身份解析与归一化（vmess/ss/vless/trojan/hysteria2/anytls/http/socks5） | `ParseURI` → `Result{Identity, CanonicalJSON, Hash}`、`FallbackHash` |

### 控制面

| 包 | 职责 |
| --- | --- |
| `monitor` | **运行时**：节点运行态注册表（`entry` + `nodeRegistry`）、健康探测 actor（`probe_scheduler`）、健康事件 hub（`health_event_hub`）、健康结果投影（`health_applier`）、流量采样；`ports.go` 定义消费方接口 `NodeManager` / `GroupRuntimeManager` / `GroupMembershipManager` / `SubscriptionRefresher` 与领域错误，`tag_providers.go` 定义标签事实提供者。**不含任何 HTTP、路由、Session 或 DTO**（已迁至 `httpapi`）。**只依赖接口，不 import `boxmgr`/`outbound/pool`**（反向才是允许的：`boxmgr`、`pool`、`subscription` 都 import 它并实现上述接口）。 |
| `controlplane` | **跨层契约**：`NodeManager` / `GroupRuntimeManager` / `GroupMembershipManager` / `GroupRuntimeStatus` / `SubscriptionRefresher` / `SubscriptionStatus` / `ManagedNodeConfig` 与领域错误（`ErrNodeNotFound` 等、`NodeDuplicateError`）。**只依赖 `config` / `store`**，不 import `monitor` 或 `httpapi`；`boxmgr` / `subscription` 实现它，`httpapi` 消费它。领域 Service 将逐步迁入此包。 |
| `httpapi` | **管理面 HTTP 层**：`Server` 生命周期、路由表、全部 handler 与 DTO、响应管线（`pathID` / `requireStore` / 三个域状态函数）、会话认证、SSE、静态资源与 `//go:embed assets/*`。它 import `monitor` 读运行时状态、直连 `store` 做持久化；`monitor` **不**反向 import 它。`boxmgr` 持有 `*httpapi.Server` 并经 `MonitorServer()` 暴露给 `app`。 |

### 事实与标签

| 包 | 职责 |
| --- | --- |
| `nodefacts` | 纯事实/条件引擎：把 store 行转成类型化 fact、字段注册表、条件 DSL 与求值。**不认识 tag，也不 import `unlock`/`ipquality`**（provider 通过 `ProviderInfo` 注入） |
| `nodetag` | tag 语义与调度：规则编译、互斥组仲裁、manual 覆盖 auto、重算队列（2s 防抖 + 10min sweep）、落库、内置模板 |

### 能力检测与输出

| 包 | 职责 |
| --- | --- |
| `nodedetect` | 手动触发的路由无关诊断：延迟 / 测速 / 出口 IP。**绝不发布健康事件** |
| `unlock` | 流媒体与 AI 服务的解锁检测（netflix / disney+ / chatgpt / gemini / claude / youtube / bahamut / tiktok / amazon / reddit），`init()` 注册式。结果类型是 `noderesult` 的别名 |
| `ipquality` | IP 质量 provider 适配（ippure / ip-api），每个 provider 结果独立、不做综合评分。结果类型是 `noderesult.Quality` 的别名 |
| `geoip` | mmdb 下载/更新 + 地区查询；`router.go` 提供按地区的路由辅助 |
| `subscription` | 上游订阅拉取与刷新、节点归属（membership）变更；实现 `monitor.SubscriptionRefresher` |
| `subrender` | 把节点 URI 渲染成 Clash/Mihomo 订阅文档与入口代理（无运行时依赖，供 `/sub/*` 复用） |

### 依赖方向

```
main → app → boxmgr ──→ builder ──→ config/store/groupmember/runtimetag/nodecodec
              │  └──→ monitor（boxmgr 把 box context 注入给 pool，pool 反向查 monitor）
              ├──→ outbound/pool ──→ monitor（延迟/黑名单/健康事件）+ group（状态机）
              ├──→ group
              └──→ httpapi（持有 Server 并在内部启动；app 经 MonitorServer() 调 Start/Shutdown）
httpapi → monitor / controlplane / store / config（单向；monitor 从不 import httpapi）
boxmgr / subscription → controlplane（实现其接口）；controlplane → config / store（叶子之上）
app → nodetag ←(事实)→ nodefacts        monitor 是 unlock/ipquality 与 nodefacts 的桥接层
subscription → boxmgr（经 interface）
store / unlock / ipquality → noderesult（叶子，检测结果类型的唯一来源）
nodedetect / unlock / ipquality / geoip → netx（叶子，拨号 http.Client 的唯一构造点）
```

---

## 4. Data Flows

### 4.1 代理数据面（每次连接）

`inbound` → `pool` outbound 取 `PoolSnapshot`（不可变）→ 按 mode 选成员 → 拨号 → 成功/失败回写：

- 失败：普通模式走 `sharedMemberState`（阈值 → 临时黑名单）；**分组模式**走 `group.RecordFailure`（滑动窗口 → SUSPECT/EVICTED，**踢出按 NodeID 全局传播到所有分组**）。
- 被踢出成员仍在周期探活；冷却结束 + 连续成功 N 次后由 `RecordHealthSuccess` 自动恢复（恢复同样全局；自动恢复参数是每个分组一项配置）。

### 4.2 控制面 API

`/api/*` 需认证（`management.password` 为空则免密；Cookie `session_token` 或 `Authorization: Bearer`，会话 24h 存 SQLite）；
`/sub/{groupID}` 公开但用分组 `SubscriptionToken` 做常量时间比较。路由表见 `internal/httpapi/routes.go`（节点 / 分组 / 标签 / 订阅 / 综合检测 / 设置 / Debug / GeoIP 若干组）。

### 4.3 标签重算 → 分组增量重建

```
检测阶段落库 → enqueueRetag(nodeID)
  → nodetag.Queue（2s 防抖合并，10min 全量 sweep）
  → nodetag.Service.Recompute（CompileRules → nodefacts.Loader.Load → Resolve → ReplaceAutoNodeTags）
  → notify（仅"可见 tag 名集合变化"的节点）
  → boxmgr.ApplyGroupMembershipChanges → 只重建成员集合真正变化的组（base box 从不 reload）
```

### 4.4 配置热重载的三条路径（`boxmgr`）

1. **冷启动** `Start`：base box + 每个启用分组一个独立 box。
2. **增量** `reloadNodesIncrementally`：节点 URI 未变则复用旧 tag/outbound，变更节点用新 generation 生成新 tag → `pool.CreateMember` → `PrepareUpdate`/`Commit` → 旧成员异步排空。仅在"未启用 multi-port 且未启用 GeoIP 且其余配置一致"时可用。
3. **全量蓝绿** `reload`：`waitForConnectionsToDrain` → 关旧 box → 等待端口释放 → 用 `generation+1` 重建全部 → `SweepStaleNodes`。

tag 代际不可覆盖：同一 tag 重复 `Create` 会当场关掉旧 outbound、无法排空，所以变更节点必须换新 `@vN`。

---

## 5. Build, Test, and Development Commands

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
cd frontend && npx vite build                   # 只刷新内嵌产物（vite.config.ts 已指向 ../internal/httpapi/assets）

# 容器
docker compose up -d --build
```

**构建顺序陷阱**：`internal/httpapi/server.go` 有 `//go:embed assets/*`，**没有前端产物时 `go build ./...` 必然失败**（`pattern assets/*: no matching files found`）。
先跑前端构建；若只想验证纯后端改动，可临时放一个占位 `internal/httpapi/assets/index.html`，**验完必须删掉**（否则会打出 stub UI 的二进制）。

**交叉编译 Linux**（与 Dockerfile / CI 一致）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w" \
  -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" \
  -o easy-proxies-linux-amd64 ./cmd/easy_proxies
```

产物名 `easy-proxies-linux-*` / `easy-proxies-windows-*.exe` 已被 gitignore。交叉编译需为 linux 目标全量重编依赖（约 8 分钟/架构），不要在前台等。

**CI 行为**：`build-and-release.yml` 在推送 `v*` tag 或手动运行时验证并构建 Linux amd64 / arm64 二进制，tag 构建会上传 Release 附件及 SHA256 校验文件。该工作流执行前端 lint / tsc / build、后端全量测试及关键包 race 检查。
**镜像发布**：`docker-publish.yml` 在 main/master、版本 tag 推送及 PR 时构建多架构镜像，非 PR 运行使用 `GITHUB_TOKEN` 推送到 GHCR。不要在 Release 工作流中重复添加依赖 `DOCKERHUB_TOKEN` 的 Docker Hub 发布任务。
镜像的精确版本标签必须匹配 `frontend/package.json` 与 Git Release tag；只有稳定版本 tag 构建更新 `latest`，分支及预发布版本不得覆盖稳定版别名。修改安装脚本时运行 `bash -n install.sh` 和 `python3 -m unittest discover -s tests -p 'test_install.py'`。

---

## 6. Coding Style & Naming Conventions

**Go**
- `gofmt`（tab 缩进）；包名小写单词；导出标识符 `PascalCase`，非导出 `camelCase`；错误包装用 `fmt.Errorf("...: %w", err)`。
- 保持 `internal/` 的包边界：不要绕过 `store`（直接写 SQL）或 `boxmgr`（自己 new box）等抽象，也不要让 `nodefacts`/`nodetag` 反向依赖 `unlock`/`ipquality`。
- **锁边界是硬约束**：`boxmgr.Manager.mu` 只保护管理字段与 box 生命周期，`reloadMu` 串行化生命周期操作；**禁止持锁做网络拨号、解析订阅或等待 drain**。数据面读路径只做一次 `atomic.Pointer.Load()`。
- 新增跨包接口时优先"消费方定义接口"（monitor 定义 `NodeManager` / `GroupRuntimeManager` / `SubscriptionRefresher`，由 boxmgr / subscription 实现）。

**前端**
- 两空格缩进；组件文件 `PascalCase.tsx`；hooks / 工具 `camelCase`；共享类型放 `types/index.ts`；API 调用统一放 `api/client.ts`。
- 用 `utils/cn.ts` 的 `cn()` 合并类名；页面骨架复用 `components/ui/PageLayout.tsx`，表单字段用同文件的 `Field`。
- 提交前务必 `npm run lint` + `npm run build`（tsc 严格检查）。

---

## 7. Testing Guidelines

基础设施变更先参考 [plane/README.md](plane/README.md)、[当前契约](plane/specs/proxy-pool/spec.md) 与 [写路径评审清单](plane/write-path-audit.md)。新增实现需关联 Requirement/任务 ID，区分已实现、待实现与未验证；历史方案中的“未推送”等描述不是当前操作约束。L0–L5 是责任分层，不要求创建同名包。

- 使用标准 `testing` 包；用例名 `TestBehavior`；解析器与配置变体优先表驱动；fixture 必须确定性（注入固定 `time.Now()`，避免 sleep）。
- 回归测试就近放在改动包内（`internal/**/xxx_test.go`）。
- 重点覆盖区：`group`（状态机、踢出与恢复）、`outbound/pool`（快照与选路）、`builder`（URI 解析）、`nodefacts`/`nodetag`（条件求值与规则往返）、`monitor`（探测调度与 API 行为）、`store`（migration 与 tag 投影）。
- 前端没有单测框架，验收标准就是 `npm run lint` + `npm run build` 通过。

---

## 8. Database & Config Conventions

- **migration 只追加**：`internal/store/migrations.go` 的列表按 `Version` 递增、只允许在末尾新增（每个 migration 独立事务，`schema_migrations` 记录已应用版本）。新列必须带 `NOT NULL DEFAULT`，保证升级行为不变。当前最新为 v14。
- 表结构变更后需同步 `sqlite.go` 的列常量 / `scan*` / `INSERT` / `UPDATE`；改 `store.GroupPool` 还要同步 `config.GroupPoolConfig` + `boxmgr` 的双向转换与相等性判断（否则热重载判定不到变化）。
- `nodes.tags` **只是 `node_tags` 的只读投影**（由 `refreshNodeTagProjection` 重写）；改标签必须走 store 的 tag API（`SetManualNodeTags` / `ReplaceAutoNodeTags`），旁路写入会被下次重算覆盖。
- 配置优先级：**store（数据库）优先于 `config.yaml`**；启动时 `loadNodesFromStore` / `loadGroupsFromStore` / `loadTagNamesFromStore` 会用库里的数据覆盖配置中的对应字段。`yaml:"-"` 的字段即"只来自 store"。
- 默认端口：共享池入口 `2323`、管理面板 `9888`、multi-port 起始 `24000`。
- tag 语义区分：**分组过滤存 tag ID**（tag 改名不影响分组），**节点身上的标签是名字**；两者由 `groupmember.NewFilter` 的 `WithTagNames` 桥接。

---

## 9. Commit & Pull Request Guidelines

### 版本递增规则

- 本次版本基线为 **2.0**，npm 包版本为 **2.0.0**。以 `frontend/package.json` 的 `version` 为当前版本来源，同步更新 `frontend/package-lock.json` 顶层及 `packages[""].version`。
- 每次完成一批功能更新或问题修复时，必须在当前版本上递增一次；同一批任务的多个文件修改、验证及返修不重复递增。
- 小功能、问题修复和日常优化：按产品版本 **+0.01** 的含义递增，使用 SemVer 的 patch 位记录，例如 `2.0.0 → 2.0.1 → 2.0.2`（产品版本对应 `2.0 → 2.01 → 2.02`）。
- 大功能更新：按产品版本 **+0.1** 的含义递增，使用 SemVer 的 minor 位记录，并将 patch 归零，例如 `2.0.2 → 2.1.0`。同一批次同时包含大小更新时，仅执行一次大功能递增。
- 包版本和发布标签统一采用三段式 SemVer（如 `2.0.0`、`v2.0.0`），以兼容 npm 和现有发布配置。不要使用浮点数计算版本；patch 达到 10 后仍按整数递增，不自动进位到 minor。
- 本次初始化为 `2.0.0`，不额外叠加版本。后续从文件中的实际当前版本继续递增；创建或推送发布标签需遵循用户当次的发布指令。

- 历史使用简洁的 Conventional Commit 风格：`feat(store): ...`、`fix(pool): ...`、`deps: ...`、`build: ...`；祈使句、单一主题。
- PR 需说明：行为变化、**配置或数据库影响（是否有新 migration）**、以及确切的验证命令；涉及可见 UI 变化请附截图。
- 绝不提交真实订阅、凭据、`config.yaml`、SQLite 数据与证书；`skip_cert_verify` 默认保持关闭。

---

## 10. Guardrails（容易踩的地方）

1. **成员判定只有一份**：任何"这个节点属不属于该组"的判断都必须走 `groupmember.Filter.Allow` / `Nodes`。判定顺序是契约：组未启用 → `excluded_node_ids` 无条件排除 → `explicit_node_ids` 无条件加入（绕过黑白名单）→ region 命中 + tag 黑名单/白名单（`any`/`all`）。未解析的 tag ID 会变成永不匹配的哨兵（宁可缩小，不许悄悄放宽）。
2. **`pool.Options` 必须可 JSON 往返**：sing-box 通过 `option.Outbound` 的 `badjson` 编解码它（`time.Duration` → 纳秒整数，实测保真），且**每次建 box 前必须 `pool.Register`**，否则解码拿不到类型；缺省值不会自动补齐，所以 `normalizeOptions` 的兜底不能删。
3. **成员 outbound 不能声明静态依赖**：`CreateMember` 后若 `Dependencies()` 非空会立即失败并回滚（否则 sing-box 的 `dependByTag` 无法随快照更新）。
4. **sing-box `Remove` 失败不要重试**：可能在删除 lookup 后仍返回错误，只能通过保存下来的 `RetiredMember.Close()` 关闭。
5. **落地区域只能来自拨测后的出口 IP**，禁止从 URI 的服务器地址推断；未分类统一归 `geoip.RegionOther`，且 `builder` 与 `groupmember` 两处归一必须一致。
6. **组状态事件与快照的顺序**：`ReconcileSilent` 先迁移状态，**等匹配的 pool 快照可见后再 `NotifyEvents`**；`PrepareUpdate` 失败必须 `Rollback` 并清理已创建的成员。
7. **自动标签不能依赖自动标签**：条件注册表刻意不含 `tags.auto`（只暴露 `tags.manual`），否则重算非幂等。
8. `ResetSharedStateStore` / `ResetAllRuntimeState` 只用于进程启动重试、关闭与测试，**热重载期间调用会清掉无关分组的状态**。
9. 新增解锁检测器：加一个 `checker_*.go`，实现 `Checker` 接口并在 `init()` 里 `RegisterChecker`，取一个未占用的 `Order()`。provider 字段、内置标签模板、schema 枚举会自动派生；重复 key 或别名冲突会在启动时 panic。
10. **`golang.org/x/net` 必须钉在 `v0.50.0`（`x/crypto` ≤ `v0.48.0`）**：sing-box v1.13.x 用 `//go:linkname` 直接抓 `x/net/http2.(*Transport).connPool` 这个**私有字段**，而该字段自 x/net `v0.51.0` 起已改名 `connPoolOrDef` → 一旦升级 x/net，链接器就会报 `relocation target ... not defined`。后果随 Go 版本放大：`go test` 下 `subscription` / `boxmgr` / `app` 三个包必然 `[build failed]`；Go ≥ 1.27 时**连生产构建都失败**（1.26 只是靠链接期 DCE 侥幸通过，不要依赖）。由于 `modernc.org/sqlite` 与 `geoip2-golang` 会反向要求更高的 x/net，这两个版本也要跟着钉住（当前 `v1.52.0` / `v2.2.0`）。调整这几个依赖后务必跑一次 `go build ./cmd/easy_proxies` 验证链接。
11. **经节点拨号的 HTTP 客户端一律走 `netx`，且 `DialFunc` 不允许为 nil**：`netx` 造的是**裸** `http.Transport`（这是刻意的：每个消费者只要它声明的那几个字段）。因此 `nil` 拨号不等于"用标准库默认值"——那样会静默丢掉 `http.DefaultTransport` 的 HTTP/2、代理环境变量与共享空闲连接池。主机本地（不经节点）的请求，例如 `IPAPIClient` 的批量查询，应直接用 `&http.Client{Timeout: ...}`，不要经过 `netx`。新增消费者时把差异写成显式 `Options`，不要在调用处重拼 transport。
12. **错误→状态码是三个策略，不是一个**（`httpapi/response.go`）：`nodeErrorStatus`（类型驱动，`respondNodeError` 靠它额外带 `existing_node_id`）、`subscriptionErrorStatus`（消息驱动，未识别 → 500）、`tagErrorStatus`（消息驱动，未识别 → 400）。它们**故意不一致**：同样是"节点不存在"，tag 域是 400（请求体点了不存在的节点）、订阅域是 404，且 `tag_handlers_test.go` 钉住了这个 400。调用方必须显式点名自己用哪个策略——`errorToStatus(err)` 这种统一分发器今天写不对，因为两个域只能靠"谁调用的"区分，错误的类型和文案都区分不出来。同理 `httpapi/response.go` 的 `pathID`（400）与 `pathIDOrNotFound`（公开 `/sub/*` 用 404，避免探测 ID 形态）不可互换。
13. **不要为"减少 handler 样板"引入泛型 `handle[Req, Resp]` 包装**：实测 14 个解码请求体的 handler 各自带 3–19 个错误分支，包装只省掉最外层的 decode/serialize，分支一个不少，缩短量为 0；最贴近该形状的 `handleReload` 还会因丢掉 `respondNodeError` 的 `existing_node_id` 而行为变化。真正有效的是 `pathID` / `requireStore` / 三个域状态函数。若 httpapi 拆分时要重提，从这次测量出发，不要从想法出发。
