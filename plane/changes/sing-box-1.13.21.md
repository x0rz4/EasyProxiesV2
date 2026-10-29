# 升级内嵌 sing-box 至 1.13.21

日期：2026-10-29；基线 9887a12；项目版本 2.2.20。

## 范围与兼容边界

按用户指定版本将 `github.com/sagernet/sing-box` 从 v1.13.19 升至 v1.13.21，仍以库形式内嵌。执行精确 `go get github.com/sagernet/sing-box@v1.13.21` 和 `go mod tidy`，同步 go.mod/go.sum，不使用全量 `go get -u`。

最低版本选择联动更新三个间接依赖：sing-quic 从 v0.6.4-0.20260803041914-d83826c306d7 至 v0.6.5、sing-tun 从 v0.8.12-0.20260810140523-7c73233bd0fb 至 v0.8.15、sagernet/tailscale 从 v1.92.4-sing-box-1.13-mod.9 至 mod.10。保留 sing v0.8.14 与现有 Go toolchain。

tidy 同时移除不再需要的 go-json-experiment/json 显式间接要求和校验项，将已有 Windows RSS 实现直接导入的 x/sys v0.47.0 归为直接依赖；后者版本未变。其余依赖版本保持原选择，不把下载已有模块误记为本批升级。

上游 [v1.13.21 发布说明](https://github.com/SagerNet/sing-box/releases/tag/v1.13.21) 只概述修复和改进；[该版本 go.mod](https://github.com/SagerNet/sing-box/blob/v1.13.21/go.mod) 仍使用 x/net v0.50.0、x/crypto v0.48.0，本项目继续保持这两个兼容钉版，以及 sqlite v1.52.0、geoip2 v2.2.0。不能将此次升级当作此前 TCP 内存压力或 Killed 故障已经解决的证据。

无应用源码、配置字段、数据库结构或迁移变更；项目版本三处同步 2.2.20，当前架构依赖表同步精确版本。既有标签不移动，本任务不打发布标签。

## 验证

以下检查全部成功；前端构建完成后才执行 Go 测试/构建，未修改构建产物：

```powershell
go mod verify
# frontend 目录
npm run lint
npm test
npm run build
# 仓库根目录
$env:GOTMPDIR = (Resolve-Path .tools/sing-box-upgrade).Path
$env:CGO_ENABLED = '0'
go test -p 1 ./...
go build -o .tools/sing-box-upgrade/windows.exe ./cmd/easy_proxies
go version -m .tools/sing-box-upgrade/windows.exe
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go vet -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' ./...
go build -trimpath -ldflags '-s -w' -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' -o .tools/sing-box-upgrade/linux-amd64 ./cmd/easy_proxies
$env:GOARCH = 'arm64'
go build -trimpath -ldflags '-s -w' -tags 'with_utls with_quic with_grpc with_wireguard with_gvisor' -o .tools/sing-box-upgrade/linux-arm64 ./cmd/easy_proxies
```

`go mod verify` 返回 all modules verified；前端 15 项行为测试通过。Go 全量包测试通过，Windows 本机链接、Linux amd64 全生产 tags vet/链接、Linux arm64 全生产 tags 链接通过；生成二进制的 Go build info 确认 sing-box v1.13.21。版本选择输出 `selected-modules.txt`、测试/编译日志与二进制保留在忽略目录 `.tools/sing-box-upgrade/`，不入库。

另在 Windows 上运行既有 opt-in 本地 CONNECT/echo 实验：100 个合成节点、1 组、1 条连接、3 秒，校验 payload、活动连接及结束归零；用例 PASS，结果 `local-forward.{json,txt}`。命令：

```powershell
# 新 PowerShell 会话使用本机默认 GOOS/GOARCH
$env:GOTMPDIR = (Resolve-Path .tools/sing-box-upgrade).Path
$env:EASY_PROXIES_SCALE_EXPERIMENT = '1'
$env:EASY_PROXIES_SCALE_CASE = 'n100-g1-p100-c1'
$env:EASY_PROXIES_SCALE_SECONDS = '3'
$env:EASY_PROXIES_SCALE_OUTPUT = Join-Path $env:GOTMPDIR 'local-forward.json'
go test -p 1 ./internal/boxmgr -run '^TestPoolScaleExperiment$' -v -count 1 -timeout 90s
```

该实验只验证本机转发正确性，没有与旧版本作同条件性能对比，不能宣称吞吐或生产内存改善。未执行 race（本机无 C 编译器）、Linux 实机负载和所有协议的端到端互通。推送后的远端 CI 状态单独判断，不以本地交叉编译替代远端测试。
