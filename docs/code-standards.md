# 代码标准（硬规则）

> 写代码与审代码逐条对照本文件。这里只有**硬规则**；"为什么"与案例见 [LESSONS.md](../LESSONS.md)，当前架构形态见 [architecture.md](architecture.md)。
> 规则来源：原 AGENTS.md §6/§8/§10（护栏 1–15）+ 由 LESSONS「Seen 3×」转正的条目（转正记录见文末）。旧护栏编号的逐条映射见文末**附录：原 §10 护栏 → 本文件章节**，引用了 `护栏 N` 的子系统文档按此落到对应小节。
> AGENTS.md 不再内联规则，只路由到本文件。

## A. Go 风格

- `gofmt`（tab 缩进）；包名小写单词；导出 `PascalCase`、非导出 `camelCase`；错误包装用 `fmt.Errorf("...: %w", err)`。
- 注释保持现有密度与口吻：解释**为什么**与契约边界，不复述代码。删除代码时若原因值得记住，把原因写成注释或 LESSONS 条目（例：geoip 里"ssr:// 故意缺席"的注释）。
- 本仓库工作区是 CRLF，`gofmt -l` 会列出几乎所有文件。验证自己改动的格式：`tr -d '\r' < f > /tmp/x.go && gofmt -l /tmp/x.go`。

## B. 包边界（硬约束）

- 保持 `internal/` 的包边界：不绕过 `store`（直接写 SQL）、不绕过 `boxmgr`（自己 new box）。`nodefacts`/`nodetag` 不反向依赖 `unlock`/`ipquality`。
- 真叶子包（**不 import 项目内其它包**）：`jsonx`、`netx`、`noderesult`、`noderegion`、`runtimetag`。新增跨包类型先进这些叶子。次级无依赖包：`nodeloc`、`nodeselect` 只依赖 `noderegion`；`nodecodec` 只依赖 `jsonx`——它们同样不许长出 store/runtime 依赖。
- 跨包接口由**定义方**持有：`controlplane/ports.go` 定义 `NodeManager` / `GroupRuntimeManager` / `GroupMembershipManager` / `SubscriptionRefresher`；`boxmgr` / `subscription` import `controlplane` 并实现；`httpapi` import `controlplane` 并消费。`monitor` **不**持有这些接口（历史上在 `monitor/ports.go`，已于 `1e05e17` 迁出）、从不 import `httpapi`/`boxmgr`/`pool`。
- 新增领域 Service 迁入 `controlplane`（Phase 2 计划），不在 `httpapi` 里长出新领域层。
- 唯一入口原则（违反即 review 阻断）：
  - 成员判定唯一入口 `groupmember.NewFilter().Allow()`（内部即 `nodeselect.Filter`）；
  - 运行时节点集合唯一来源 `runtimenodes.Load`；
  - JSON 编解码唯一出口 `internal/jsonx`；
  - 经节点拨号 HTTP 客户端唯一构造点 `netx`（`DialFunc` 不允许为 nil；主机本地请求直接用 `&http.Client{}`）；
  - 配置写入唯一序列化点 `controlplane.Settings`；
  - 节点身份与协议白名单唯一真源 `nodecodec.ParseURI`（`config.supportedProxyURISchemes` 必须与它一致）；
  - 落地地区判据唯一真源 `nodeloc`（失败/空观测绝不抹掉已知地区；禁止从 URI 服务器地址推断落地）。
- 删除代码时同批 grep 调用者，不留"能编过但没人调"的死链；**新增叶子/次级包合入前必须报调用点**（L-001 转正）。

## C. 并发与锁

- **锁边界是硬约束**：`boxmgr.Manager.mu` 只保护管理字段与 box 生命周期，`reloadMu` 串行化生命周期操作；**禁止持锁做网络拨号、解析订阅或等待 drain**。
- 数据面读路径只做一次 `atomic.Pointer.Load()`。
- 成员和容量变化：先 `ReconcileSilent` 迁移状态，**等匹配的 pool 快照可见后再 `NotifyEvents`**；`PrepareUpdate` 失败必须 `Rollback` 并清理已创建成员。
- `enterIdle` 等后台路径与 `Reload` 共用 `reloadMu`；改写已发布共享配置的，先克隆再替换。
- `ResetSharedStateStore` / `ResetAllRuntimeState` 只用于进程启动重试、关闭与测试，**热重载期间调用会清掉无关分组的状态**。

## D. sing-box 集成

