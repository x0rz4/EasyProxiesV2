# 子系统：前端（React 19 + TS + Vite 7）

改动 `frontend/src/` 之前先读这份。

## 技术栈与构建

React 19 + TypeScript + Vite 7 + Tailwind CSS 4 + DaisyUI 5 + TanStack Query + lucide-react + sonner。

**产物直接落到 `internal/httpapi/assets/`**，被 `//go:embed assets/*` 内嵌进二进制 → 单文件运行。`vite.config.ts` 的 `emptyOutDir` 会**先清空**该目录，因此**前端构建与 `go build`/`go test` 绝不能并行**。没有产物时 `go build ./...` 必失败。

无单测框架，验收标准 = `npm run lint` + `npm run build`（含 `tsc -b` 严格检查，`noUnusedLocals`/`noUnusedParameters` 开启）通过。

## 目录约定

```
frontend/src/
  api/client.ts          全部 API 调用（唯一出口）
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

**表单字段 `Field` 目前不是共享组件**：`GroupPoolsPanel.tsx`、`OperationsPanel.tsx`、`tags/TagEditorDrawer.tsx` 各自定义了本地 `Field`，三者签名不同（`{label,hint,children}` vs `{id,label,hint,className,children}`）。写新表单时**不要**假设它来自 `PageLayout.tsx`。若要统一，先对齐这三份签名再上提。

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
