# Linux 安装与系统服务

当前入口为 `install.sh`，安装器支持 Linux amd64 / arm64 Release 二进制及 Docker。二进制服务名固定为 `easy-proxies`，默认目录 `/opt/easy-proxies`。配置、SQLite 与节点文件沿用原目录；重复安装保留它们。

## 安装与补注册

```bash
sudo bash install.sh --method binary
# 只下载/校验二进制和准备配置，不操作系统服务
bash install.sh --dir "$HOME/easy-proxies" --no-start
# 对已有二进制和配置补注册，不下载 Release 或访问网络
sudo bash install.sh --action register --dir /opt/easy-proxies
```

默认安装和补注册会启用开机自启，然后启动/重启服务。`register` 要求目录里已有可执行的 `easy-proxies` 和非空 `config.yaml`。路径使用不含空格或特殊字符的绝对路径。

注册时写入安装器归属标记及安装目录。来自旧版安装器、命令和工作目录一致的 systemd 单元可直接升级；其它同名服务、服务文件符号链接或其它目录的注册会被拒绝。迁移安装目录时，先用原目录 `--action unregister --dir OLD`，再在新目录注册。

## 管理命令

| 参数 | 行为 |
| --- | --- |
| `--action start/stop/restart` | 启动、停止或重启当前系统服务 |
| `--action status` | 显示运行状态和自启状态；未运行时返回非零；不下载或创建安装目录 |
| `--action enable/disable` | 开启/关闭开机自启；不改变当前运行状态 |
| `--action unregister --dir PATH` | 停止并禁用自启，删除本安装器注册的服务文件；保留程序、配置和数据 |
| `--action register --no-start` | 注册服务和自启，不立即启动 |
| `--no-autostart` | 安装/注册时关闭自启；仍默认立即启动，可与 `--no-start` 组合 |
| `--service-manager auto/systemd/openrc` | 默认探测正在运行的服务管理器，也可显式指定 |

服务更改需要 root；只读状态查询可由普通用户执行。`--action` 的服务管理命令只适用于二进制模式。`--no-start` 在默认 `install` 下保持原有的“仅文件安装”语义，不会安装服务或改变现有服务的启用状态。

## systemd

注册文件 `/etc/systemd/system/easy-proxies.service`。固定配置路径和工作目录；等待网络目标，失败后五秒重启；停止期限 45 秒，给应用内部三十秒清理预算留出余量。服务的文件创建掩码为 0077，文件描述符上限为 65536。

安装器按 daemon-reload → enable/disable → restart → is-active 执行；启用后再检查自启状态。进程活动检查不等同于所有节点健康或管理 API 已就绪。

```bash
systemctl status easy-proxies --no-pager
systemctl is-enabled easy-proxies
journalctl -u easy-proxies -n 50 --no-pager
```

## OpenRC

注册文件 `/etc/init.d/easy-proxies`，加入 `default` runlevel。要求正在运行的 OpenRC（存在 `/run/openrc/softlevel`）、`rc-status`、`rc-service`、`rc-update`、`supervise-daemon` 和 `logger`；安装器本身使用 Bash。Alpine 若缺少 Bash/curl，先安装这些依赖；容器中仅安装了 OpenRC 包并不等于 OpenRC 已启动。

通过 `supervise-daemon` 管理前台进程，退出后五秒重启；设置 SIGTERM 的 45 秒停止等待及后续强制停止。标准输出和错误交给系统日志，标签为 `easy-proxies`，日志保留由系统日志服务管理。

```bash
rc-service easy-proxies status
rc-update show default
# Alpine 且启用了对应系统日志服务时
logread -e easy-proxies
```

模板参照 [systemd 服务文档](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html) 和 [OpenRC supervisor 指南](https://github.com/OpenRC/openrc/blob/master/supervise-daemon-guide.md)、[openrc-run 参数](https://github.com/OpenRC/openrc/blob/master/man/openrc-run.8)。

## Docker 与失败处理

Docker 保持带归属标签的容器替换、host 网络和持久化挂载。默认 `--restart unless-stopped`，依赖 Docker daemon 在开机后运行；人为停止的容器不会自动恢复。`--no-autostart` 使用 `--restart no`；`--no-start` 只拉镜像和准备文件。安装器不修改 Docker daemon 的系统服务设置。

SHA256 不匹配或镜像拉取失败，不替换旧程序/容器。服务启用或启动检查失败时返回非零，不写新的 `installed-version` 成功记录，并提示服务状态和日志位置。此时注册文件或已校验的新二进制可能已经写入，需排查后重试；不承诺自动回滚数据库或二进制。

安装器测试见 `tests/test_install.py`：网络和系统管理命令均为模拟，服务文件写入临时目录。Linux 测试主机若有 `systemd-analyze`，额外校验生成的单元；不包含真实重启服务器的集成验收。
