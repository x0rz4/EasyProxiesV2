#!/usr/bin/env bash
set -euo pipefail

REPO=x0rz4/EasyProxiesV2
METHOD=binary
ACTION=install
VERSION=latest
INSTALL_DIR=/opt/easy-proxies
START=1
AUTOSTART=1
SERVICE_MANAGER=auto
SERVICE_NAME=easy-proxies
SYSTEMD_DIR=/etc/systemd/system
OPENRC_DIR=/etc/init.d
OPENRC_STATE_FILE=/run/openrc/softlevel
CONTAINER=easy-proxies
MANAGED_MARKER="# Managed by $REPO installer"
TMP_DIR=
STAGED_SERVICE=
WAS_RUNNING=0
DOCKER_RESTART_POLICY=unless-stopped

usage() {
  cat <<'EOF'
用法：bash install.sh [选项]
  --method binary|docker  安装方式（默认：binary 二进制）
  --action ACTION        install 安装（默认）、update 更新、register 注册服务、
                         start 启动、stop 停止、restart 重启、status 查看状态、
                         enable 开启自启、disable 关闭自启、unregister 注销服务
  --update               等同于 --action update
  --version VERSION      Release 版本，例如 2.2.14 或 v2.2.14（默认：latest）
  --dir PATH             安装及配置/数据目录（默认：/opt/easy-proxies）
  --service-manager auto|systemd|openrc  二进制服务管理器（默认：auto 自动检测）
  --no-start             安装：仅准备文件；更新：仅更新文件/镜像，不重启现有服务；
                         注册：仅注册服务，不立即启动
  --no-autostart         安装/注册时关闭开机自启；更新始终保留原来的自启设置
  -h, --help             显示中文帮助

默认安装到 /opt/easy-proxies，注册开机自启并启动服务。无需交互选择。
支持 Linux amd64/arm64；Docker 需要已安装且可访问的 Docker daemon。
更新需要已有安装；默认重启原本运行的服务/容器，原本停止的仍保持停止。
配置 config.yaml、节点文件 nodes.txt 与 data/ 数据目录会被保留。
register 和服务管理动作无需下载；服务更改需要 root 及运行中的 systemd/OpenRC。
仅更新文件可使用 --update --no-start，之后手动重启以加载新版本。
示例：sudo bash install.sh
      sudo bash install.sh --update
      sudo bash install.sh --update --version 2.2.14
      sudo bash install.sh --method docker --update
EOF
}

fail() { echo "错误：$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "缺少必要命令：$1"; }
download() {
  curl --fail --silent --show-error --location --retry 3 "$1" --output "$2" || fail '下载失败，请检查网络和 Release 文件是否存在'
}

cleanup() {
  if [[ -n "$STAGED_SERVICE" ]]; then rm -f -- "$STAGED_SERVICE"; fi
  if [[ -n "$TMP_DIR" ]]; then rm -rf -- "$TMP_DIR"; fi
}

parse_args() {
  while (($#)); do
    case "$1" in
      --method|--action|--version|--dir|--service-manager)
        (($# >= 2)) || fail "参数缺少值：$1"
        case "$1" in
          --method) METHOD=$2 ;;
          --action) ACTION=$2 ;;
          --version) VERSION=$2 ;;
          --dir) INSTALL_DIR=$2 ;;
          --service-manager) SERVICE_MANAGER=$2 ;;
        esac
        shift 2 ;;
      --update) ACTION=update; shift ;;
      --no-start) START=0; shift ;;
      --no-autostart) AUTOSTART=0; shift ;;
      -h|--help) usage; exit 0 ;;
      *) fail "未知参数：$1" ;;
    esac
  done
  [[ "$METHOD" == binary || "$METHOD" == docker ]] || fail '安装方式必须为 binary 或 docker'
  case "$ACTION" in
    install|update|register|start|stop|restart|status|enable|disable|unregister) ;;
    *) fail '未知操作，请使用 --help 查看帮助' ;;
  esac
  case "$SERVICE_MANAGER" in
    auto|systemd|openrc) ;;
    *) fail '服务管理器必须为 auto、systemd 或 openrc' ;;
  esac
  [[ "$METHOD" == binary || "$ACTION" == install || "$ACTION" == update ]] || fail '服务管理动作仅支持 --method binary'
  if [[ "$ACTION" == update ]]; then
    ((AUTOSTART)) || fail '更新会保留原来的自启设置，不能与 --no-autostart 同时使用'
  elif [[ "$ACTION" != install && "$ACTION" != register ]]; then
    ((START && AUTOSTART)) || fail '--no-start/--no-autostart 仅适用于安装或注册'
  fi
}

