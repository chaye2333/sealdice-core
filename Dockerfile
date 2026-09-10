# 多阶段构建：编译 WebUI → 编译核心 → 精简运行镜像
#
# 构建（在仓库根目录执行）：
#   docker build -t sealdice-core:local .
#
# 说明：
#   * 核心使用 CGO_ENABLED=0 编译，SQLite 走纯 Go 实现（见 migrate/db_util.go 的 !cgo 分支），
#     因此运行镜像不需要 glibc / GCC，镜像更小、跨平台更稳。
#   * WebUI 由仓库内的 ui/ 目录编译，产物复制进 static/frontend 后再编译核心，
#     这样 UI 会被 embed 进最终二进制。

# ---------- 阶段 1：编译 WebUI ----------
FROM node:22-alpine AS ui-builder

WORKDIR /src/ui

RUN corepack enable

# 先装依赖，利用 Docker 层缓存
# pnpm-workspace.yaml 里有 allowBuilds 授权（esbuild / @tailwindcss/oxide 是原生模块），
# 少了它 pnpm install 会以 ERR_PNPM_IGNORED_BUILDS 失败。
COPY ui/package.json ui/pnpm-lock.yaml ui/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile

COPY ui/ ./
RUN pnpm run build-only

# ---------- 阶段 2：编译 Go 核心 ----------
FROM golang:1.25-alpine AS core-builder

ARG VERSION_BUILD_METADATA="+dev"

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# 先拉依赖，利用 Docker 层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . ./
# 用真实 WebUI 产物覆盖仓库里的占位页
RUN rm -rf static/frontend && mkdir -p static/frontend
COPY --from=ui-builder /src/ui/dist/ static/frontend/

ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
# VERSION_BUILD_METADATA 必须是合法 semver 元数据（带 + 前缀），否则 dice.VERSION 初始化会 panic。
# 不传时用构建日期兜底。
RUN BUILD_META="${VERSION_BUILD_METADATA:-+$(date -u +%Y%m%d)}" && \
    go build -trimpath \
      -ldflags "-s -w -X 'sealdice-core/dice.VERSION_BUILD_METADATA=${BUILD_META}'" \
      -o /out/sealdice-core . && \
    /out/sealdice-core --version

# ---------- 阶段 3：运行镜像 ----------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 1000 sealdice && \
    adduser -u 1000 -G sealdice -s /bin/sh -D sealdice

WORKDIR /app

COPY --from=core-builder /out/sealdice-core /app/sealdice-core

# 数据目录：数据库、serve.yaml、日志、extra 等都写在这里
RUN mkdir -p /app/data /app/extra && chown -R sealdice:sealdice /app

USER sealdice

EXPOSE 3211

# 容器模式下禁用内置客户端；UI 监听 0.0.0.0 以便映射端口
ENTRYPOINT ["/app/sealdice-core"]
CMD ["--container-mode", "--address", "0.0.0.0:3211"]
