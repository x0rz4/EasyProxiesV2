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

usage() {
  cat <<'EOF'
Usage: bash install.sh [options]
  --method binary|docker  Deployment method (default: binary)
  --action ACTION        install (default), register, start, stop, restart,
                         status, enable, disable, unregister
  --version VERSION      Release version, e.g. 2.2.11 or v2.2.11 (default: latest)
  --dir PATH             Config/data directory (default: /opt/easy-proxies)
  --service-manager auto|systemd|openrc  Binary service manager (default: auto)
  --no-start             install: files only; register: register without starting
  --no-autostart         Disable boot autostart when installing/registering
  -h, --help             Show help

Linux amd64 and arm64 release binaries. Registration uses an existing local
binary/config, without downloads. Binary service changes require root and a
running systemd or OpenRC (with supervise-daemon). Status is read-only.
Default install/register enables boot autostart and starts/restarts the service.
Service actions other than install are binary-only. Docker must already be
accessible; containers use host networking and restart unless explicitly stopped.
Existing config.yaml and data are preserved, including when unregistering.
EOF
}

fail() { echo "Error: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "Required command: $1"; }
download() { curl --fail --silent --show-error --location --retry 3 "$1" --output "$2"; }

cleanup() {
  if [[ -n "$STAGED_SERVICE" ]]; then rm -f -- "$STAGED_SERVICE"; fi
  if [[ -n "$TMP_DIR" ]]; then rm -rf -- "$TMP_DIR"; fi
}

parse_args() {
  while (($#)); do
    case "$1" in
      --method|--action|--version|--dir|--service-manager)
        (($# >= 2)) || fail "Missing value for $1"
        case "$1" in
          --method) METHOD=$2 ;;
          --action) ACTION=$2 ;;
          --version) VERSION=$2 ;;
          --dir) INSTALL_DIR=$2 ;;
          --service-manager) SERVICE_MANAGER=$2 ;;
        esac
        shift 2 ;;
      --no-start) START=0; shift ;;
      --no-autostart) AUTOSTART=0; shift ;;
      -h|--help) usage; exit 0 ;;
      *) fail "Unknown option: $1" ;;
    esac
  done
  [[ "$METHOD" == binary || "$METHOD" == docker ]] || fail 'Method must be binary or docker'
  case "$ACTION" in
    install|register|start|stop|restart|status|enable|disable|unregister) ;;
    *) fail 'Unknown action; see --help' ;;
  esac
  case "$SERVICE_MANAGER" in
    auto|systemd|openrc) ;;
    *) fail 'Service manager must be auto, systemd or openrc' ;;
  esac
  [[ "$METHOD" == binary || "$ACTION" == install ]] || fail 'Service actions require --method binary'
  if [[ "$ACTION" != install && "$ACTION" != register ]]; then
    ((START && AUTOSTART)) || fail '--no-start/--no-autostart apply only to install or register'
  fi
}

prepare_directory() {
  # Both service managers interpret their configuration; disallow expansion,
  # whitespace and control characters before making any filesystem changes.
  [[ "$INSTALL_DIR" =~ ^/[a-zA-Z0-9_./-]+$ && "$INSTALL_DIR" != / ]] || fail 'Use an absolute directory without spaces or special characters'
  if [[ "$ACTION" == install ]]; then mkdir -p "$INSTALL_DIR"; fi
  [[ -d "$INSTALL_DIR" ]] || fail "Install directory not found: $INSTALL_DIR"
  INSTALL_DIR=$(cd "$INSTALL_DIR" && pwd -P)
  [[ "$INSTALL_DIR" =~ ^/[a-zA-Z0-9_./-]+$ && "$INSTALL_DIR" != / ]] || fail 'Invalid resolved install directory'
}

detect_service_manager() {
  if [[ "$ACTION" != status ]]; then
    [[ "$(id -u)" == 0 ]] || fail 'Run as root for service changes, or install with --no-start --dir PATH'
  fi
  if [[ "$SERVICE_MANAGER" == auto ]]; then
    if command -v systemctl >/dev/null 2>&1 && systemctl show-environment >/dev/null 2>&1; then
      SERVICE_MANAGER=systemd
    elif command -v rc-status >/dev/null 2>&1 && [[ -f "$OPENRC_STATE_FILE" ]] && rc-status --runlevel >/dev/null 2>&1; then
      SERVICE_MANAGER=openrc
    else
      fail 'No running systemd or OpenRC found; use --no-start for a files-only install'
    fi
  fi
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    need systemctl
    systemctl show-environment >/dev/null 2>&1 || fail 'systemd is not running'
    SERVICE_FILE=$SYSTEMD_DIR/$SERVICE_NAME.service
  else
    need rc-status; need rc-service; need rc-update
    [[ -f "$OPENRC_STATE_FILE" ]] || fail 'OpenRC did not boot this system'
    rc-status --runlevel >/dev/null 2>&1 || fail 'OpenRC is not running'
    if [[ "$ACTION" == install || "$ACTION" == register ]]; then need supervise-daemon; need logger; fi
    SERVICE_FILE=$OPENRC_DIR/$SERVICE_NAME
  fi
}