prepare_directory() {
  # Both service managers interpret their configuration; disallow expansion,
  # whitespace and control characters before making any filesystem changes.
  [[ "$INSTALL_DIR" =~ ^/[a-zA-Z0-9_./-]+$ && "$INSTALL_DIR" != / ]] || fail '请使用不含空格或特殊字符的绝对安装目录'
  if [[ "$ACTION" == install ]]; then mkdir -p "$INSTALL_DIR"; fi
  [[ -d "$INSTALL_DIR" ]] || fail "安装目录不存在：$INSTALL_DIR"
  INSTALL_DIR=$(cd "$INSTALL_DIR" && pwd -P)
  [[ "$INSTALL_DIR" =~ ^/[a-zA-Z0-9_./-]+$ && "$INSTALL_DIR" != / ]] || fail '解析后的安装目录无效'
}

detect_service_manager() {
  if [[ "$ACTION" != status ]]; then
    [[ "$(id -u)" == 0 ]] || fail '管理系统服务需要 root；仅准备文件请使用 --no-start --dir PATH'
  fi
  if [[ "$SERVICE_MANAGER" == auto ]]; then
    if command -v systemctl >/dev/null 2>&1 && systemctl show-environment >/dev/null 2>&1; then
      SERVICE_MANAGER=systemd
    elif command -v rc-status >/dev/null 2>&1 && [[ -f "$OPENRC_STATE_FILE" ]] && rc-status --runlevel >/dev/null 2>&1; then
      SERVICE_MANAGER=openrc
    else
      fail '未找到运行中的 systemd/OpenRC；仅准备文件请使用 --no-start'
    fi
  fi
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    need systemctl
    systemctl show-environment >/dev/null 2>&1 || fail 'systemd 未运行'
    SERVICE_FILE=$SYSTEMD_DIR/$SERVICE_NAME.service
  else
    need rc-status; need rc-service; need rc-update
    [[ -f "$OPENRC_STATE_FILE" ]] || fail '当前系统不是由 OpenRC 启动的'
    rc-status --runlevel >/dev/null 2>&1 || fail 'OpenRC 未运行'
    if [[ "$ACTION" == install || "$ACTION" == register ]]; then need supervise-daemon; need logger; fi
    SERVICE_FILE=$OPENRC_DIR/$SERVICE_NAME
  fi
}

check_registration_owner() {
  [[ ! -e "$SERVICE_FILE" && ! -L "$SERVICE_FILE" ]] && return 0
  [[ -f "$SERVICE_FILE" && ! -L "$SERVICE_FILE" ]] || fail "拒绝替换服务符号链接或特殊文件：$SERVICE_FILE"
  if grep -Fxq "$MANAGED_MARKER" "$SERVICE_FILE"; then
    grep -Fxq "# Install directory: $INSTALL_DIR" "$SERVICE_FILE" || fail '服务属于另一个安装目录，请使用对应 --dir 或先注销服务'
  elif [[ "$SERVICE_MANAGER" == systemd ]] &&
       grep -Fxq "ExecStart=$INSTALL_DIR/easy-proxies --config $INSTALL_DIR/config.yaml" "$SERVICE_FILE" &&
       grep -Fxq "WorkingDirectory=$INSTALL_DIR" "$SERVICE_FILE" &&
       grep -Fxq 'Description=Easy Proxies' "$SERVICE_FILE"; then
    # Migrate units created by the original installer without an owner marker.
    echo '正在升级旧版 Easy Proxies 服务注册'
  else
    fail "此服务不属于本安装器：$SERVICE_FILE"
  fi
}

boot_enabled() {
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    systemctl is-enabled --quiet "$SERVICE_NAME"
  else
    local services
    services=$(rc-update show default) || fail '无法读取 OpenRC 的 default 运行级别'
    grep -Eq "^[[:space:]]*$SERVICE_NAME[[:space:]]+\\|[[:space:]]+default([[:space:]]|$)" <<< "$services"
  fi
}

service_running() {
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    systemctl is-active --quiet "$SERVICE_NAME"
  else
    service_command status >/dev/null 2>&1
  fi
}

