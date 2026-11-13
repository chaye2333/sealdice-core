# 多阶段构建：编译老 UI（sealdice-ui）→ 导出 OpenAPI → 编译新 UI（仓库内 ui/）→ 编译核心 → 运行镜像
#
# 四条重要说明：
#
# 1) 镜像里同时包含两套界面（见 ui_router.go）：
#    · `/old-ui/`：老 UI，来自 fork 的 chaye2333/sealdice-ui，**默认入口**（`/` 302 到这里）；
#    · `/v2ui/`  ：新 UI，就是**仓库里的 ui/**（上游 #1689 起），编译到 static/v2ui/dist。
#    两套分别由 `go:embed frontend` 与 `go:embed all:v2ui` 打进二进制。
#    注意：仓库里的 static/frontend 只有一个占位 index.html（引用的 assets 不存在），
#    所以老 UI 必须在构建期现场编译，否则 `/` 打开是白页。
#
# 2) 新 UI 的 API 客户端由 ui/openapi.json 生成，而 openapi.json 只能由 Go 二进制导出，
#    所以顺序必须是：Go 导出 openapi.json → node 生成客户端并编译 UI → Go 再编译最终二进制。
#    （对应 Taskfile.yml 的 build-with-v2ui 任务）
#
# 3) 老 UI 源码钉在 LEGACY_UI_REF 这个 commit 上，改老 UI 后要同步更新它。
#
# 4) 核心用 CGO_ENABLED=0 编译，SQLite 走纯 Go 实现（见 migrate/db_util.go 的 !cgo 分支），
#    因此运行镜像不需要 glibc / GCC，镜像更小、跨平台更稳。
#
# 构建（在仓库根目录执行）：
#   docker build -t sealdice-core:local .

# ---------- 阶段 1：导出 openapi.json ----------
FROM golang:1.25-alpine AS openapi-builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# 先拉依赖，利用 Docker 层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . ./

# go:embed frontend 至少要能匹配到一个文件（.dockerignore 只放行占位页）。
# 这里再兜一层：任何忽略规则/检出差异都不该让 `go run .` 在镜像里直接失败。
RUN if [ ! -f static/frontend/index.html ]; then \
      mkdir -p static/frontend && \
      printf '%s\n' '<!doctype html><meta charset="utf-8"><title>sealdice-core</title>' \
        > static/frontend/index.html; \
    fi

# openapi.json 是新 UI 的契约输入，导不出来就没法编译前端
RUN go run . --gen-openapi=./ui/openapi.json && test -s ui/openapi.json

# ---------- 阶段 2：编译新 WebUI（仓库内 ui/） ----------
FROM node:22-alpine AS ui-builder

# 必须与 ui/package.json 的 packageManager 一致：preinstall 会校验 pnpm major
ARG PNPM_VERSION=10.14.0

# esbuild / rolldown 等原生依赖在 alpine 上需要这些工具链
RUN apk add --no-cache python3 make g++

WORKDIR /uisrc

RUN npm install -g "pnpm@${PNPM_VERSION}"

# 先只拷依赖清单，装完依赖再拷源码，尽量吃到层缓存
COPY --from=openapi-builder /src/ui/package.json ./package.json
COPY --from=openapi-builder /src/ui/pnpm-lock.yaml ./pnpm-lock.yaml
COPY --from=openapi-builder /src/ui/pnpm-workspace.yaml ./pnpm-workspace.yaml
COPY --from=openapi-builder /src/ui/scripts ./scripts
RUN pnpm install --frozen-lockfile

COPY --from=openapi-builder /src/ui/ ./

# 用后端导出的契约生成客户端，再以 embed 模式编译（产物写到 ../static/v2ui/dist）
RUN pnpm run generate-client && pnpm run build:embed:prepared && \
    test -f /static/v2ui/dist/index.html && \
    echo "=== 已编译管理界面（v2ui）===" && \
    ls /static/v2ui/dist

# ---------- 阶段 3：编译老 UI（sealdice-ui fork，供 /old-ui/ 使用） ----------
#
# 默认入口仍是老 UI（见 ui_router.go），但仓库里只有 static/frontend/index.html 这个
# 占位页，引用的 assets 并不存在 —— 所以镜像必须把老 UI 真正编译出来，
# 否则用户打开 `/` 就是一张白页。
#
# 源码取本 fork 配套的 chaye2333/sealdice-ui（含我们加过的设置项）。
#
# ⚠️ LEGACY_UI_REF 必须钉成具体 commit：这一层只依赖 ARG 的值，写成 master
# 会永远命中缓存，之后的老 UI 改动都进不了镜像而且构建照样成功（老 Dockerfile 踩过）。
FROM node:22-alpine AS legacy-ui-builder