check_registration_owner() {
  [[ ! -e "$SERVICE_FILE" && ! -L "$SERVICE_FILE" ]] && return 0
  [[ -f "$SERVICE_FILE" && ! -L "$SERVICE_FILE" ]] || fail "Refusing to replace service symlink or special file: $SERVICE_FILE"
  if grep -Fxq "$MANAGED_MARKER" "$SERVICE_FILE"; then
    grep -Fxq "# Install directory: $INSTALL_DIR" "$SERVICE_FILE" || fail 'Service belongs to another install directory; use its --dir or unregister it first'
  elif [[ "$SERVICE_MANAGER" == systemd ]] &&
       grep -Fxq "ExecStart=$INSTALL_DIR/easy-proxies --config $INSTALL_DIR/config.yaml" "$SERVICE_FILE" &&
       grep -Fxq "WorkingDirectory=$INSTALL_DIR" "$SERVICE_FILE" &&
       grep -Fxq 'Description=Easy Proxies' "$SERVICE_FILE"; then
    # Migrate units created by the original installer without an owner marker.
    echo 'Upgrading legacy Easy Proxies service registration'
  else
    fail "Service is not managed by this installer: $SERVICE_FILE"
  fi
}

boot_enabled() {
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    systemctl is-enabled --quiet "$SERVICE_NAME"
  else
    local services
    services=$(rc-update show default) || fail 'Cannot read OpenRC default runlevel'
    grep -Eq "^[[:space:]]*$SERVICE_NAME[[:space:]]+\\|[[:space:]]+default([[:space:]]|$)" <<< "$services"
  fi
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
    boot_enabled || fail 'Service was registered but boot autostart was not enabled'
  elif boot_enabled; then
    fail 'Boot autostart is still enabled'
  fi
  echo "Boot autostart: $([[ $enabled == 1 ]] && echo enabled || echo disabled)"
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
    echo "Logs: journalctl -u $SERVICE_NAME -n 50 --no-pager" >&2
  else
    echo "Check system logs for the $SERVICE_NAME syslog tag" >&2
  fi
}

start_service() {
  local action=$1
  if ! service_command "$action"; then
    service_diagnostics
    fail "Service $action failed; configuration and data have been preserved"
  fi
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    if ! systemctl is-active --quiet "$SERVICE_NAME"; then
      service_diagnostics
      fail 'Service did not stay active; inspect its logs'
    fi
  elif ! service_command status; then
    service_diagnostics
    fail 'Service did not stay active; inspect its logs'
  fi
}

register_service() {
  [[ -x "$INSTALL_DIR/easy-proxies" ]] || fail "Executable not found: $INSTALL_DIR/easy-proxies"
  [[ -s "$INSTALL_DIR/config.yaml" ]] || fail "Nonempty config required: $INSTALL_DIR/config.yaml"
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
  echo "Registered $SERVICE_NAME ($SERVICE_MANAGER): $SERVICE_FILE"
}

manage_service() {
  case "$ACTION" in
    register) prepare_directory; register_service ;;
    start|restart) start_service "$ACTION" ;;
    stop) service_command stop ;;
    status)
      local code=0
      service_command status || code=$?
      if boot_enabled; then echo 'Boot autostart: enabled'; else echo 'Boot autostart: disabled'; fi
      return "$code" ;;
    enable) set_autostart 1 ;;
    disable) set_autostart 0 ;;
    unregister)
      prepare_directory
      if [[ ! -e "$SERVICE_FILE" && ! -L "$SERVICE_FILE" ]]; then
        echo 'Service is already unregistered'; return 0
      fi
      check_registration_owner
      if [[ "$SERVICE_MANAGER" == systemd ]]; then service_command stop;
      elif service_command status >/dev/null 2>&1; then service_command stop; fi
      set_autostart 0
      rm -f -- "$SERVICE_FILE"
      if [[ "$SERVICE_MANAGER" == systemd ]]; then systemctl daemon-reload; fi
      echo 'Service unregistered; binary, configuration and data preserved' ;;
  esac
}