prepare_update() {
  [[ -s "$INSTALL_DIR/config.yaml" ]] || fail "更新需要已有非空配置：$INSTALL_DIR/config.yaml；首次安装请不加 --update"
  if [[ "$METHOD" == binary ]]; then
    [[ -x "$INSTALL_DIR/easy-proxies" ]] || fail "更新需要已有程序：$INSTALL_DIR/easy-proxies"
    if ((START)); then
      [[ -e "$SERVICE_FILE" || -L "$SERVICE_FILE" ]] || fail '更新未找到已注册服务；请先 register，或使用 --update --no-start 仅更新文件'
      check_registration_owner
      if service_running; then WAS_RUNNING=1; fi
    fi
  else
    docker container inspect "$CONTAINER" >/dev/null 2>&1 || fail '更新未找到已有容器，首次安装请不加 --update'
    local owner file source retries running
    owner=$(docker inspect --format '{{ index .Config.Labels "io.easy-proxies.installer" }}' "$CONTAINER")
    [[ "$owner" == "$REPO" ]] || fail "容器 $CONTAINER 不属于本安装器"
    for file in config.yaml nodes.txt data; do
      source=$(docker inspect --format "{{range .Mounts}}{{if eq .Destination \"/etc/easy-proxies/$file\"}}{{.Source}}{{end}}{{end}}" "$CONTAINER")
      [[ "$source" == "$INSTALL_DIR/$file" ]] || fail "容器挂载目录不匹配，请使用原安装目录 --dir：$file"
    done
    DOCKER_RESTART_POLICY=$(docker inspect --format '{{.HostConfig.RestartPolicy.Name}}' "$CONTAINER")
    case "$DOCKER_RESTART_POLICY" in
      no|always|unless-stopped) ;;
      on-failure)
        retries=$(docker inspect --format '{{.HostConfig.RestartPolicy.MaximumRetryCount}}' "$CONTAINER")
        [[ "$retries" =~ ^[0-9]+$ ]] || fail '无法读取容器重启次数限制'
        if ((retries > 0)); then DOCKER_RESTART_POLICY+=:$retries; fi ;;
      *) fail '无法识别已有容器的重启策略' ;;
    esac
    running=$(docker inspect --format '{{.State.Running}}' "$CONTAINER")
    case "$running" in
      true) WAS_RUNNING=1 ;;
      false) WAS_RUNNING=0 ;;
      *) fail '无法读取容器运行状态' ;;
    esac
  fi
  echo "正在更新已有安装：$INSTALL_DIR；保留配置、节点文件、数据库及自启设置"
}

set_autostart() {
  local enabled=$1
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    if ((enabled)); then systemctl enable "$SERVICE_NAME"; else systemctl disable "$SERVICE_NAME"; fi
  elif ((enabled)); then
    rc-update add "$SERVICE_NAME" default
  elif boot_enabled; then
    rc-update del "$SERVICE_NAME" default
  fi
  if ((enabled)); then
    boot_enabled || fail '服务已注册，但开机自启未能启用'
  elif boot_enabled; then
    fail '开机自启仍处于启用状态'
  fi
  echo "开机自启：$([[ $enabled == 1 ]] && echo 已启用 || echo 已关闭)"
}

service_command() {
  local action=$1
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    if [[ "$action" == status ]]; then systemctl --no-pager --full status "$SERVICE_NAME";
    else systemctl "$action" "$SERVICE_NAME"; fi
  else
    rc-service "$SERVICE_NAME" "$action"
  fi
}

service_diagnostics() {
  service_command status >&2 || true
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    echo "查看日志：journalctl -u $SERVICE_NAME -n 50 --no-pager" >&2
  else
    echo "请查看系统日志中的 $SERVICE_NAME 标签" >&2
  fi
}

start_service() {
  local action=$1
  if ! service_command "$action"; then
    service_diagnostics
    fail "服务操作 $action 失败；配置和数据已保留"
  fi
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    if ! systemctl is-active --quiet "$SERVICE_NAME"; then
      service_diagnostics
      fail '服务未保持运行，请检查日志'
    fi
  elif ! service_command status; then
    service_diagnostics
    fail '服务未保持运行，请检查日志'
  fi
}