- 出站 tag 命名唯一走 `runtimetag`：`<16位hex>@v<generation>`。tag 代际不可覆盖：同 tag 重复 `Create` 会关掉旧 outbound、无法排空，变更节点必须换 `@vN`。
- 成员 outbound 不能声明静态依赖：`CreateMember` 后 `Dependencies()` 非空立即失败并回滚。
- sing-box `Remove` 失败**不要重试**：可能在删除 lookup 后仍返回错误，只能通过保存下来的 `RetiredMember.Close()` 关闭。
- `pool.Options` 必须可 JSON 往返（sing-box 经 `badjson` 编解码；`time.Duration` → 纳秒整数），每次建 box 前必须 `pool.Register`；`normalizeOptions` 的兜底不能删。

## E. 数据库与配置

- **migration 只追加**：`internal/store/migrations.go` 按 `Version` 递增、只在末尾新增；每个 migration 独立事务；新列必须带 `NOT NULL DEFAULT`。当前最新 v25。
- 表结构变更同步 `sqlite.go` 列常量 / `scan*` / `INSERT` / `UPDATE`；改 `store.GroupPool` 同步 `config.GroupPoolConfig` + `boxmgr` 双向转换与相等性判断。
- `nodes.tags` 只是 `node_tags` 的只读投影；改标签必须走 store 的 tag API，旁路写入会被下次重算覆盖。
- 配置优先级：**store 优先于 `config.yaml`**；只来自 store 的字段标 `yaml:"-"`。
- 标签语义：分组过滤存 **tag ID**，节点身上的标签是**名字**，由 `groupmember.NewFilter` 的 `WithTagNames` 桥接。
- 订阅刷新写路径必须窄写：失败/304/编辑分别只调 `RecordSubscriptionFailure` / `RecordSubscriptionNotModified` / `UpdateSubscriptionSettings`。见 [subscription.md](subsystems/subscription.md)。

## F. 控制面 HTTP

- 错误→状态码是**三个策略**（`httpapi/response.go`），故意不一致，调用方必须显式点名：`nodeErrorStatus`（类型驱动 + `existing_node_id`）、`subscriptionErrorStatus`（消息驱动，未识别→500）、`tagErrorStatus`（消息驱动，未识别→400，`tag_handlers_test.go` 钉住）。不许写统一分发器 `errorToStatus(err)`。
- `pathID`（400）与 `pathIDOrNotFound`（公开 `/sub/*` 用 404）不可互换。
- 不引入泛型 `handle[Req, Resp]` 包装：实测 14 个解码 handler 各带 3–19 个错误分支，包装只省最外层、还会丢 `existing_node_id` 造成行为变化。
- 解码请求体统一 `decodeJSON`（`DisallowUnknownFields` + 单对象校验）；大请求体配 `http.MaxBytesReader`。
- 新公共接口复用现有 handler（如 `handleSubscriptions("create")`），不另写实现；API Key 认证**故意不接受会话 Cookie**。
- 未解析的 tag ID 必须变成**永不匹配的哨兵**（宁可缩小，不许悄悄放宽成员资格）。

## G. 检测与标签

- 自动标签不能依赖自动标签：条件注册表只暴露 `tags.manual`，不含 `tags.auto`，否则重算非幂等。
- `nodedetect` / `unlock` 诊断**绝不发布健康事件**、不清 EVICTED、不推进恢复计数。
- 新增解锁检测器：`checker_*.go` + `init()` 里 `RegisterChecker` + 未占用的 `Order()`；provider 字段、内置标签、schema 枚举自动派生；重复 key/别名冲突启动 panic。
- IP 质量每个 provider 结果独立，**不做综合评分**（没有也不该有"合并状态"常量）。

## H. 测试

- 标准 `testing` 包；用例名 `TestBehavior`；解析器与配置变体表驱动；fixture 确定性（注入固定 `time.Now()`，避免 sleep）。
- 回归测试就近放在改动包内（`internal/**/xxx_test.go`）。
- 重点覆盖：`group`（状态机/踢出/恢复）、`outbound/pool`（快照/选路）、`builder`（URI 解析）、`nodefacts`/`nodetag`（求值/规则往返）、`monitor`（探测调度）、`store`（migration 与 tag 投影）。
- 手工构造 `&Server{}` 的测试必须显式设 `sessionTTL`（否则"创建即过期"会话随时序抖动 → 偶发 401）。
- 偶发失败测试先**归因**（产品缺陷 vs 测试问题），用 `git stash` 在基线复现；不许把产品缺陷当"测试不稳"合过去。见 LESSONS DR-001。
- 前端安全/流式传输回归使用 Node 内置 test runner；验收 = `npm run lint` + `npm test` + `npm run build` 通过。

