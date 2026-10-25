# SS obfs 插件兼容修复

日期：2026-10-26。目标版本：2.2.12。

## 问题与设计

Clash SS 节点的 `plugin: obfs`、`plugin-opts.mode/host` 被直接传给 sing-box，构建实例时触发 `plugin not found: obfs`。只改插件名会让 TLS 模式退回 HTTP、丢失混淆域名。

在 builder 的唯一出站构建入口适配 `obfs → obfs-local`、`mode → obfs`、`host → obfs-host`；使用 sing-box 的 SIP003 参数解析器处理转义，原生键优先于别名。保持节点 URI、Spec、身份哈希及 Clash 导出格式，已有节点升级后可直接重载。原生 `obfs-local` 与其它插件原样传递。

本次不迁移数据库、不扩展插件种类、不调整订阅事务或运行时重载策略。当前契约新增 `OUTBOUND-001`。

## 任务与验收

- [x] 增加从 Clash 导入至出站的回归测试，以及原生插件、转义参数和异常参数用例。
- [x] 增加 boxmgr 冷启动、增量重载及全量重载的真实 sing-box 实例测试。
- [x] 完成运行时适配；更新当前文档、契约及版本。
- [x] 执行测试、前端构建及 Linux 生产标签链接验证，记录结果。
- [x] 单独提交 Git（代码、测试与本计划一并提交）。

## 验证记录

1. 修改前运行新增测试：Clash HTTP/TLS 参数断言失败；boxmgr 冷启动稳定复现 `create sing-box instance: initialize outbound[0]: plugin not found: obfs`。
2. 修改后 `go test -p 1 ./internal/builder ./internal/boxmgr ./internal/nodecodec ./internal/config ./internal/subrender ./internal/subscription -count=1` 全部通过（`CGO_ENABLED=0`、工作区 `GOTMPDIR`）。覆盖身份 golden、导出兼容以及实际内嵌 sing-box 三条生命周期路径；无需外部 obfs 进程。
3. `npm run lint`、`npm run build` 通过；Vite 仍有原有大 chunk 提示。
4. `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' -o .tools/release/easy-proxies-ss-obfs-linux-amd64 ./cmd/easy_proxies` 通过，完成真实链接。产物为本地验证构建。
5. 改动 Go 文件的换行归一化后 gofmt 检查、版本三处一致性及 `git diff --check` 通过。

未连接真实上游代理做联网测试，也未在远程服务器部署或发布 Release。
