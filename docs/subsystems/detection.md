# 子系统：能力检测（解锁 / IP 质量 / 诊断）

改动 `internal/unlock`、`internal/ipquality`、`internal/nodedetect`、`internal/noderesult` 之前先读这份。

## 共享结果类型（noderesult）

`noderesult` 是检测结果负载的**唯一定义**（叶子包，不 import 本项目任何包）。`store` 嵌入这些类型并加元数据字段；`unlock`/`ipquality` 用 `type` 别名指回来（`Result = noderesult.Quality` 等），因此检测结果落库 / 出 API 不需要逐字段转换。

## 手动诊断（nodedetect）

HTTP 诊断入口共享进程级预算：延迟 128、测速 8、质量 10、解锁 32 个同时执行的操作。综合任务设置仍可进一步降低单任务并发；单节点与批量入口使用同一类型预算，等待可由调用方取消。预算跨 HTTP Server 更替保留，健康探测使用独立调度器，不占诊断预算。解锁以节点为单位，一个操作内 checker 仍可并发。

延迟 / 测速 / 出口 IP 的路由无关诊断。**绝不发布健康事件**——诊断成功不得清除 EVICTED、不得推进恢复计数（spec DIAGNOSTIC-001）。与真实数据面失败、显式人工恢复是三类不同的受控输入。

## 解锁检测（unlock）

流媒体与 AI 服务解锁（10 个 checker，`Key()` 为注册名）：netflix、disney_plus、youtube、bahamut、tiktok、amazon、reddit、chatgpt、gemini、claude。结果类型是 `noderesult` 别名。

**新增检测器（护栏 9）**：加一个 `checker_*.go`，实现 `Checker` 接口、在 `init()` 里 `RegisterChecker`、取一个未占用的 `Order()`。provider 字段、内置标签模板、schema 枚举会自动派生；**重复 key 或别名冲突会在启动时 panic**。当前 `Order()` 分布：netflix 10、disney_plus 20、chatgpt 30、gemini 35、youtube 40、bahamut 45、tiktok 50、amazon 60、reddit 70。

## IP 质量（ipquality）

provider 适配（ippure / ip-api）。**每个 provider 结果独立、不做综合评分**——没有 `StatusUntested` 这种"综合状态"常量，也不要在 provider 之间伪造合并分数。`IPAPIClient` 批量查询走主机本地 `&http.Client{Timeout:...}`，**不经节点 outbound、不经过 netx**（护栏 11）。`IPPureProvider` 单次查询、无连接复用。

## netx 约束（护栏 11）

经节点拨号的 HTTP 客户端一律走 `netx`，`DialFunc` 不允许为 nil。`netx` 造的是**裸** `http.Transport`（刻意：每个消费者只要它声明的那几个字段），所以 nil 拨号不等于"用标准库默认值"——那会静默丢掉 HTTP/2、代理环境变量、共享空闲连接池。消费者差异写成显式 `Options`，不在调用处重拼 transport。
