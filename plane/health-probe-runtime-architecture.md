# 健康探测运行时架构

本文描述健康探测重构后的代码边界与并发不变量。实现以 `internal/monitor` 为准。

## 组件边界

```text
HTTP / boxmgr / subscription
            |
            v
    monitor.Manager（兼容门面）
       |          |           |
       v          v           v
 NodeRegistry  ProbeScheduler  HealthEventHub
       |          |           |
 runtime entry  bounded I/O   tag / Node ID reverse index
                              |
                              v
                        affected pools only
```

### Manager

`Manager` 保留 HTTP、boxmgr 和 pool 已使用的公开接口，负责配置快照、节点健康结果提交、流量与 debug 数据。它不再拥有探测队列、轮次门禁或 runtime tag map。

### NodeRegistry

`NodeRegistry` 原子维护两份索引：

- concrete runtime tag → entry；
- stable Node ID → entry。

注册、版本迁移、reload generation、stale sweep 和注销都在同一组件内完成。Node ID 查找不再扫描全部节点；registry 锁不会跨网络操作。

### ProbeScheduler

`ProbeScheduler` 是 startup、periodic 和 manual 探测的单所有者 actor：

- actor 决定轮次顺序、任务合并、首次收敛和 delayed retry；
- 网络操作进入有界 executor，完成后通过事件回到 actor；
- 每个 round 固定使用开始时的 `ProbePolicy`；
- runtime tag lease 在调度时取得，在最终结果、取消或版本迁移后释放；
- 500ms 首次重试由 actor timer 调度，不占 worker；
- 周期调度计算最近 deadline，只在到期或运行事实变化时唤醒，不做每秒全表扫描。

手动批量探测仍保持单轮互斥语义。已有轮次运行时返回 `ErrProbeRoundInProgress`，SSE 的结果回调保持串行。

### HealthEventHub

健康结果提交 entry 后，按 runtime tag 和 stable Node ID 投递。Pool 在发布新成员拓扑时同步替换订阅索引，因此单次结果只触发实际包含该节点的 pool，不再广播给所有 pool。同一订阅同时匹配 tag 与 Node ID 时只调用一次。

## 首次探测收敛

```text
enqueue runtime tags
        |
        v
reserve tag leases -> first-attempt wave
        |                    |
        | success/missing    | retryable failure
        v                    v
 commit terminal      delayed task (500ms)
                             |
                             v
                     second-attempt wave
                             |
                             v
                     commit terminal result
        |
        v
rescan runtime entries until pending == 0
```

收敛目标只包含已注册的 runtime entry。最终状态可以是可用或不可用，但不能永久停留在“待检查”。版本迁移后，旧 work item 不会拨号或写入新 generation；新 tag 由后续收敛任务处理。

## 并发不变量

1. 任意时刻最多一个 batch round；单节点 standalone probe 与 batch 共用 tag lease。
2. actor、registry 和 event hub 的锁均不跨网络拨号。
3. 失败重试不持有 worker；重试期间只保留具体 runtime tag 的 lease。
4. result 先提交 entry，再投递 pool observer。
5. 取消不会合成不可用结果；已取得的 lease 必须释放。
6. scheduler 不依赖 watchdog；入队命令、lease 释放和 deadline timer 都是显式唤醒源。

## 验证重点

- 1,000 节点并发上限与每节点一次执行；
- concurrency=1 时，后续节点首次尝试先于前序失败节点重试；
- startup backoff、网络执行和服务停止三个阶段的取消；
- runtime tag 迁移时旧结果隔离与 lease 释放；
- 周期 deadline 前不运行、deadline 到达仅触发一次；
- targeted health dispatch 的去重和动态成员索引替换；
- `go test ./...`、`go vet ./...`、支持 CGO 的环境执行相关 `go test -race`。
