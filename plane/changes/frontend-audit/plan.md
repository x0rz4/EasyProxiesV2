# 前端合理性、安全与性能审计

日期：2026-10-28。基线 fdd9d08 / 2.2.17，分支 codex/frontend-audit。状态：已完成，版本 2.2.18。

## 范围与交付

审视全部十个页面及共享 API、查询缓存、流式连接、表单和依赖树。保留 React/TanStack Query 与后端一致成员谓词；根据证据进行局部重构，不凭包版本较旧就全部跨主版本升级。每批变更在此记录原因、实现、验证与局限，最终递增一次版本并提交 Git。

- [x] A 依赖：npm audit（完整/生产）、peer/engines、受影响入口分析；修复已知依赖漏洞、验证干净安装。
- [x] B 安全：登录/退出/会话恢复、缓存与凭据清理、XSS/链接/静态资源边界；实测安全回归。
- [x] C 性能：首屏 bundle 与按需页面、查询取消/分页缓存/轮询、SSE/日志刷新开销；提供前后测量及资源清理回归。
- [x] D 适用性：全部页面数据/空态/失败态/破坏操作与移动端、键盘使用；修复确认的问题，评估共享组件边界。
- [x] E 验收：真实浏览器桌面/移动验证，前端 lint/build、安全/行为回归、Go 全量测试和生产 tags vet/链接；同步当前文档和最终证据，提交 Git。

## 初始证据

本文各轮“验证中/未提交”为该轮时点记录；当前验收状态见末尾，避免将历史失败或工具限制覆盖为成功。

