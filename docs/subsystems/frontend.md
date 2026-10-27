# 子系统：前端（React 19 + TS + Vite 7）

改动 `frontend/src/` 之前先读这份。

## 技术栈与构建

React 19 + TypeScript + Vite 7 + Tailwind CSS 4 + DaisyUI 5 + TanStack Query + lucide-react + sonner。

**产物直接落到 `internal/httpapi/assets/`**，被 `//go:embed assets/*` 内嵌进二进制 → 单文件运行。`vite.config.ts` 的 `emptyOutDir` 会**先清空**该目录，因此**前端构建与 `go build`/`go test` 绝不能并行**。没有产物时 `go build ./...` 必失败。

安全/流式传输回归使用 Node 内置 test runner，`scripts/test.mjs` 通过显式 esbuild 开发依赖打包到忽略目录 `.tools/frontend-audit/tests/`。验收为 `npm run lint`、`npm test`、`npm run build`（含 tsc -b 严格检查）。CI 和 Docker 前端阶段均运行这三项；Node 22 构建环境，建议使用当前受支持的 22.x 或 24.x。审计基线/方法见 [前端审计](../../plane/changes/frontend-audit/plan.md)。

App 模块级 lazy/Suspense 按需加载十个页面，PanelBoundary 限制页面错误范围。国旗字体由 npm 锁定包同源输出，许可在 public/FONT-LICENSE.txt，页脚可访问；README 预览保留在 docs/images，不再作为 public 文件进入二进制。HTML lang=zh-CN；图标沿用管理台六边形。

系统设置的基础池调度支持显式 `balance_p2c`，并提示“双随机候选中取较少连接，不保证全池最少”；不改变分组调度选项或已有默认选择。

## 目录约定

```
frontend/src/
  api/client.ts          业务 API 唯一入口
  api/transport.ts       内存 Bearer、统一 Headers/401/非缓存请求
  api/sse.ts             有界分帧、取消/reader 释放与重连等待
  queryClient.ts         5 秒陈旧窗口、60 秒闲置回收、读取重试政策
  types/index.ts         共享类型
  utils/cn.ts            cn() 合并类名
  utils/format.ts / fuzzySearch.ts / region.ts / unlockDisplay.ts
  components/            页面与面板
    ui/PageLayout.tsx    页面骨架：PageLayout / PageHeader / PageContent + surfaceClass / controlClass
    tags/                标签体系组件（ConditionRow / TagEditorDrawer）
    charts/              图表
```

组件文件 `PascalCase.tsx`；hooks / 工具 `camelCase`；两空格缩进。

## 页面骨架

复用 `components/ui/PageLayout.tsx` 的 `PageLayout` / `PageHeader` / `PageContent`，以及同文件导出的 `surfaceClass` / `controlClass` 类名常量。

`ui/Field.tsx` 统一分组/运维/标签编辑器字段，支持可选 id/className/hint，保留显式 label 关联并命名单个直接原生控件。`ui/Modal.tsx` 和 `hooks/useModalFocus.ts` 共用首次焦点、Tab 环绕、Escape 关闭和关闭后恢复；onClose 最新回调由 useEffectEvent 读取，避免编辑时重复抢焦点。`ui/QueryError.tsx` 承接查询失败/重试，不能把失败当空列表。

设置和运维参数将服务数据与本地 draft 分开，查询函数不写表单 state；在途读取/保存响应不覆盖保存后新输入。`useUnsavedChanges` 对系统设置、运维参数、订阅刷新草稿保护整页关闭、hash 导航和退出；强制 401 仍直接清除认证。未保存草稿没有落浏览器持久存储。

浏览器会话只用服务器 HttpOnly Cookie 恢复，Bearer 只在内存保留，旧 auth_token 存储键在启动时清除。启动检查真实 authenticated 状态，网络失败提供重试；退出必须先成功撤销服务器会话，再取消并清空 QueryClient。旧会话请求的 401 不注销新登录。主题存储受限时降级为内存偏好。