ARG LEGACY_UI_REPO="chaye2333/sealdice-ui"
ARG LEGACY_UI_REF="84dbca34ba968bcd5736fe24c2e4c79881b452ff"
ARG LEGACY_PNPM_VERSION=10

RUN apk add --no-cache curl tar

WORKDIR /legacy

# 直接取 tarball，省掉完整 git 历史
RUN set -eux; \
    curl -fsSL "https://codeload.github.com/${LEGACY_UI_REPO}/tar.gz/${LEGACY_UI_REF}" -o ui.tar.gz; \
    tar -xzf ui.tar.gz --strip-components=1; \
    rm -f ui.tar.gz; \
    test -f package.json; \
    test -f pnpm-lock.yaml

# 不用 corepack：非交互环境下它会弹「是否下载 pnpm」的提示而直接失败
RUN npm install -g "pnpm@${LEGACY_PNPM_VERSION}"

RUN pnpm install --frozen-lockfile

RUN pnpm run build-only && \
    test -f dist/index.html && \
    test -d dist/assets && \
    echo "=== 已编译老 UI（sealdice-ui） ===" && \
    ls dist

# ---------- 阶段 4：编译 Go 核心 ----------
FROM golang:1.25-alpine AS core-builder

ARG VERSION_BUILD_METADATA="+dev"

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . ./

# 老 UI 产物放进 static/frontend（go:embed frontend 要求目录在编译时存在）
RUN rm -rf static/frontend && mkdir -p static/frontend
COPY --from=legacy-ui-builder /legacy/dist/ ./static/frontend/

# 校验：老 UI 必须是真正的构建产物
RUN test -f static/frontend/index.html && \
    test -d static/frontend/assets && \
    ! grep -qi "placeholder" static/frontend/index.html && \
    echo "=== 已打包老 UI（/old-ui/） ===" && \
    ls static/frontend

# 把新 UI 产物放进 embed 目录（go:embed all:v2ui 要求目录在编译时存在）
RUN rm -rf static/v2ui/dist && mkdir -p static/v2ui
COPY --from=ui-builder /static/v2ui/dist ./static/v2ui/dist

# 校验：必须是真正的构建产物，而不是占位页
RUN test -f static/v2ui/dist/index.html && \
    test -d static/v2ui/dist/assets && \
    ! grep -q "placeholder" static/v2ui/dist/index.html && \
    echo "=== 已打包管理界面 ===" && \
    ls static/v2ui/dist

ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
# VERSION_BUILD_METADATA 必须是合法 semver 元数据（带 + 前缀），否则 dice.VERSION 初始化会 panic。
# 不传时用构建日期兜底。
RUN BUILD_META="${VERSION_BUILD_METADATA:-+$(date -u +%Y%m%d)}" && \
    go build -trimpath \
      -ldflags "-s -w -X 'sealdice-core/dice.VERSION_BUILD_METADATA=${BUILD_META}'" \
      -o /out/sealdice-core . && \
    /out/sealdice-core --version

# ---------- 阶段 5：运行镜像 ----------
FROM alpine:3.20

LABEL org.opencontainers.image.source="https://github.com/sealdice/sealdice-core" \
      sealdice.ui.source="in-repo ui/ (v2ui)"

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 1000 sealdice && \
    adduser -u 1000 -G sealdice -s /bin/sh -D sealdice && \
    mkdir -p /app/data /app/extra && \
    chown -R sealdice:sealdice /app

WORKDIR /app

# ⚠️ 必须用 COPY --chown，**不要** COPY 之后再 `chown -R`。
#
# 踩过的坑：`COPY` 一个 73MB 的二进制、下一层再 `chown -R sealdice:sealdice /app`，
# chown 会把整个文件**复制进新层**，镜像里就存了两份二进制 ——
# 实测未压缩 155MB（其中两份 73MB），压缩后 59.4MB；改成 --chown 后
# 未压缩约 83MB、压缩约 31MB，少掉整整一个二进制的体积。
# 用户/目录也已经在上一步建好并 chown 过，这里不需要再动。
COPY --chown=sealdice:sealdice --from=core-builder /out/sealdice-core /app/sealdice-core

USER sealdice

EXPOSE 3211

# 容器模式下禁用内置客户端；UI 监听 0.0.0.0 以便映射端口
ENTRYPOINT ["/app/sealdice-core"]
CMD ["--container-mode", "--address", "0.0.0.0:3211"]
