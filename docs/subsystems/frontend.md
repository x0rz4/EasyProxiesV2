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

## 固定视口 vs 整页滚动

- 解锁检测页：`PageLayout lg:h-[calc(100vh-4rem)] lg:overflow-hidden` + `PageContent fill lg:overflow-hidden`，**只有表格区内部滚动**。
- 节点管理页：`PageLayout` 默认 `min-h-full`，由 App 的 `<main class="overflow-y-auto">` 整页滚动。

改这两个页面高度时要留意差异——往固定视口页里放常驻展开的高控件会把内容撑爆裁掉。

## 多选控件

不要用原生 `<select multiple>`：DaisyUI 5 的 `.select` 带 `display:inline-flex`，会让 `<option>` 当 flex item、宽度收缩到最小内容宽度导致**逐字竖排**。用 `NodeFilterBar.tsx` 的 `MultiSelect` 下拉浮层（收起 `h-8`，展开 absolute + 空间避让 / 向上翻转）。

## UI 截图验证（本机无 playwright/puppeteer，有 Edge）

```
"/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe" --headless=new \
  --disable-gpu --hide-scrollbars --virtual-time-budget=9000 \
  --window-size=W,H --screenshot=out.png URL
```

配 vite dev server + 临时入口 html（如 `frontend/panel-check.html` + `src/panel-check.tsx`，**验完必须删除**）。用 `?open=N` 之类 query 参数 + `setTimeout` 里 `document.querySelectorAll('[aria-haspopup="listbox"]')[N].click()` 可截到浮层展开态。
