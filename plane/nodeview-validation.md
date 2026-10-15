# 节点视图与分组自愈验收

日期：2026-10-15。阶段 0–6 已实现并分批提交到本地 Git，未推送远端。

## 交付范围

- 地区证据集中写入：失败保留已知地区、身份修订/乱序保护、独立探测两次确认地区变化。入口地址不参与出口地区推断。
- 地区别名与未知语义：新版规则区分未知和已知 other；旧组/标签规则保留版本语义。
- 统一 nodeview 查询、facets、分组预览、命名筛选，以及管理/监控/解锁/分组页面接入。成员判定保留 excluded > explicit > 规则契约。
- ProbeResult/Applier 统一检测持久化与投影重试；手动诊断不产生路由健康事件。导入节点进入待测路径，身份替换清理旧健康事实，订阅增量合并保留未变身份的历史。
- 容量自愈支持活动/备用选择、受控复检、订阅刷新、全局预算、冷却/退避/熔断与动作日志。默认 dry-run；执行需显式启用。复检遵守现有恢复冷却与成功阈值，操作说明见 [group-healing.md](group-healing.md)。
- migration v15–v19 追加地区证据、规则版本、保存筛选、检测回执及容量策略/日志。
- 发布 CI 仅生成 Linux amd64/arm64 二进制及校验文件；Docker job、Dockerfiles、compose 和独立 Docker workflow 均未改动。

## 验证结果

使用 Go 1.27.1；工具链、日志、隔离数据库与二进制位于忽略目录 `.tools/`。

| 检查 | 结果 |
| --- | --- |
| `go test -json ./...` | 全部 24 个有测试的后端包通过 |
| `go test ./internal/monitor` | 最后补充待检测候选修复后通过 |
| `npm run lint`、`npm run build`（frontend） | 通过；最终提示文案修改后再次通过，Vite 有大 chunk 提示 |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -p 4 -trimpath -ldflags "-s -w" -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" ./cmd/easy_proxies` | 通过，最终前端资产已嵌入 |
| 上述生产构建改为 `GOARCH=arm64` | 通过，最终前端资产已嵌入 |
| ELF 标识及机器类型 | amd64=62、arm64=183，均通过；生成 SHA-256 |
| CI YAML 解析与 Docker 差异检查 | 通过；不等同于远端 Actions 实跑 |
| 本地浏览器验收 | 监控未知地区独立计数；香港筛选仅 1 个节点，包含未知后 2 个；分组预览保留不可用显式节点；自愈日志展示模拟动作；解锁页使用统一筛选；未捕获控制台警告/错误 |

浏览器使用仅监听回环地址的隔离实例与两个不可达测试节点，不使用真实订阅或生产数据库。验收截图位于 `.tools/ui-check/healing-preview.png`。

产物位于 `.tools/release/`：`easy-proxies-linux-amd64`、`easy-proxies-linux-arm64`、各自 `.sha256` 及 `SHA256SUMS`。Linux 产物在本机仅交叉构建并核验 ELF，未在真实 Linux 主机运行。

## 尚未实跑的环境检查

本机 `go test -race ./internal/group` 因缺少 gcc/C 编译器在构建阶段失败，不能算作 race 检测通过。Ubuntu CI 已配置 `go test -race ./internal/group ./internal/outbound/pool ./internal/monitor ./internal/probe`；用户推送后才能验证该检查和完整远端发布流程。

普通推送不触发此发布 workflow；`v*` tag 触发构建与 Release，手动 workflow_dispatch 可验证构建。Docker 保持原有触发及行为。
