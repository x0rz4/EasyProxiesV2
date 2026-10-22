# 贡献者指南（PR / 自审入口）

> 审自己的改动和审社区 PR 走同一套清单。硬规则本体在 [code-standards.md](code-standards.md)，
> 这里只讲**流程**：PR 必须写什么、怎么跑验证、什么直接打回。

## PR 必须说明（AGENTS 传统，保留）

1. **行为变化**：用户/运维可见的差异；纯重构也要写"无行为变化"并给出证据（契约测试）。
2. **数据库影响**：有无新 migration（只许追加到 `migrations.go` 末尾，新列 `NOT NULL DEFAULT`）；有无表结构变更需要同步 `sqlite.go` 列常量 / `scan*` / `INSERT` / `UPDATE`。
3. **配置影响**：`config.yaml` / `config.example.yaml` 字段增删；store 与 config 优先级是否被破坏。
4. **确切的验证命令**：贴实际执行过的命令与结果。未执行的检查明确写"未执行"，不许沉默。
5. 可见 UI 变化附截图（截图方法见 [subsystems/frontend.md](subsystems/frontend.md)）。

## 验证矩阵

| 改动面 | 必跑 |
| --- | --- |
| 任何后端 | `go vet -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" ./...` + 对应包 `go test` |
| 跨包 / 疑似并发 | 关键包 race：`go test -race ./internal/outbound/pool/ ./internal/group/ ./internal/boxmgr/` |
| 全量 | `go test ./...`（**先前端构建**，见下） |
| 前端 | `cd frontend && npm run lint && npm run build`（含 tsc -b 严格检查） |
| 依赖变更 | `go build ./cmd/easy_proxies` 验证**链接**（x/net 钉版陷阱，见 LESSONS L-014） |
| install.sh | `bash -n install.sh` + `python3 -m unittest discover -s tests -p 'test_install.py'` |

**构建顺序**：先 `cd frontend && npm run build`，再跑任何 `go build`/`go test`——两者绝不并行（`//go:embed assets/*` + `emptyOutDir`，LESSONS L-012）。Windows 本机若被应用控制策略拦截测试二进制，用 `go test -c` 落地到仓库目录再执行，验完删除（LESSONS L-015），并在 PR 描述里如实记录哪些包换了执行方式。

## 直接打回清单

- 绕过唯一入口：直写 SQL、自 new box、`nodes.tags` 旁路写标签、本地重写"enabled 过滤"的节点集合（须走 `runtimenodes.Load`）。
- 持锁拨号 / 解析订阅 / 等 drain；`NotifyEvents` 早于快照可见。
- 用统一分发器合并三个错误→状态码策略；或对 `tagErrorStatus` 的 400 做"顺手修正"。
- 引入泛型 `handle[Req,Resp]`（LESSONS L-011 / code-standards §F）。
- migration 插在中间、无 `NOT NULL DEFAULT` 的新列、宽写接口复活。
- x/net、x/crypto、sqlite、geoip2 版本被 `go get -u` 松动而无链接验证。
- 真实订阅 / 凭据 / `config.yaml` / SQLite / 证书入库；`skip_cert_verify` 默认打开。
- 把偶发失败测试标为"环境问题"合入而无归因（先 `git stash` 在基线复现，LESSONS L-005）。
- 包移动但文档/AGENTS 路由未同批更新（LESSONS L-017 → code-standards §K）。

## 社区 PR 特别项

- 外部依赖升级 PR：先看是否动到护栏 10 的钉版四件套；CI 里"构建过"不等于"链接过"。
- 新增解锁检测器：检查 `init()` 注册、`Order()` 未占用、key/别名不冲突（冲突应启动 panic，不是静默覆盖）。
- 大型重构：要求先提契约冻结测试（DR-004），再提搬迁。
- 提交风格：Conventional Commit、祈使句、单一主题；杂七杂八的顺手改动拆出去。

## 版本与发布（本仓库规则）

- 每批任务在 `frontend/package.json` 当前版本上递增一次（小修 patch +1，大功能 minor +1 且 patch 归零；同步 `package-lock.json` 两处），同批不重复递增。
- 打 tag 前确认该 commit 的 CI 跑完且全绿（LESSONS L-013，差一次即转正为硬规则——审 PR 时按此提醒）。
- 只有稳定版本 tag 更新 `latest`；不在 Release 工作流里加回依赖 `DOCKERHUB_TOKEN` 的 Docker Hub 任务（`53bba31` 删过，别再长回来）。