register_service() {
  [[ -x "$INSTALL_DIR/easy-proxies" ]] || fail "未找到可执行程序：$INSTALL_DIR/easy-proxies"
  [[ -s "$INSTALL_DIR/config.yaml" ]] || fail "需要非空配置文件：$INSTALL_DIR/config.yaml"
  check_registration_owner
  local staged
  mkdir -p "$(dirname "$SERVICE_FILE")" "$INSTALL_DIR/data"
  staged=$(mktemp "$(dirname "$SERVICE_FILE")/.easy-proxies.XXXXXX")
  STAGED_SERVICE=$staged
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    cat > "$staged" <<EOF
$MANAGED_MARKER
# Install directory: $INSTALL_DIR
[Unit]
Description=Easy Proxies
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/easy-proxies --config $INSTALL_DIR/config.yaml
Restart=on-failure
RestartSec=5
TimeoutStopSec=45
LimitNOFILE=65536
UMask=0077

[Install]
WantedBy=multi-user.target
EOF
    chmod 644 "$staged"
  else
    cat > "$staged" <<EOF
#!/sbin/openrc-run
$MANAGED_MARKER
# Install directory: $INSTALL_DIR
description="Easy Proxies"
command="$INSTALL_DIR/easy-proxies"
command_args="--config $INSTALL_DIR/config.yaml"
directory="$INSTALL_DIR"
supervisor="supervise-daemon"
pidfile="/run/$SERVICE_NAME.pid"
respawn_delay=5
respawn_max=0
retry="TERM/45/KILL/5"
umask=0077
output_logger="logger -t $SERVICE_NAME"
error_logger="logger -t $SERVICE_NAME -p daemon.err"
required_files="$INSTALL_DIR/config.yaml"
depend() {
  need net
  use logger
}
EOF
    chmod 755 "$staged"
  fi
  mv -f "$staged" "$SERVICE_FILE"
  STAGED_SERVICE=
  if [[ "$SERVICE_MANAGER" == systemd ]]; then systemctl daemon-reload; fi
  set_autostart "$AUTOSTART"
  if ((START)); then start_service restart; fi
  echo "服务已注册：$SERVICE_NAME（$SERVICE_MANAGER），文件：$SERVICE_FILE"
}

manage_service() {
  case "$ACTION" in
    register) prepare_directory; register_service ;;
    start|restart) start_service "$ACTION" ;;
    stop) service_command stop ;;
    status)
      local code=0
      service_command status || code=$?
      if boot_enabled; then echo '开机自启：已启用'; else echo '开机自启：已关闭'; fi
      return "$code" ;;
    enable) set_autostart 1 ;;
    disable) set_autostart 0 ;;
    unregister)
      prepare_directory
      if [[ ! -e "$SERVICE_FILE" && ! -L "$SERVICE_FILE" ]]; then
        echo '服务已注销，无需重复操作'; return 0
      fi
      check_registration_owner
      if [[ "$SERVICE_MANAGER" == systemd ]]; then service_command stop;
      elif service_command status >/dev/null 2>&1; then service_command stop; fi
      set_autostart 0
      rm -f -- "$SERVICE_FILE"
      if [[ "$SERVICE_MANAGER" == systemd ]]; then systemctl daemon-reload; fi
      echo '服务已注销；程序、配置和数据均已保留' ;;
  esac
}