install_release() {
  local arch tag image asset base_url release_url expected password
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fail 'Only amd64 and arm64 are supported' ;;
  esac
  if [[ "$METHOD" == binary && $START == 1 ]]; then
    detect_service_manager
  elif [[ "$METHOD" == docker ]]; then
    need docker
    docker info >/dev/null
    if ((START)) && docker container inspect "$CONTAINER" >/dev/null 2>&1; then
      local owner
      owner=$(docker inspect --format '{{ index .Config.Labels "io.easy-proxies.installer" }}' "$CONTAINER")
      [[ "$owner" == "$REPO" ]] || fail "Container $CONTAINER is not managed by this installer; rename or remove it first"
    fi
  fi
  prepare_directory
  if [[ "$METHOD" == binary && $START == 1 ]]; then check_registration_owner; fi
  need curl
  if [[ "$VERSION" == latest ]]; then
    release_url=$(curl --fail --silent --show-error --location --retry 3 \
      --output /dev/null --write-out '%{url_effective}' "https://github.com/$REPO/releases/latest")
    [[ "$release_url" == "https://github.com/$REPO/releases/tag/"* ]] || fail 'Cannot resolve latest release'
    VERSION=${release_url##*/}
  fi
  VERSION=${VERSION#v}
  [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail 'Use a release version such as 2.2.11'
  tag=v$VERSION
  image=ghcr.io/${REPO,,}:$VERSION
  TMP_DIR=$(mktemp -d)
  if [[ "$METHOD" == binary ]]; then
    need sha256sum
    asset=easy-proxies-linux-$arch
    base_url=https://github.com/$REPO/releases/download/$tag
    download "$base_url/$asset" "$TMP_DIR/$asset"
    download "$base_url/$asset.sha256" "$TMP_DIR/checksum"
    # Never follow paths from a remote checksum file.
    read -r expected _ < "$TMP_DIR/checksum"
    [[ "$expected" =~ ^[a-fA-F0-9]{64}$ ]] || fail 'Invalid SHA256 checksum'
    (cd "$TMP_DIR" && printf '%s  %s\n' "$expected" "$asset" | sha256sum -c -)
  else
    docker pull "$image"
  fi
  if [[ ! -e "$INSTALL_DIR/config.yaml" ]]; then
    download "https://raw.githubusercontent.com/$REPO/$tag/config.example.yaml" "$TMP_DIR/config.yaml"
    password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
    [[ ${#password} == 48 ]] || fail 'Cannot generate management password'
    sed "s/password: \"123456\"/password: \"$password\"/" "$TMP_DIR/config.yaml" > "$TMP_DIR/config.new"
    grep -q "password: \"$password\"" "$TMP_DIR/config.new" || fail 'Management password template not found'
    install -m 600 "$TMP_DIR/config.new" "$INSTALL_DIR/config.yaml"
    echo "Generated management password is stored in $INSTALL_DIR/config.yaml"
  fi
  mkdir -p "$INSTALL_DIR/data"
  touch "$INSTALL_DIR/nodes.txt"
  if [[ "$METHOD" == binary ]]; then
    # Rename avoids ETXTBSY when replacing a running executable.
    install -m 755 "$TMP_DIR/$asset" "$INSTALL_DIR/.easy-proxies.new"
    mv -f "$INSTALL_DIR/.easy-proxies.new" "$INSTALL_DIR/easy-proxies"
    if ((START)); then register_service; else echo "Start: cd $INSTALL_DIR && ./easy-proxies --config ./config.yaml"; fi
  else
    printf '%s\n' "$image" > "$INSTALL_DIR/docker-image"
    if ((START)); then
      if docker container inspect "$CONTAINER" >/dev/null 2>&1; then docker rm -f "$CONTAINER"; fi
      local restart_policy=no
      if ((AUTOSTART)); then restart_policy=unless-stopped; fi
      docker run -d --name "$CONTAINER" --restart "$restart_policy" --network host \
        --label "io.easy-proxies.installer=$REPO" \
        --mount "type=bind,src=$INSTALL_DIR/config.yaml,dst=/etc/easy-proxies/config.yaml" \
        --mount "type=bind,src=$INSTALL_DIR/nodes.txt,dst=/etc/easy-proxies/nodes.txt" \
        --mount "type=bind,src=$INSTALL_DIR/data,dst=/etc/easy-proxies/data" "$image"
    fi
  fi
  printf '%s\n' "$tag" > "$INSTALL_DIR/installed-version"
  echo "Installed $tag ($METHOD/$arch). Configuration: $INSTALL_DIR/config.yaml"
  echo 'Management URL: http://<server-ip>:9888 (unless changed in your configuration)'
}

main() {
  trap cleanup EXIT
  parse_args "$@"
  [[ "$(uname -s)" == Linux ]] || fail 'Only Linux is supported'
  if [[ "$ACTION" == install ]]; then install_release;
  else detect_service_manager; manage_service; fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
