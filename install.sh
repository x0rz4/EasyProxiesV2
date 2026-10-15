#!/usr/bin/env bash
set -euo pipefail

REPO=x0rz4/EasyProxiesV2
METHOD=binary
VERSION=latest
INSTALL_DIR=/opt/easy-proxies
START=1
CONTAINER=easy-proxies

usage() {
  cat <<'EOF'
Usage: bash install.sh [options]
  --method binary|docker  Deployment method (default: binary)
  --version VERSION       Release version, e.g. 2.1.0 or v2.1.0 (default: latest)
  --dir PATH              Config/data directory (default: /opt/easy-proxies)
  --no-start              Prepare files / pull image without starting a service
  -h, --help              Show help

Linux amd64 and arm64 only. Binary service installation requires root and systemd.
Docker must already be installed and accessible; containers use host networking.
Existing config.yaml and data are preserved. Back up data before downgrading.
EOF
}
fail() { echo "Error: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "Required command: $1"; }
while (($#)); do
  case "$1" in
    --method|--version|--dir)
      (($# >= 2)) || fail "Missing value for $1"
      case "$1" in
        --method) METHOD=$2 ;;
        --version) VERSION=$2 ;;
        --dir) INSTALL_DIR=$2 ;;
      esac
      shift 2 ;;
    --no-start) START=0; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "Unknown option: $1" ;;
  esac
done
[[ "$METHOD" == binary || "$METHOD" == docker ]] || fail 'Method must be binary or docker'
[[ "$(uname -s)" == Linux ]] || fail 'Only Linux is supported'
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) fail 'Only amd64 and arm64 are supported' ;;
esac
need curl
if [[ "$VERSION" == latest ]]; then
  RELEASE_URL=$(curl --fail --silent --show-error --location --retry 3 \
    --output /dev/null --write-out '%{url_effective}' "https://github.com/$REPO/releases/latest")
  [[ "$RELEASE_URL" == "https://github.com/$REPO/releases/tag/"* ]] || fail 'Cannot resolve latest release'
  VERSION=${RELEASE_URL##*/}
fi
VERSION=${VERSION#v}
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail 'Use a release version such as 2.1.0'
TAG=v$VERSION
IMAGE=ghcr.io/${REPO,,}:$VERSION

if [[ "$METHOD" == binary ]]; then
  need sha256sum
  if ((START)); then
    [[ $EUID == 0 ]] || fail 'Run as root for systemd, or use --no-start --dir PATH'
    need systemctl
    [[ -d /run/systemd/system ]] || fail 'systemd is not running; use --no-start'
  fi
else
  need docker
  docker info >/dev/null
  if ((START)) && docker container inspect "$CONTAINER" >/dev/null 2>&1; then
    OWNER=$(docker inspect --format '{{ index .Config.Labels "io.easy-proxies.installer" }}' "$CONTAINER")
    [[ "$OWNER" == "$REPO" ]] || fail "Container $CONTAINER is not managed by this installer; rename or remove it first"
  fi
fi

mkdir -p "$INSTALL_DIR"
INSTALL_DIR=$(cd "$INSTALL_DIR" && pwd -P)
# Paths are embedded in systemd units and Docker bind mounts.
[[ "$INSTALL_DIR" =~ ^/[a-zA-Z0-9_./-]+$ && "$INSTALL_DIR" != / ]] || fail 'Use an absolute directory without spaces or special characters'
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT
download() { curl --fail --silent --show-error --location --retry 3 "$1" --output "$2"; }
if [[ "$METHOD" == binary ]]; then
  ASSET=easy-proxies-linux-$ARCH
  BASE_URL=https://github.com/$REPO/releases/download/$TAG
  download "$BASE_URL/$ASSET" "$TMP_DIR/$ASSET"
  download "$BASE_URL/$ASSET.sha256" "$TMP_DIR/checksum"
  # Verify only the expected binary; do not follow paths from a remote checksum file.
  read -r EXPECTED _ < "$TMP_DIR/checksum"
  [[ "$EXPECTED" =~ ^[a-fA-F0-9]{64}$ ]] || fail 'Invalid SHA256 checksum'
  (cd "$TMP_DIR" && printf '%s  %s\n' "$EXPECTED" "$ASSET" | sha256sum -c -)
else
  docker pull "$IMAGE"
fi

if [[ ! -e "$INSTALL_DIR/config.yaml" ]]; then
  download "https://raw.githubusercontent.com/$REPO/$TAG/config.example.yaml" "$TMP_DIR/config.yaml"
  PASSWORD=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
  [[ ${#PASSWORD} == 48 ]] || fail 'Cannot generate management password'
  sed "s/password: \"123456\"/password: \"$PASSWORD\"/" "$TMP_DIR/config.yaml" > "$TMP_DIR/config.new"
  grep -q "password: \"$PASSWORD\"" "$TMP_DIR/config.new" || fail 'Management password template not found'
  install -m 600 "$TMP_DIR/config.new" "$INSTALL_DIR/config.yaml"
  echo "Generated management password is stored in $INSTALL_DIR/config.yaml"
fi
mkdir -p "$INSTALL_DIR/data"
touch "$INSTALL_DIR/nodes.txt"

if [[ "$METHOD" == binary ]]; then
  # Replace via rename so updating a running executable does not fail with ETXTBSY.
  install -m 755 "$TMP_DIR/$ASSET" "$INSTALL_DIR/.easy-proxies.new"
  mv -f "$INSTALL_DIR/.easy-proxies.new" "$INSTALL_DIR/easy-proxies"
  if ((START)); then
    cat > /etc/systemd/system/easy-proxies.service <<EOF
[Unit]
Description=Easy Proxies
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/easy-proxies --config $INSTALL_DIR/config.yaml
Restart=on-failure
RestartSec=5
UMask=0077

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable easy-proxies
    systemctl restart easy-proxies
    systemctl is-active --quiet easy-proxies
  else
    echo "Start: cd $INSTALL_DIR && ./easy-proxies --config ./config.yaml"
  fi
else
  printf '%s\n' "$IMAGE" > "$INSTALL_DIR/docker-image"
  if ((START)); then
    if docker container inspect "$CONTAINER" >/dev/null 2>&1; then
      docker rm -f "$CONTAINER"
    fi
    docker run -d --name "$CONTAINER" --restart unless-stopped --network host \
      --label "io.easy-proxies.installer=$REPO" \
      --mount "type=bind,src=$INSTALL_DIR/config.yaml,dst=/etc/easy-proxies/config.yaml" \
      --mount "type=bind,src=$INSTALL_DIR/nodes.txt,dst=/etc/easy-proxies/nodes.txt" \
      --mount "type=bind,src=$INSTALL_DIR/data,dst=/etc/easy-proxies/data" "$IMAGE"
  fi
fi
printf '%s\n' "$TAG" > "$INSTALL_DIR/installed-version"
echo "Installed $TAG ($METHOD/$ARCH). Configuration: $INSTALL_DIR/config.yaml"
echo 'Management URL: http://<server-ip>:9888 (unless changed in your configuration)'
