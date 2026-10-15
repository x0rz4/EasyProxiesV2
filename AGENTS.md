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
internal/monitor/assets/      前端构建产物，被 //go:embed assets/* 内嵌（gitignore，禁止手改）
plane/*.md                    设计文档（重载、并发优化、打标系统等方案背景，可能落后于实现）
config.example.yaml           配置模板（复制为 config.yaml 使用；config.yaml 不入库）
Dockerfile / Dockerfile.ci    本地全量构建 / CI 只带产物的运行时镜像
docker-compose.yml            mumujie/easy_proxies 镜像 + 数据卷
.github/workflows/            打 tag 触发：多架构二进制 + Docker 镜像发布
```

---

## 3. Module Map（`internal/`，20 个包）

### 装配与基础

| 包 | 职责 | 关键入口 |
| --- | --- | --- |
| `app` | 唯一的装配点：GeoIP 预热 → 开库 → 从 store 载入 nodes/groups/tags → 建 boxmgr → 建标签服务与重算队列 → 订阅管理器 → 统计落库 → 优雅退出 | `Run()`；`loadNodesFromStore` / `loadGroupsFromStore` / `loadTagNamesFromStore`；`groupStatePersister`（把 `GroupStateEvent` 落库） |
| `config` | 配置结构与解析：YAML 加载/校验、节点 URI 与订阅内容解析（clash/v2ray/base64）、`NodeConfig.NodeKey()` | `Load` / `LoadForReload`（重载路径，允许部分字段变化）、`ParseSubscriptionContent`、`ParseImportContent` |
| `store` | 唯一的持久化层（`Store` 接口 86 个方法）：nodes / subscriptions / stats / sessions / unlock / detection / group pools / tags | `Open()` 内部跑 `Migrate`；`migrations.go`、`sqlite.go`、`sqlite_tags.go`、`node_identity.go`、`migrate_legacy.go` |
| `jsonx` | JSON 编解码统一出口（Sonic + `MarshalCanonical`） | `Marshal` / `Unmarshal` / `MarshalCanonical` |

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
| `monitor` | 管理面全部内容：HTTP API + 内嵌 WebUI、节点运行态注册表（`entry` + `nodeRegistry`）、健康探测 actor（`probe_scheduler`）、健康事件 hub、综合检测任务引擎（`nodecheck`）、标签 HTTP 面（`tag_handlers`/`tag_schema`）、分组订阅输出、会话认证。**只依赖接口，不 import `boxmgr`/`outbound/pool`**（反向才是允许的：`boxmgr`、`pool`、`subscription` 都 import 它，并实现它定义的 `NodeManager` / `GroupRuntimeManager` / `SubscriptionRefresher`）。 |

### 事实与标签

| 包 | 职责 |
| --- | --- |
| `nodefacts` | 纯事实/条件引擎：把 store 行转成类型化 fact、字段注册表、条件 DSL 与求值。**不认识 tag，也不 import `unlock`/`ipquality`**（provider 通过 `ProviderInfo` 注入） |
| `nodetag` | tag 语义与调度：规则编译、互斥组仲裁、manual 覆盖 auto、重算队列（2s 防抖 + 10min sweep）、落库、内置模板 |

### 能力检测与输出

| 包 | 职责 |
| --- | --- |
| `nodedetect` | 手动触发的路由无关诊断：延迟 / 测速 / 出口 IP。**绝不发布健康事件** |
| `unlock` | 流媒体与 AI 服务的解锁检测（netflix / disney+ / chatgpt / gemini / claude / youtube / bahamut / tiktok / amazon / reddit），`init()` 注册式 |
| `ipquality` | IP 质量 provider 适配（ippure / ip-api），每个 provider 结果独立、不做综合评分 |
| `geoip` | mmdb 下载/更新 + 地区查询；`router.go` 提供按地区的路由辅助 |
| `subscription` | 上游订阅拉取与刷新、节点归属（membership）变更；实现 `monitor.SubscriptionRefresher` |
| `subrender` | 把节点 URI 渲染成 Clash/Mihomo 订阅文档与入口代理（无运行时依赖，供 `/sub/*` 复用） |

### 依赖方向

```
main → app → boxmgr ──→ builder ──→ config/store/groupmember/runtimetag/nodecodec
              │  └──→ monitor（boxmgr 把 box context 注入给 pool，pool 反向查 monitor）
              ├──→ outbound/pool ──→ monitor（延迟/黑名单/健康事件）+ group（状态机）
              └──→ group
app → nodetag ←(事实)→ nodefacts        monitor 是 unlock/ipquality 与 nodefacts 的桥接层
subscription → boxmgr（经 interface）
```

---

## 4. Data Flows

### 4.1 代理数据面（每次连接）

`inbound` → `pool` outbound 取 `PoolSnapshot`（不可变）→ 按 mode 选成员 → 拨号 → 成功/失败回写：

- 失败：普通模式走 `sharedMemberState`（阈值 → 临时黑名单）；**分组模式**走 `group.RecordFailure`（滑动窗口 → SUSPECT/EVICTED，**踢出按 NodeID 全局传播到所有分组**）。
- 被踢出成员仍在周期探活；冷却结束 + 连续成功 N 次后由 `RecordHealthSuccess` 自动恢复（恢复同样全局；自动恢复参数是每个分组一项配置）。

### 4.2 控制面 API

`/api/*` 需认证（`management.password` 为空则免密；Cookie `session_token` 或 `Authorization: Bearer`，会话 24h 存 SQLite）；
`/sub/{groupID}` 公开但用分组 `SubscriptionToken` 做常量时间比较。路由表见 `internal/monitor/routes.go`（节点 / 分组 / 标签 / 订阅 / 综合检测 / 设置 / Debug / GeoIP 若干组）。

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
cd frontend && npm run lint && npm run build    # ESLint + tsc -b + vite build（产物直接落到 internal/monitor/assets）
cd frontend && npx vite build                   # 只刷新内嵌产物（vite.config.ts 已指向 ../internal/monitor/assets）

# 容器
docker compose up -d --build
```

**构建顺序陷阱**：`internal/monitor/server.go` 有 `//go:embed assets/*`，**没有前端产物时 `go build ./...` 必然失败**（`pattern assets/*: no matching files found`）。
先跑前端构建；若只想验证纯后端改动，可临时放一个占位 `internal/monitor/assets/index.html`，**验完必须删掉**（否则会打出 stub UI 的二进制）。

**交叉编译 Linux**（与 Dockerfile / CI 一致）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w" \
  -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" \
  -o easy-proxies-linux-amd64 ./cmd/easy_proxies
```

产物名 `easy-proxies-linux-*` / `easy-proxies-windows-*.exe` 已被 gitignore。交叉编译需为 linux 目标全量重编依赖（约 8 分钟/架构），不要在前台等。

**CI 行为**：`.github/workflows/` 仅在推送 `v*` tag 时构建（linux-amd64 / windows-amd64 二进制 + linux/amd64+arm64 镜像）。
**CI 不跑 `go test`，也不跑前端 `lint` / `tsc`** —— 提交前必须在本地自行执行。

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
