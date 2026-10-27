# 前端合理性、安全与性能审计

日期：2026-10-28。基线 fdd9d08 / 2.2.17，分支 codex/frontend-audit。状态：进行中。

## 范围与交付

审视全部十个页面及共享 API、查询缓存、流式连接、表单和依赖树。保留 React/TanStack Query 与后端一致成员谓词；根据证据进行局部重构，不凭包版本较旧就全部跨主版本升级。每批变更在此记录原因、实现、验证与局限，最终递增一次版本并提交 Git。

- [x] A 依赖：npm audit（完整/生产）、peer/engines、受影响入口分析；修复已知依赖漏洞、验证干净安装。
- [x] B 安全：登录/退出/会话恢复、缓存与凭据清理、XSS/链接/静态资源边界；实测安全回归。
- [ ] C 性能：首屏 bundle 与按需页面、查询取消/分页缓存/轮询、SSE/日志刷新开销；提供前后测量及资源清理回归。
- [ ] D 适用性：全部页面数据/空态/失败态/破坏操作与移动端、键盘使用；修复确认的问题，评估共享组件边界。
- [ ] E 验收：真实浏览器桌面/移动验证，前端 lint/build、安全/行为回归、Go 全量测试和生产 tags vet/链接；同步当前文档和最终证据，提交 Git。

## 初始证据

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
