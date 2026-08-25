# ── Stage 1: Build frontend assets ──────────────────────────────
FROM --platform=$BUILDPLATFORM node:22-slim AS frontend
WORKDIR /frontend
# 使用淘宝 npm 镜像
ARG NPM_REGISTRY=https://registry.npmmirror.com
RUN npm config set registry ${NPM_REGISTRY}
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ .
RUN npx vite build --outDir /frontend-dist

# ── Stage 2: Build Go binary ───────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
# 默认使用国内 Go 代理
ARG GOPROXY=https://goproxy.cn,https://goproxy.io,direct
RUN go env -w GOPROXY=${GOPROXY} && go mod download
COPY . .
# Copy frontend build output into the embed directory
COPY --from=frontend /frontend-dist/ ./internal/monitor/assets/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies

# ── Stage 3: Runtime ───────────────────────────────────────────
FROM debian:bookworm-slim AS runtime
# 使用阿里云 Debian 镜像源
RUN sed -i 's|deb.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates gosu \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 easy \
    && mkdir -p /etc/easy-proxies/data \
    && chown -R easy:easy /etc/easy-proxies
WORKDIR /app
COPY --from=builder /src/easy-proxies /usr/local/bin/easy-proxies
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
COPY --chown=easy:easy config.example.yaml /etc/easy-proxies/config.yaml
# Pool/Hybrid mode: 2323, Management: 9091, Multi-port/Hybrid mode: 24000-24200
EXPOSE 2323 9091 24000-24200
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["--config", "/etc/easy-proxies/config.yaml"]