节点文本筛选等待 250 ms 静止后请求，querySpec 始终与输入一致，筛选查询闲置保留 30 秒；分页详情仍传 AbortSignal。标签分配仅在编辑时读取目标节点，初次加载成功前禁用保存。SSE 共用 16 Mi 字符事件上限与分帧器，正确处理 CRLF/多行/UTF-8；流量 EOF 触发重连，mutating probe-all POST 不重放，检测任务 GET 重连后读取新快照。测速抽屉关闭/换节点/卸载即取消，终态释放 reader。调试流每 100 ms 合并渲染一次，队列和显示各最多 1000 条。

`utils/runtimeLogs.ts` 提供固定 1000 槽的待处理环形缓冲；满载覆盖最早到达项，drain 后释放槽引用。每条事件只计算一次去重键与时间戳，合并保留未变行对象，供 memo 行复用。Debug 历史请求也接收 Query 的 AbortSignal，离开页面取消在途读取。该队列是即时展示的有损窗口，不替代后端健康日志；后端历史与节点元数据读取仍随节点总数增长。

## 页面与表格滚动

- 解锁检测页与节点管理页使用默认 `PageLayout` / `PageContent`，由 App 的 `<main class="overflow-y-auto">` 整页滚动。
- 两页表格使用 `max-h-[calc(100vh-280px)] min-h-[400px]` 独立纵横滚动，表头固定；筛选栏不锁在固定高度视口内。解锁页手机端标题操作仅显示图标，保留可访问名称与提示。

解锁页基础范围仅在线可用节点：复用 nodeview.Healthy，排除未加载、待检查、不可用、黑名单、停用及临时放行节点。`useNodeFilter` 的 scope 在用户筛选之后施加，清除/加载保存筛选不能放宽这个范围。已移除统计卡片及标签归属提示，诊断筛选保留。

`NodeFilterBar` 的可选 `countDisplay="healthy"` 仅用于解锁页：本页数量来自查询 items，匹配总数来自查询 total，在线可用总数来自 healthy facets；加载与失败分别显示查询中、数量暂不可用，后台刷新显示更新中。默认模式沿用原文案，节点管理页不受影响。“全部”诊断按钮显示在线可用总数。

解锁表格按查询页的稳定节点 ID 渲染、选择和排序，不因详情暂缺而过滤掉行；缺失运行态时显示“运行态更新中”并禁用单节点操作，抽屉也只使用当前运行态。页码超出最新总数时回到最后有效页。

## 多选控件

不要用原生 `<select multiple>`：DaisyUI 5 的 `.select` 带 `display:inline-flex`，会让 `<option>` 当 flex item、宽度收缩到最小内容宽度导致**逐字竖排**。用 `NodeFilterBar.tsx` 的 `MultiSelect` 下拉浮层（收起 `h-8`，展开 absolute + 空间避让 / 向上翻转）。

## UI 截图验证（本机无 playwright/puppeteer，有 Edge）

```
"/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe" --headless=new \
  --disable-gpu --hide-scrollbars --virtual-time-budget=9000 \
  --window-size=W,H --screenshot=out.png URL
```

配 vite dev server + 临时入口 html（如 `frontend/panel-check.html` + `src/panel-check.tsx`，**验完必须删除**）。用 `?open=N` 之类 query 参数 + `setTimeout` 里 `document.querySelectorAll('[aria-haspopup="listbox"]')[N].click()` 可截到浮层展开态。

节点管理与解锁检测页先查询当前页 ID，再按 ID 获取配置、运行态及诊断结果；详情请求随页面切换取消。全局数量来自 facets，跨页选择只保存 ID，检测筛选全体走 query-ids。批量探测在点击后分批解析所选运行态，每次最多 200 个；不会为显示一页后台抓取所有节点。

解锁页已选批量检测在提交前通过 healthy_only 的 query-ids 与跨页选择取交集，提示并移除不再在线可用的目标。检测筛选全体也携带同一健康范围；状态变更按当前查询快照判断，后续检测错误仍按原任务流程保存，不额外发布健康事实。
