# Multi-port 按需监听与 Hybrid 热点节点优化

## 总结

将现有 multi-port listener 从基础 sing-box 实例中拆出。默认 `eager` 保持兼容；显式启用 `on_demand` 后，只有已激活的节点拥有独立运行实例和 listener，冷节点不占 FD、不参与基础实例 Reload。

Hybrid 模式下所有节点仍可通过 pool 使用，只有置顶或近期使用的热点节点获得独立端口。纯 multi-port 的冷节点必须先通过管理页或 API 激活。

## 配置与运行时

- 扩展 `multi_port` 配置：
  - `activation_mode: eager | on_demand`，默认 `eager`。
  - `max_active: 200`，限定活动 listener 和可用端口槽数量。
  - `idle_timeout: 30m`，仅回收未置顶、无活动连接的节点。
- on-demand 端口槽固定为 `base_port` 至 `base_port + max_active - 1`：
  - 冷节点没有对外端口，激活时分配空闲槽。
  - 达到上限时回收最久未访问且无连接的非置顶节点，并复用其端口。
  - 置顶节点在配置不变时保持端口；普通节点重新激活后端口可能变化。
  - `nodes.port` 保留为 eager 模式兼容配置；on-demand 实际端口单独保存，不破坏切回 eager 后的原端口。
- 基础实例保留所有节点 outbound、健康监控和 hybrid/pool 共享入口，不再构建 on-demand 节点 listener。
- 每个活动节点使用独立 `nodeBox`，只包含该节点 outbound、单成员 observer pool 和一个 inbound：
  - 激活、回收和置顶操作不调用全局 Reload。
  - 节点 CRUD 或订阅刷新仍更新基础拓扑，但只重建受影响的活动 nodeBox。
  - listener 接入成功后更新 LRU 时间；TCP/UDP 活动连接归零前不会自动回收。
- eager 模式节点超过 200 只记录日志并在界面警告，不强制截断，保证旧配置行为不变。
- pure multi-port + on-demand 中，未激活端口直接不可用，不实现透明首包激活；不引入 Linux 专属 nftables/TPROXY 依赖。

## 状态、API 与界面

- 新增 `node_multi_port_states` 表，按节点保存：
  - `pinned`
  - `desired_active`
  - `bound_port`
  - `last_access_at`
  - `activated_at`
- 重启时优先恢复置顶节点，再恢复 30 分钟内使用过的普通节点，按最近访问排序并受 `max_active` 限制；禁用或已删除节点不恢复。
- 新增接口：
  - `GET /api/multi-port/status`
  - `POST /api/multi-port/nodes/{id}/activate`
  - `POST /api/multi-port/nodes/{id}/deactivate`，有连接时返回 `409`，可用 `force=true` 强制断开。
  - `PUT /api/multi-port/nodes/{id}/pin`，请求体为 `{ "pinned": true | false }`。
- 激活和停用为幂等操作；端口容量不足、节点被禁用、端口冲突和置顶节点直接停用均返回明确的 `409` JSON 错误。
- 置顶冷节点时自动激活；取消置顶后保持活动并重新获得 30 分钟空闲期限。置顶数不能超过 `max_active`。
- 节点管理响应增加：
  - `id`
  - `multi_port_status`: `eager | inactive | starting | active | stopping | error | disabled`
  - `multi_port_pinned`
  - `multi_port_port`
  - `multi_port_last_access_at`
  - `multi_port_runtime_error`
- 设置页增加激活模式、最大 listener 数和空闲回收时间，并显示建议上限及实际端口范围。
- 节点管理页增加独立端口状态、激活/停用和置顶操作；显示当前活动数、上限和端口。运行中操作禁用重复点击。
- README、示例配置和 Docker 说明明确：
  - eager 建议不超过 200 个节点。
  - 大规模节点使用 hybrid + on-demand。
  - Docker 只需映射 `base_port` 开始的 `max_active` 个端口。
  - 冷节点不会因直接访问未监听端口而自动激活。

## 一致性与并发

- 每个节点使用独立生命周期锁，端口分配和 LRU 淘汰使用全局容量锁，避免并发激活获得同一端口。
- 先成功启动 nodeBox，再提交活动状态；失败时释放槽位并保持节点未激活。
- 回收时先确认活动连接为零，再关闭实例、释放端口并持久化状态。
- 应用关闭前刷新合并后的最近访问时间；访问时间最多每 5 秒批量写库，避免每次连接同步写 SQLite。
- 修改 `base_port` 或缩小 `max_active` 时重新映射活动节点；新上限不得小于置顶节点数量，超出的普通节点按 LRU 回收。
- 端口启动冲突时扫描当前活动槽范围内的其他空闲槽；全部不可用才返回错误，不静默扩展端口范围。

## 测试与验收

- 验证旧配置默认 eager，listener 数量和端口行为不变；超过 200 仅告警。
- 使用 1000 个节点验证 on-demand 基础实例不包含节点 listener，激活 10 个节点时仅存在 10 个独立 listener。
- 验证所有实际端口始终位于配置的有限槽范围内，LRU 淘汰后端口可复用。
- 验证置顶节点不会被空闲或容量淘汰，普通节点 30 分钟无访问且无连接后回收。
- 验证活动连接阻止自动回收；强制停用会断开该节点连接但不影响 pool、分组和其他 nodeBox。
- 验证 hybrid 冷节点继续通过 pool 路由；纯 multi-port 冷节点必须先激活。
- 验证激活、停用和置顶不调用全局 Reload；节点 CRUD 只重建受影响的活动实例。
- 验证并发激活、容量满、全为置顶、端口占用、启动失败和应用关闭期间状态一致。
- 验证重启恢复置顶及未过期节点，过期普通节点保持冷却。
- 运行 `go test ./...`、boxmgr/pool/store 生命周期并发与 race 测试、带生产 tags 的 Go 构建、前端 ESLint 和生产构建；不进行浏览器测试。

## 已确定默认

- on-demand 使用管理 API/UI 激活，不实现透明首包激活。
- 热点策略为手动置顶加独立端口访问 LRU，不按 pool 流量自动排名。
- 最大活动 listener 默认 200，空闲回收时间默认 30 分钟。
- 冷节点使用有限端口槽，普通节点重新激活时端口允许变化。
- 旧 multi-port/hybrid 配置继续采用 eager，升级后不会自动关闭现有端口。