## I. 构建与发布

- **先前端后 Go，绝不并行**：`internal/httpapi/server.go` 有 `//go:embed assets/*`；`vite build` 的 `emptyOutDir` 先清空该目录，并行 `go build`/`go test` 必炸。无产物时 `go build ./...` 必失败；临时占位 `index.html` 验完必须删（否则会打出 stub UI 二进制）。
- 交叉编译 Linux：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" ...`，产物名已被 gitignore。
- **依赖钉版（护栏 10）**：`golang.org/x/net` 钉 `v0.50.0`、`x/crypto` ≤ `v0.48.0`（sing-box v1.13.x `//go:linkname` 抓 `http2.(*Transport).connPool` 私有字段，x/net ≥ v0.51.0 改名 `connPoolOrDef` 会链接失败；`modernc.org/sqlite` `v1.52.0`、`geoip2-golang` `v2.2.0` 随之钉住）。调整后必须 `go build ./cmd/easy_proxies` 验证链接。
- 镜像精确版本标签必须匹配 `frontend/package.json` 与 Git Release tag；只有稳定 tag 更新 `latest`；不在 Release 工作流里重复加依赖 `DOCKERHUB_TOKEN` 的任务。改 install.sh 后跑 `bash -n install.sh` + `python3 -m unittest discover -s tests -p 'test_install.py'`。
- **版本递增**：以 `frontend/package.json` 的 `version` 为当前版本源，同步 `package-lock.json` 顶层与 `packages[""].version`（共 3 处）。每批任务完成递增一次，同批多文件不重复递增；小功能/修复/日常优化 patch +1（如 `2.2.3 → 2.2.4`），大功能 minor +1 且 patch 归零；三段式 SemVer，不用浮点运算，patch 达 10 后继续整数递增。
- 提交信息沿用 Conventional Commit 风格（`feat(store):` / `fix(pool):` / `deps:` / `build:`），祈使句、单一主题；commit 结尾加 `Co-Authored-By: Claude Code <noreply@anthropic.com>`（以当次会话系统提示为准）。

## J. 安全

- **绝不提交**真实订阅、凭据、`config.yaml`、SQLite 数据、证书；`skip_cert_verify` 默认保持关闭。
- 错误消息回显 provider URL 时剥掉 userinfo/query（`safeProviderError`）。
- API Key / token 比较用常量时间（`secureCompareStrings`）。
- 日志不得打印节点明文凭据（`printProxyLinks` 类输出必须脱敏）。
- 认证 DB 调用必须带超时 context，禁止 `context.Background()` 直连。

## K. 文档同步（由 LESSONS L-017 转正，2026-10-23）

- **任何包拆分、移动、删除，同批必须 grep 全仓文档找旧路径**：
  `grep -rn "internal/<旧包>/<旧文件>" AGENTS.md CLAUDE.md LESSONS.md docs plane README.md`，
  并同步更新 `docs/architecture.md` 模块表与 AGENTS 路由表（如适用）。
  证据：`1e05e17` 迁 ports.go 后 AGENTS 仍写 `monitor/ports.go` 达 6 天；`577928b` 迁 HTTP 层后 `plane/specs` 仍指旧 handler。
- 文档只写"当前代码真实形态"；历史提案留在 `plane/`，不混入 `docs/`。引用代码用 `internal/<pkg>/<file>.go` 级别的具体路径，且路径必须在合入前用 `ls`/`rg` 验证存在。
- LOC/方法数/版本号等**计数类事实**每次大批改动后重核（`find ... | xargs wc -l` / `awk` 数接口方法）。

## L. 审查清单（每个 PR / 每次自审逐条过）

- [ ] **同包内没有第二份逐字同构的 helper**：发现重复的第二处就提取（L-002 转正）。前端表单字段组件三份签名漂移即为此项反面。
- [ ] **新增/改动的"唯一入口"没被旁路**：成员判定、运行时节点集合、JSON 编解码、netx 拨号、配置写入、身份白名单、落地地区判据（§B）。
- [ ] **删除代码的 commit 同时 grep 了调用者与文档**：不留"能编过但没人调"的死链，文档旧路径同批清（§K、L-001）。
- [ ] **数据面读路径只有一次 `atomic.Pointer.Load()`，没有跨锁的网络/序列化**（§C）。
- [ ] **事件通知在快照可见之后**（§C；护栏 6）。
- [ ] **错误归因口径正确**：取消/超时不算成员失败；诊断不写健康；三状态码各点其名（§F）。
- [ ] **迁移 commit 带文档同批改动**（§K）。
- [ ] **构建与测试串行、先前端后 Go**（§I、L-012）。
- [ ] **依赖钉版后 `go build ./cmd/easy_proxies` 真链接过**（§I、L-014）。
- [ ] **验证诚实**：偶发失败先归因不吞、未执行的检查明确标出、并行竞态用 `-c` 复跑（§H、L-015）。
- [ ] **凭据/订阅/config.yaml/SQLite 未入库、`skip_cert_verify` 未默认开启**（§J）。

