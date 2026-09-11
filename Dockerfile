# 多阶段构建：编译 WebUI（sealdice-ui）→ 编译核心 → 精简运行镜像
#
# 构建（在仓库根目录执行）：
#   docker build -t sealdice-core:local .
#
# 三条重要说明：
#
# 1) WebUI 不能拿仓库里的 ui/ 目录来构建！
#    那个 ui/ 是海豹仓库自带的一个 Vue 脚手架样例页（HelloWorld/TheWelcome/counter），
#    编译出来是 "You've successfully created a project with Vite + Vue 3" 欢迎页，
#    不是海豹的管理界面。
#    真正的管理界面由 sealdice-ui 仓库产出，上游约定是放在 static/frontend/，
#    再被 static/static.go 的 go:embed 打进二进制。
#
# 2) 默认从「本仓库配套的 sealdice-ui 分支」拉源码现场编译（UI_FROM_SOURCE=1）。
#    这样镜像里的管理界面就带有我们一起加上的设置项（官方QQ超时/分片上传、身份绑定）。
#    上游的官方做法是 `go generate ./static/...` 从 sealdice-ui 的 pre-release 下载
#    zip 产物（见 static/gen/download-fe.go），但它打包的是上游 UI，
#    没有我们新增的选项；需要那个版本时把 UI_FROM_SOURCE 设为 0 即可。
#    无论走哪条路，构建期都会校验 static/frontend/index.html 存在，
#    不会静默塞一个错误的前端进去。
#
# 3) 核心用 CGO_ENABLED=0 编译，SQLite 走纯 Go 实现（见 migrate/db_util.go 的 !cgo 分支），
#    因此运行镜像不需要 glibc / GCC，镜像更小、跨平台更稳。

# ---------- 阶段 1：编译 WebUI（sealdice-ui） ----------
FROM node:22-alpine AS ui-builder

# UI_REPO / UI_REF 决定用哪份前端源码。
#
# ⚠️ UI_REF 必须钉成**具体的 commit hash**，不能用 "master" 这种分支名！
#
# 原因（真实踩过）：Docker 按 ARG 的值做层缓存，而 `curl` 那一层只依赖 UI_REF 的值。
# 写成 "master" 时这个值永远不变 → 该层永远命中缓存 →
# **之后推到 fork 的所有 UI 改动都不会进镜像**，而且构建照样"成功"，
# 极难发现（镜像里的其实是第一次构建时的旧 UI）。
#
# 改 UI 之后必须同步更新这里的 hash（见下面的「改 UI 流程」）。
# CI 里还有一步烟雾测试会校验新 UI 的关键选项确实在镜像里，漏改会被拦下。
ARG UI_REPO="chaye2333/sealdice-ui"
ARG UI_REF="bd0a9b21a2f0d66c9c0309aff8bb2a530e20475f"
# 前端包管理器版本。仓库里的 pnpm-workspace.yaml 用 allowBuilds 控制哪些依赖允许跑
# postinstall 脚本（esbuild / @tailwindcss/oxide 必须为 true，否则 vite 无法运行），
# 这是 pnpm 10 的写法，所以固定用 pnpm 10。
ARG PNPM_VERSION=10

RUN apk add --no-cache curl tar

WORKDIR /uisrc

# 直接取 tarball，省掉完整 git 历史，构建更快
RUN set -eux; \
    curl -fsSL "https://codeload.github.com/${UI_REPO}/tar.gz/${UI_REF}" -o ui.tar.gz; \
    tar -xzf ui.tar.gz --strip-components=1; \
    rm -f ui.tar.gz; \
    test -f package.json; \
    test -f pnpm-lock.yaml

# 不用 corepack：非交互环境下它会弹「是否下载 pnpm」的提示而直接失败，直接用 npm 装指定版本更稳
RUN npm install -g "pnpm@${PNPM_VERSION}"

RUN pnpm install --frozen-lockfile

# 只编译前端产物，不做 eslint 等额外检查（build-only = type-check + vite build）
RUN pnpm run build-only && \
    test -f dist/index.html && \
    test -d dist/assets && \
    echo "=== 已编译管理界面 ===" && \
    ls dist

# ---------- 阶段 2：编译 Go 核心 ----------
FROM golang:1.25-alpine AS core-builder

ARG VERSION_BUILD_METADATA="+dev"
# 1 = 使用阶段 1 现场编译的 UI；0 = 用上游 pre-release zip
ARG UI_FROM_SOURCE=1

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# 先拉依赖，利用 Docker 层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . ./

# 准备 static/frontend（go:embed 要求该目录在编译时必须存在）
RUN mkdir -p static/frontend

# 路线 A：用我们自己编译的前端产物
COPY --from=ui-builder /uisrc/dist/ ./static/frontend/

# 路线 B：拉取上游 sealdice-ui pre-release 产物并解压到 static/frontend
# 注意：go generate 的工作目录是 static/ 包目录，所以它写入的是 static/frontend
RUN if [ "$UI_FROM_SOURCE" = "0" ]; then \
      rm -rf static/frontend && \
      go generate ./static/... ; \
    fi

# 统一校验：不管走哪条路，前端都必须是真正的构建产物
RUN test -f static/frontend/index.html && \
    test -d static/frontend/assets && \
    echo "=== 已打包管理界面 ===" && \
    ls static/frontend

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

# 记录前端来源，方便上线后核对镜像里到底是哪份 UI。
# 注意这里的默认值必须和阶段 1 保持一致，否则 label 会误导排查。
ARG UI_REPO="chaye2333/sealdice-ui"
ARG UI_REF="bd0a9b21a2f0d66c9c0309aff8bb2a530e20475f"
LABEL org.opencontainers.image.source="https://github.com/${UI_REPO}" \
      sealdice.ui.repo="${UI_REPO}" \
      sealdice.ui.ref="${UI_REF}"

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
