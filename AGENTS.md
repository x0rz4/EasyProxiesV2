# Repository Guidelines —— 路由表

EasyProxiesV2：基于 [sing-box](https://github.com/SagerNet/sing-box)（**以库的形式内嵌，不是子进程**）的代理池与订阅管理工具。单一 Go 二进制承载数据面（监听 / 调度 / 拨号）、控制面（HTTP API + 内嵌 WebUI）与持久化（SQLite）。二开自 [jasonwong1991/easy_proxies](https://github.com/jasonwong1991/easy_proxies)。

> **本文件只路由，不写规则。** 工程规则的唯一体是 [docs/code-standards.md](docs/code-standards.md)；
> 事故教训与转正机制的唯一体是 [LESSONS.md](LESSONS.md)；系统当前形态的唯一体是 [docs/architecture.md](docs/architecture.md)。
> 三者与本文件冲突时，以后三者为准，并请顺手修正本文件。
> 本仓库的代码由 agent 编写、评审、合并、发布——开工先看 [LESSONS.md](LESSONS.md) 开篇。

---

## 任务 → 文档

| 你要做的事 | 先翻这里 | 再对照 |
| --- | --- | --- |
| **动手改任何代码前**，了解系统现在长什么样 | [docs/architecture.md](docs/architecture.md)（形态、包地图、真实依赖图、三条重载路径、数据流） | — |
| 改数据面：pool / group / boxmgr / monitor / probe / builder | [docs/subsystems/data-plane.md](docs/subsystems/data-plane.md) | [code-standards](docs/code-standards.md) §B/§C/§D；行为契约 [plane/specs/proxy-pool/spec.md](plane/specs/proxy-pool/spec.md) |
| 改控制面：httpapi / controlplane | [docs/subsystems/control-plane.md](docs/subsystems/control-plane.md) | [code-standards](docs/code-standards.md) §F |
| 改事实/地区/成员/标签：nodecodec / noderegion / nodeloc / groupmember / nodeselect / nodeview / nodefacts / nodetag / runtimenodes | [docs/subsystems/facts-and-membership.md](docs/subsystems/facts-and-membership.md) | [code-standards](docs/code-standards.md) §B/§G |
| 改检测：unlock / ipquality / nodedetect / noderesult | [docs/subsystems/detection.md](docs/subsystems/detection.md) | [code-standards](docs/code-standards.md) §G |
| 改订阅 / 配置解析 / subrender | [docs/subsystems/subscription.md](docs/subsystems/subscription.md) | [code-standards](docs/code-standards.md) §E |
| 改 store / migration / jsonx | [docs/subsystems/persistence.md](docs/subsystems/persistence.md) | [code-standards](docs/code-standards.md) §E |
| 改前端 | [docs/subsystems/frontend.md](docs/subsystems/frontend.md) | [code-standards](docs/code-standards.md) §A/§H/§I |
| **写代码 / 自审** | [docs/code-standards.md](docs/code-standards.md)（§A–§K 硬规则） | 收工前扫 [LESSONS.md](LESSONS.md) 相关条目；PR 自检用 [code-standards §L 审查清单](docs/code-standards.md) |
| **提 PR / 审社区 PR** | [docs/contributors.md](docs/contributors.md)（PR 必含内容、验证矩阵、直接打回清单、特别项） | [code-standards §L](docs/code-standards.md) 逐条过 |
| 构建 / 测试 / 发布 / 版本递增 | [docs/code-standards.md](docs/code-standards.md) §I + §H（命令与陷阱） | 发布纪律另见 [LESSONS L-012 / L-013](LESSONS.md) |
| 查"运行时契约/Requirement/规模提案/历史设计" | [plane/README.md](plane/README.md)（specs/ 当前契约、changes/ 提案、专题与历史记录入口） | 历史文档的旧限制不覆盖当前规范（[LESSONS L-018](LESSONS.md)） |
| **每晚复盘**（合并当天 commit 后） | [LESSONS.md](LESSONS.md) §机制（三类问题、Seen 3× 升级、Done right） | 执行 code-standards §K 的文档同步清单 |
| 碰到怪现象，怀疑以前踩过 | [LESSONS.md](LESSONS.md) 全文检索关键词 | — |

---

## 文档地图

```
CLAUDE.md                     入口：只指向本文件与 LESSONS.md
AGENTS.md                     ← 你在这里（路由表，无规则）
LESSONS.md                    错题本：复盘日志 + Seen 3× 转正机制 + Done right 正面例子
docs/
  architecture.md             当前形态唯一真源：技术栈、目录、包地图、真实依赖图、数据流
  code-standards.md           硬规则唯一体：§A 风格 §B 包边界 §C 并发 §D sing-box §E 数据库
                              §F 控制面 §G 检测 §H 测试 §I 构建发布 §J 安全 §K 文档同步 §L 审查清单
  contributors.md             PR 流程：必含内容、验证矩阵、打回清单、社区 PR 特别项
  subsystems/<层>.md          七个子系统地契：data-plane / control-plane / facts-and-membership /
                              detection / subscription / persistence / frontend
plane/
  README.md                   规格驱动入口：specs/（当前契约 MUST）、changes/（提案）、专题史
  specs/proxy-pool/spec.md    运行时行为契约（Requirement ID / Scenario）
  write-path-audit.md         写路径审计清单与评审检查
  changes/ · 专题 *.md        提案与历史记录——引用前先对照 docs/ 与代码
```

维护约定：`docs/` 只写"当前代码真实形态"；历史与提案留 `plane/`，同一约束只在一处定义（引用 ID/文件名，不各自发明语义）。前端构建产物 `internal/httpapi/assets/` 禁止手改。

---

## 历史编号对照

旧 AGENTS.md 内联条款已全部迁出，历史文档中的引用按此表落到新位置：

| 旧引用 | 现位置 |
| --- | --- |
| §1 技术栈 / §2 布局 / §3 Module Map / §4 数据流 / §5 命令 | docs/architecture.md（§1–§2、§3、§4、§5–§8、§11）与 code-standards §H/§I |
| §6 风格命名 / §7 测试 / §8 数据库与配置 / §9 版本规则 | code-standards §A/§B、§H、§E、§I |
| §10 护栏 1–15（按序） | §B+facts doc(1) · §D(2,3,4) · §B(5) · §C(6) · §G(7) · §C(8) · §G(9) · §I(10) · §B+detection doc(11) · §F(12,13) · §B(14) · §E(15) |
| 散落引用示例：`AGENTS.md §10.10` → code-standards §I（x/net 钉版）；`AGENTS.md §9` → code-standards §I（版本递增）；`AGENTS §8` → code-standards §E |

（本对照表本身是 [LESSONS L-017](LESSONS.md) 的产物——文档重构时，指向旧编号的引用必须同批可解析。）