install_release() {
  local arch tag image asset base_url release_url expected password
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fail '仅支持 amd64 和 arm64 架构' ;;
  esac
  if [[ "$METHOD" == binary && $START == 1 ]]; then
    detect_service_manager
  elif [[ "$METHOD" == docker ]]; then
    need docker
    docker info >/dev/null
    if ((START)) && docker container inspect "$CONTAINER" >/dev/null 2>&1; then
      local owner
      owner=$(docker inspect --format '{{ index .Config.Labels "io.easy-proxies.installer" }}' "$CONTAINER")
      [[ "$owner" == "$REPO" ]] || fail "容器 $CONTAINER 不属于本安装器，请先重命名或移除冲突容器"
    fi
  fi
  prepare_directory
  if [[ "$ACTION" == update ]]; then prepare_update; fi
  if [[ "$METHOD" == binary && $START == 1 ]]; then check_registration_owner; fi
  need curl
  if [[ "$VERSION" == latest ]]; then
    release_url=$(curl --fail --silent --show-error --location --retry 3 \
      --output /dev/null --write-out '%{url_effective}' "https://github.com/$REPO/releases/latest") || fail '获取最新版本失败，请检查网络或使用 --version 指定版本'
    [[ "$release_url" == "https://github.com/$REPO/releases/tag/"* ]] || fail '无法解析最新 Release 版本'
    VERSION=${release_url##*/}
  fi
  VERSION=${VERSION#v}
  [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail '请使用有效的 Release 版本，例如 2.2.14'
  tag=v$VERSION
  image=ghcr.io/${REPO,,}:$VERSION
  TMP_DIR=$(mktemp -d)
  echo "目标版本：$tag；安装方式：$METHOD/$arch；目录：$INSTALL_DIR"
  if [[ "$METHOD" == binary ]]; then
    need sha256sum
    asset=easy-proxies-linux-$arch
    base_url=https://github.com/$REPO/releases/download/$tag
    download "$base_url/$asset" "$TMP_DIR/$asset"
    download "$base_url/$asset.sha256" "$TMP_DIR/checksum"
    # Never follow paths from a remote checksum file.
    read -r expected _ < "$TMP_DIR/checksum"
    [[ "$expected" =~ ^[a-fA-F0-9]{64}$ ]] || fail 'SHA256 校验文件格式无效'
    (cd "$TMP_DIR" && printf '%s  %s\n' "$expected" "$asset" | sha256sum -c -) || fail 'SHA256 校验失败，已有程序未替换'
  else
    docker pull "$image" || fail '镜像拉取失败，已有容器未替换'
  fi
  if [[ ! -e "$INSTALL_DIR/config.yaml" ]]; then
    download "https://raw.githubusercontent.com/$REPO/$tag/config.example.yaml" "$TMP_DIR/config.yaml"
    password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
    [[ ${#password} == 48 ]] || fail '无法生成管理密码'
    sed "s/password: \"123456\"/password: \"$password\"/" "$TMP_DIR/config.yaml" > "$TMP_DIR/config.new"
    grep -q "password: \"$password\"" "$TMP_DIR/config.new" || fail '配置模板中未找到管理密码字段'
    install -m 600 "$TMP_DIR/config.new" "$INSTALL_DIR/config.yaml"
    echo "已生成随机管理密码，保存在 $INSTALL_DIR/config.yaml"
  fi
  mkdir -p "$INSTALL_DIR/data"
  touch "$INSTALL_DIR/nodes.txt"
  if [[ "$METHOD" == binary ]]; then
    # Rename avoids ETXTBSY when replacing a running executable.
    install -m 755 "$TMP_DIR/$asset" "$INSTALL_DIR/.easy-proxies.new"
    mv -f "$INSTALL_DIR/.easy-proxies.new" "$INSTALL_DIR/easy-proxies"
    if [[ "$ACTION" == update ]]; then
      if ((START && WAS_RUNNING)); then start_service restart;
      elif ((START)); then echo '文件已更新，服务保持停止；自启设置未改变';
      else echo '文件已更新，未操作现有服务；手动重启后加载新版本'; fi
    elif ((START)); then register_service;
    else echo "手动启动：cd $INSTALL_DIR && ./easy-proxies --config ./config.yaml"; fi
  else
    printf '%s\n' "$image" > "$INSTALL_DIR/docker-image"
    if ((START)); then
      if docker container inspect "$CONTAINER" >/dev/null 2>&1; then docker rm -f "$CONTAINER"; fi
      local restart_policy=no
      local -a container_command=(run -d)
      if [[ "$ACTION" == update ]]; then
        restart_policy=$DOCKER_RESTART_POLICY
        container_command=(create)
      elif ((AUTOSTART)); then restart_policy=unless-stopped; fi
      docker "${container_command[@]}" --name "$CONTAINER" --restart "$restart_policy" --network host \
        --label "io.easy-proxies.installer=$REPO" \
        --mount "type=bind,src=$INSTALL_DIR/config.yaml,dst=/etc/easy-proxies/config.yaml" \
        --mount "type=bind,src=$INSTALL_DIR/nodes.txt,dst=/etc/easy-proxies/nodes.txt" \
        --mount "type=bind,src=$INSTALL_DIR/data,dst=/etc/easy-proxies/data" "$image" || fail '容器创建失败；配置和数据仍保存在安装目录'
      if [[ "$ACTION" == update ]]; then
        if ((WAS_RUNNING)); then
          docker start "$CONTAINER" || fail '更新后的容器启动失败，请检查 docker logs easy-proxies'
          [[ "$(docker inspect --format '{{.State.Running}}' "$CONTAINER")" == true ]] || fail '更新后的容器未保持运行，请检查 docker logs easy-proxies'
        else echo '镜像已更新，容器保持停止；重启策略未改变'; fi
      fi
    elif [[ "$ACTION" == update ]]; then
      echo '镜像已拉取，未替换现有容器；重新执行不带 --no-start 的更新以生效'
    fi
  fi
  printf '%s\n' "$tag" > "$INSTALL_DIR/installed-version"
  if [[ "$ACTION" == update ]]; then
    echo "更新完成：$tag（$METHOD/$arch），配置：$INSTALL_DIR/config.yaml"
  else echo "安装完成：$tag（$METHOD/$arch），配置：$INSTALL_DIR/config.yaml"; fi
  echo '管理面板：http://<服务器IP>:9888（以配置文件中的监听设置为准）'
}

main() {
  trap cleanup EXIT
  parse_args "$@"
  [[ "$(uname -s)" == Linux ]] || fail '仅支持 Linux 系统'
  if [[ "$ACTION" == install || "$ACTION" == update ]]; then install_release;
  else detect_service_manager; manage_service; fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