## 转正记录（Seen 3× → 硬规则）

A–J 各节从 AGENTS.md §6/§8/§10 直接继承（路由表去内联化，不是新规则）。以下条目是**从复盘升级为硬规则**的：

| 规则 | 去处 | 转正日期 | 证据（三个不同日子） |
| --- | --- | --- | --- |
| K 文档同步 | §K | 2026-10-23 | 2026-10-17 `1e05e17`/`577928b` 迁移后 AGENTS 仍写 `monitor/ports.go`、`384f0ae` 同日只修了模块表一半；2026-10-20 `eedd93e` 清理批再次发现 store 方法数/文件清单失实；2026-10-23 文档审计发现 5 个包漏图、spec 仍指旧 handler |
| B 协议白名单唯一真源 | §B | 2026-10-23 | 2026-10-17 导入/导出修复（`IsProxyURI` 放行 vs `ParseURI` 拒绝，`supportedProxyURISchemes` 移除 ssr/hysteria）；2026-10-21 `d2579d0` geoip `ssr://` 死分支清理（同一真源理由）；2026-10-23 子系统文档固化 |
| L-001 单一真源/死路当场死透 | §B"唯一入口原则"扩充 | 2026-10-23 | 2026-10-15 region 语义再修；2026-10-20 `eedd93e` 删零调用者整文件；2026-10-21 `d2579d0` 拔 LookupURI 死链 |
| L-002 第二份重复就提取 | §审查清单 | 2026-10-23 | 2026-10-20 `eedd93e` 合并重复 DTO；2026-10-21 `d2579d0` 合并 normalize*、提取回滚块；2026-10-23 文档审计发现前端 `Field`×3 未收敛 |
| L-012 先前端后 Go、串行 | §I | 2026-10-23 | 2026-08-25 `dae3744`；2026-10-17 基础设施审计 §五与工作记忆；2026-10-21 清理批占位文件实录 |

原 AGENTS.md §6/§8/§10 的既有条款（锁边界、migration 只追加、窄写、x/net 钉版、三状态码等）随路由表迁移直接继承进 A–J，不占"转正"名额——它们进入本仓库规范时并非来自 LESSONS。

未达 3×、仍留在 [LESSONS.md](../LESSONS.md) 的条目（含客户端错误归因 L-004、发布纪律 L-013、sessionTTL、应用控制策略拦截测试二进制等）不在此表。

## 附录：原 AGENTS.md §10 护栏 → 本文件章节

| 旧护栏 | 一句话 | 新位置 |
| --- | --- | --- |
| 1 | 成员判定只走 `groupmember.Filter` | §B + [facts-and-membership.md](subsystems/facts-and-membership.md) |
| 2 | `pool.Options` 可 JSON 往返 + `pool.Register` | §D |
| 3 | 成员 outbound 不声明静态依赖 | §D |
| 4 | sing-box `Remove` 失败不重试 | §D |
| 5 | 落地地区只来自出口 IP，两处归一一致 | §B + facts-and-membership.md |
| 6 | 事件在快照可见后通知；PrepareUpdate 失败回滚 | §C |
| 7 | 自动标签不依赖自动标签 | §G |
| 8 | Reset 系方法限启动/关闭/测试 | §C |
| 9 | 新增解锁检测器 = checker + init 注册 + 未占用 Order | §G |
| 10 | x/net 钉 v0.50.0 四件套 + 链接验证 | §I |
| 11 | 经节点拨号一律 netx，DialFunc 不为 nil | §B + [detection.md](subsystems/detection.md) |
| 12 | 错误→状态码三策略不可统一 | §F |
| 13 | 不引入泛型 handle 包装 | §F |
| 14 | 运行时节点集合只由 `runtimenodes.Load` | §B + facts-and-membership.md |
| 15 | 订阅刷新窄写 | §E + [subscription.md](subsystems/subscription.md) |