- npm audit：6 个受影响包（5 high、1 moderate），均处于工具依赖链，不能解释成浏览器运行时有 6 个可远程利用漏洞。包括 Vite 7.3.6、brace-expansion 5.0.8、@humanfs/node、nanoid、source-map-js、zod。原始审计/版本输出：忽略目录 `.tools/frontend-audit/npm-{audit,outdated}-before.json`。brace-expansion 的旧 override 低于当前补丁下界 5.0.12；来源 [维护者公告](https://github.com/advisories/GHSA-q2hr-2g5m-vwhr)。
- App 静态导入全部页面，当前生成单个约 655 kB JS（gzip 185 kB）；需重新构建量化并测量浏览器实际入口加载。
- client.ts 在模块加载时无保护读取 localStorage auth_token，logout 只删除该条目；HttpOnly session_token 与后端会话仍有效。App 按 token 是否存在推断认证，查询缓存退出后未清理。
- streamTraffic 正常 EOF 不通知错误，MonitorPanel 只有 onError 路径安排重连；四段重复 SSE 解码/重试代码不统一处理 401 和 reader 清理。DebugPanel 每条日志重新合并、排序与渲染最多 1000 行，需测量和限制刷新频率。

## 验证记录

当前仅完成源码初查与依赖基线，未声明审计或修复完成。后续按 A–E 补齐证据。

### 第一批实施（验证中）

- A：Vite 最低版本提高到 7.3.7；brace-expansion override 至 ^5.0.12。@eslint/js 对齐 ESLint 10，typescript-eslint 升至支持该版本的 8.72.1；按当前范围更新上述受影响传递依赖。完整 npm audit 为 0。未跨主版本升级 React/Vite/TypeScript；新增 esbuild 为测试运行器的显式开发依赖，不依赖隐式提升。新 lint 发现 ManagePanel 的无用初始赋值，已删除。
- B：凭据从 localStorage 移至内存，清除旧存储键；刷新按服务器 `authenticated` 状态与 HttpOnly Cookie 恢复。新增认证 POST /api/auth/logout，撤销 Cookie 与不同的 Bearer、清除 Cookie；持久失败报告错误，不假报成功。Auth 持久读取与撤销串行，防止在途旧读恢复被撤销缓存；所有 store 调用有期限。退出/401 清空并取消 QueryClient 数据，旧请求的 401 不注销新登录。偏好存储禁用时降级使用内存。
- B：管理 API 默认 no-store/nosniff；HTML 添加 CSP、拒绝 iframe 与 referrer，保留 inline style 满足 React/DaisyUI 动态样式，禁止 inline script。哈希静态资源长期缓存、HTML 每次验证，缺失脚本真实返回 404。fs.Stat 替代未关闭的静态探测 Open。国旗字体改用锁定 npm 包的同源构建产物，移除 CDN 请求（78,292 B，需补齐字体归属说明）。
- C：十个页面使用模块级 lazy/Suspense，失败由局部边界承接，导航保留；入口 JS 初测 655.32 → 293.74 kB（gzip 185.30 → 90.67 kB），仍需最终构建与实际浏览器网络验证。QueryClient 的陈旧窗口 5 秒、闲置回收 60 秒；4xx 不自动重试，瞬时读取最多重试一次，写入不自动重放。
- C：抽取 API transport/SSE 模块，保留 client.ts 为业务 API 入口。SSE 统一分帧（CRLF/多行/UTF-8）、401、取消、终态 reader 释放和可取消重连等待；单事件最大 16 Mi 字符。流量 EOF 通知 MonitorPanel 重连，probe-all POST 不自动重放。标签分配改为编辑时按节点取数、传递取消，加载成功前禁止保存；不再闭合状态全量读取/用空列表覆盖未加载数据。
- D：认证网络失败有明确重试页面，移动侧栏关闭时不再可聚焦，并支持 Escape，主题改原生 button；侧栏文案不再无依据声称系统正常。其它页面/抽屉/大列表适用性继续核查。

新增 Node 内置测试运行器（npm test）覆盖真实 Headers、旧 401 与新登录交错、SSE 分帧/终态/EOF/取消、非幂等请求无重放、限定标签请求及查询重试/清空。首轮 9 个用例通过但进程被查询库 GC timer 保留；确认确切 Node 子进程后终止，测试将保留期限独立于此行为用例，加入运行超时；修正后 9/9、进程正常退出（约 132 ms）。原始输出 `frontend-tests-fixed.txt`，不把早先未退出运行视为通过。Go 新回归覆盖持久撤销/重启/失败重试/读取交错、Cookie+Bearer 退出与静态安全/缓存，正在执行。

### 页面复核与第二批修复（验证中）

- 查询与表单：节点文本筛选等停止输入 250 ms 后再查询，querySpec 始终保留当前用户输入，筛选范围不滞后给批量动作；查询旧页的保留缩短到 30 秒，disabled hook 同时停 facets/保存筛选请求。设置/运维表单从 queryFn 内写 state 改为独立 draft，避免在途读取覆盖新输入；保存期间继续输入的内容也保留。初始设置读取失败不开放默认表单保存。系统设置、运维参数与订阅刷新草稿离开时提示丢弃，覆盖 hash 导航/浏览器前后与退出。
- 失败状态：标签、分组、订阅、系统设置复用 QueryError，读取失败不再静默显示成空列表；MonitorPanel 删除吞掉配置读取错误的 catch(null)。标签变更同时失效节点查询和 facets。三处重复 Field 统一到 ui/Field，保留 Operations 的显式 htmlFor。
- 流与日志：测速改用同一 fetch/SSE 入口，抽屉只在打开时挂载，关闭/换节点/离开页面取消连接；支持 named error/progress/done，终态立即释放。调试日志按 100 ms 批量更新、待处理与展示各最多 1000 条；memo 行避免对未变历史反复做本地化日期格式化。
- 键盘与标签：共用 Modal/useModalFocus，Tab 留在弹窗内，Escape 关闭并恢复触发点；TagEditorDrawer 不再因回调 prop 每次变化重新抢焦点。测速与综合检测对话框同样使用该边界。标签小弹窗支持外点/Escape，单节点读取成功前不得用空草稿覆盖原标签。对已确认缺名的单控件 fieldset 补充 37 处 aria-label，共享 Field 自动命名单个原生控件；保留已有嵌套 label 的复选框名称。
- 认证撤销排队：将持久会话互斥改为有界可取消 gate，等待队列也包含在三秒预算内，避免慢存储下失效 token 请求无限等待；新增取消回归。

### 第一轮真实浏览器证据

新增显式 opt-in `internal/boxmgr/frontend_fixture_test.go`：真实 sing-box/管理 API/内嵌构建产物，临时 SQLite/YAML 与 1000 节点、一个 50 成员组；全部上游为 loopback 合成 HTTP/CONNECT，使用合成登录密码。缓存诊断只避免公网落地请求，不伪造路由健康；第二轮真实探测 1000/1000 成功。通过命名停止文件优雅退出，默认普通测试跳过。第一轮上游误拒 CONNECT，出现 0/1000 与启动等待；停止并修正测试代理后再运行，未修改生产健康逻辑掩盖 fixture 问题。初次文件保存缺初始 YAML/ReadWriter 类型错误也已修正，保留失败日志。

浏览器检查十页（节点监控、订阅、节点管理、维护、标签、分组、解锁、运维、调试、设置）；订阅刷新服务在该轮 fixture 未装配，看到明确“未初始化”错误，后续 fixture 将补齐真实服务。标签创建成功，弹窗 Shift+Tab 留在内部、Escape 返回“添加节点”；恶意形态节点名 `<img ... onerror=...>` 在管理/分组/解锁以文本呈现；未触发执行。退出后刷新仍为登录页，重新登录后刷新凭 Cookie 恢复；控制台无 error/warn（该轮观察范围）。

1440×900 桌面与 390×844 手机实查：设置页主区域宽 380、内容宽 634，确认为横向溢出；增加显式 grid-cols-1、fieldset/卡片可收缩，待新产物复测。其余八个非监控页手机主区域内容宽等于视口（表格内部滚动保留）。截图在忽略目录 `screenshots/settings-{desktop,mobile-settled}.png`。监控还出现健康节点缺少延迟样本却显示“超时”，已将未知桶文案改为“未测得”，不改后端健康/延迟事实。

当前仍未完成全目标：需复测新产物、补齐依赖干净安装/页面与性能证据、全量最终门禁与文档同步；版本尚未递增，未提交 Git。

### 干净安装与第二轮证据

`npm ci --ignore-scripts` 后 `npm ls --depth 0` 无 peer/安装异常，完整与 `--omit=dev` 两次 audit 均 0；lint、10/10 Node 行为测试与 tsc/Vite build 通过。新构建入口 295.14 kB（gzip 91.20 kB），比最初 655.32/185.30 kB 减少约 55%/51%；gzip 是 Vite 估计，不宣称 Go 静态服务已经使用 gzip。五张 README 预览共 607261 B 移至 docs/images 并同步引用，退出 public 产物；移除默认 React/Vite 图片，使用项目六边形图标和 zh-CN 语言。国旗字体同源且附归属、CC BY 4.0 链接。

CI 两个前端构建入口加入 npm test，Docker 前端阶段改用 lint/test/tsc+Vite，避免镜像仅用 vite 跳过类型检查。测试编译目标为 Node 22，与现有 CI/镜像一致；当前本机实际 Node 24.21。未在本机运行 GitHub Actions 或 Docker 容器。

完整 Go 首轮被 fixture 的 subscription→boxmgr 测试导入环阻断，已将 fixture 改为 boxmgr_test 黑盒包，通过公开 MonitorServer() 组装正常订阅服务；未改生产包边界。修正后的 boxmgr/controlplane/httpapi 定向全包测试通过，完整检查将重新执行。

新 fixture 以 10000 节点启动、10000/10000 真实探测成功，订阅服务正常装配。手机设置主区 client/scroll 均 380 px（修复前 380/634），截图 `screenshots/settings-mobile-fixed.png`；登录控件现在有“管理密码”名称。冷开设置页的 DOM modulepreload 只包含设置页和共用依赖，不含其它九页代码；这是资源链接观察，未报告真实网络计时或 FPS。

在验证未保存草稿退出时，IAB 原生 JS confirm 使输入命令超时，后续 getJsDialog/关闭也未成功；该交互未计通过。尝试用预安装 Edge 进行独立验证后，Windows Computer Use 因无法可靠识别当前浏览器 URL 而停止本轮 UI 操作。没有绕过策略、操作 Codex UI 或修改安全设置；仍须在后续浏览器验证草稿确认/后退与数据保留，不能据工具失败宣称该交互正确或存在产品漏洞。浏览器视口曾在第一轮结束恢复，第二轮临时手机视口因工具停止尚未确认恢复；测试标签与 Edge 新标签仅用于此隔离实验。

### 阶段一代码门禁与提交

自审对照真实服务协议补充两项边界：已结束任务重连首先返回 type=task，客户端现在在释放 reader 前补发一次 UI done，避免只看到完成快照却未退出“检测中”；新增四种终态回归。旧标签分配接口把空参数当全量，故显式空 ID 在客户端直接返回空结果，不发送请求；单节点编辑在后台刷新完成前也禁用修改/保存，避免基于旧缓存覆盖标签。

最终阶段一验证全部成功（原始文件在 `.tools/frontend-audit/`）：

- `npm run lint`、`npm test`（11/11，约 131 ms 正常退出）、`npm run build`：`{lint,test,build}-phase1-final.txt`。入口最终 295.20 kB / gzip 91.22 kB，初始为 655.32 / 185.30 kB；源码 TS/TSX 10095 行。
- `GOTMPDIR=.tools/frontend-audit go test -p 1 ./...`：`go-test-phase1-repaired.txt` 全包通过。fixture 默认跳过，真实 1k/10k 运行另有证据并已通过停止文件优雅结束。
- Windows `go build -o .tools/frontend-audit/phase1-windows.exe ./cmd/easy_proxies`；CGO_ENABLED=0、GOOS=linux、GOARCH=amd64 下全生产 tags `go vet ./...` 和 `go build -trimpath -ldflags '-s -w' ... ./cmd/easy_proxies`：`build-windows-phase1.txt`、`vet-linux-phase1.txt`、`build-linux-phase1.txt` 均退出 0。tags 为 `with_utls with_quic with_grpc with_wireguard with_gvisor`。
- `git diff --check` 通过；修正了删除本地 Field 后残留的 EOF 空行。Go 生产 41339、测试 25741 行，Store 方法 111、migration v25 未变。同步 AUTH-002 / WEB-001、架构/子系统/贡献者说明。

本阶段按 code-standards §L 自审，没有旁路统一成员/节点/JSON/写入服务入口，没有把诊断当健康；只有合成 fixture 凭据和源码文档入库，实际配置、SQLite、profile、截图及编译产物均不提交。未执行 race（CGO_ENABLED=0、无 gcc/clang）；Linux 完成 vet/链接，未运行 Linux 浏览器/负载。当前阶段提交保留可复查的代码与证据；总体 C/D/E 尚待收尾，不宣称完整前端目标已验收，版本在整体完成时递增一次。

### 收尾优化与隔离验证

第一阶段提交 b0405c3。新增 opt-in `frontend_controls_test.go`：只有 EASY_PROXIES_FRONTEND_CONTROLS=1 的 loopback 测试代理注入同源测试脚本；不改生产构建、CSP 或接口。可按路径暂挂/失败请求、统计请求与取消、发送合成日志突发；不记录请求头、凭据或请求体。测试按钮控制 confirm 返回值，因此验证的是应用对取消/接受的处理，未声称自动操作了原生操作系统确认框。

10000 节点夹具（第五轮）真实启动探测 10000/10000；设置草稿 127.0.0.2 在取消导航/退出后保留，接受后离开；后退返回已保存值。另一次服务端确认 GET /api/settings 在途数为 1，输入 127.0.0.3 后释放读取，值仍保留；取消后退仍停留设置并保留输入。订阅页服务已正常装配，明确显示零订阅/零 API Key 空态。节点管理总数 10000、tbody 仅 50 行。维护页用持久统计而非瞬时运行态，fixture 未启用 app 统计落库，故此轮显示 10000 待检测，不把它计为生产健康错误。

第五轮注入 10000 条日志，显示上限 1000、MutationObserver 观察日志容器一批 childList 更新；离开后 debug SSE 活跃数 0、取消数 1。这里只是 DOM 更新批次数，不是 FPS。该轮使用已提交的第一阶段静态产物，用于批处理基线；通过命名停止文件结束，测试 PASS，793.56 秒。

本批将 pending.slice(-1000) 改为固定容量环形缓冲，每事件缓存键和数值时间戳；细节、与原态/阶段一的 CPU 基准见 [架构复核](../architecture-review-2026-10-28.md)。历史读取加入 AbortSignal。新增回归验证 10007 项回绕、drain 引用释放/复用、去重/排序/行对象稳定、Debug 请求取消；用虚拟时间验证真实 QueryClient 闲置页 30 秒和普通查询 60 秒清理（不等待真实一分钟，不以测默认值代替实际移除）。Node 全部 15 项通过并正常退出。

## 最终逐页评估

保留页面级组件与 Query 管理服务器状态的结构；抽取共享 transport/SSE/Field/Modal/错误与日志 helper 即可，不引入额外状态库或新路由框架。十页均在真实最终构建中验证 1440×900 与 390×844；每页 main.scrollWidth 等于 main.clientWidth，表格内部滚动不扩大页面。桌面主区域 1174/1184 px，手机 380/390 px，差异来自页面纵向滚动条。逐页 DOM 记录在忽略目录 `browser-layout-final.json`，截图仅为测试证据不入库。

| 页面 | 合理性、改动与实测 | 保留限制 / 建议 |
| --- | --- | --- |
| 监控 | 聚合图表与流量 SSE 适合概览，修正未知延迟文案、读取失败与 EOF 重连；1k/10k 正常显示 | 仍读取全量节点，未来增加轻量摘要，减少管理轮询对后端的 O(N) 影响 |
| 订阅 | 来源、同步与 API Key 权限说明集中，真实服务空态正常；刷新草稿有离开提示 | 添加即刷新/改地址清旧节点已有明确说明；未对公网订阅发请求 |
| 节点管理 | 10k 集合只渲染 50 行，详情按当前页 ID 读取；标签编辑在打开后才请求单个 ID | 跨页批量动作按当前 querySpec；破坏性动作验证取消，不执行删除真实数据 |
| 维护 | 持久化健康统计与连续离线天数适合清理，清楚说明最多 30 秒延迟；无待清理时为空态 | 与瞬时监控来源不同，不能将 fixture 未运行统计落库当成健康不一致缺陷 |
| 标签 | QueryError、刷新和重试可见；首轮创建标签与键盘弹窗验证通过，最终空态正常 | 最终注入 /api/tags 503，显示错误/重试；解除后恢复。规则编辑保留专门抽屉，未把复杂表单塞入共享 Field |
| 分组 | 独立监听、成员、运行状态集中；真实 50 成员组显示，移动端无横向溢出 | 资源仍按重叠组放大，应结合资源 API 控制组数量 |
| 解锁 | 仅健康节点范围、查询/详情分页，抽屉只在打开时挂载并取消流；结果未知不假装成功 | 表格保留独立滚动和稳定 ID；未以本次浏览器试验认证公网服务检测准确率 |
| 运维 | 调度、进度、运行态集中；纯读取与草稿分离、离开提示、检测终态/重连回归通过 | 频繁状态轮询有后端成本；规模建议与架构复核共用证据 |
| 调试 | 有界环形缓冲、100 ms 批处理、memo 行、历史与 SSE 取消；真实最终页面显示 1000 条上限 | 历史 API 仍为全节点，合并窗口有损；不是持久审计页面，不应接管健康事件日志职责 |
| 设置 | 明确加载/失败态、独立草稿、保存中输入保留；读取在途后输入与取消导航/后退实测通过 | 原生 beforeunload 与 OS 确认框未自动验收；本次 confirm 返回由隔离测试脚本控制 |

## 最终构建与浏览器验收（2.2.18）

第六轮夹具使用最终构建产物、10000 节点、真实启动探测全部成功。注入 10000 条日志后显示 1000 行，初次观察两批日志容器 DOM 更新（history/stream 可以分别到达，不等于 FPS）。服务端暂挂历史请求时 history/SSE 活跃各 1，离开后均 0、history 取消 1；整个巡检多次进入调试页后 SSE 累计取消 4。当前页配置/运行态/诊断/解锁详情请求均按 50 个 ID，标签弹窗请求 node_ids_count=1，没有闭合状态全量 assignments 读取。

破坏操作的删除弹窗准确显示恶意形态名称的文本，取消后弹窗消失、50 行仍在，服务端没有 DELETE 请求。注入 TagsPanel 脚本 503 后显示“页面加载失败”，仍可切换系统设置；解除并刷新后恢复。注入标签 API 503 显示错误/重试，解除后成功。控制台唯一 error 是预期注入的动态导入失败，没有把它隐去后声称全程零错误。退出后刷新仍显示登录页。手机与桌面十页复核后恢复默认视口；命名停止文件优雅退出夹具，456.12 秒 PASS。

最终检查按前端先于 Go 的顺序执行，全部退出码 0：

```powershell
# frontend 目录
npm run lint
npm test
npm run build
npm audit --json
npm audit --omit=dev --json
# 仓库根目录
$env:GOTMPDIR = (Resolve-Path .tools/frontend-audit).Path
$env:CGO_ENABLED = '0'
go test -p 1 ./...
go build -o .tools/frontend-audit/final-windows.exe ./cmd/easy_proxies
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go vet -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' ./...
go build -trimpath -ldflags '-s -w' -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' -o .tools/frontend-audit/final-linux-amd64 ./cmd/easy_proxies
git diff --check
```

lint / 15 项行为测试 / tsc+Vite build 日志为 `lint-final.txt`、`test-final.txt`、`build-final.txt`；入口最终 295.22 kB（gzip 91.23 kB，Vite 估计），初始 655.32/185.30 kB。完整和生产 npm audit 均 0，代表本次锁定依赖的已知公告扫描，不是所有安全缺陷不存在的证明。Go 日志 `go-test-final.txt`、`windows-build-final.txt`、`linux-vet-final.txt`、`linux-build-final.txt`；Windows 二进制 48972288 B，Linux 51175584 B，均在忽略目录。

版本三处同步 2.2.18，计数 Go 生产 41339、测试 25893、前端 TS/TSX 10116，Store 接口 111、migration v25 不变。当前架构、前端子系统、规格、CI/Docker 前端门禁与审计索引已同步；原始截图/日志/临时配置/SQLite/二进制不入 Git。未执行 race（无 gcc/clang，CGO_ENABLED=0）、Linux 运行态负载或本地 Docker/GitHub Actions；这些不计为通过。架构后续容量建议保留在独立复核报告，不冒充已实现。完成自审后提交本地 Git，不推送、打 tag 或发布。
