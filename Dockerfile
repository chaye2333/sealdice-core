# 多阶段构建：编译核心（内含真正的 WebUI）→ 精简运行镜像
#
# 构建（在仓库根目录执行）：
#   docker build -t sealdice-core:local .
#
# 两条重要说明：
#
# 1) WebUI 不能拿仓库里的 ui/ 目录来构建！
#    那个 ui/ 是海豹仓库自带的一个 Vue 脚手架样例页（HelloWorld/TheWelcome/counter），
#    编译出来是 "You've successfully created a project with Vite + Vue 3" 欢迎页，
#    不是海豹的管理界面。
#    真正的管理界面由 sealdice-ui 仓库产出，上游约定是放在 static/frontend/，
#    再被 static/static.go 的 go:embed 打进二进制。
#    README 明确写了用 `go generate ./...` 从官方 release 拉取前端产物
#    （见 static/gen/download-fe.go），这里沿用同一套机制。
#    如果下载失败，本 Dockerfile 会在构建期直接报错，不会静默塞一个错误的前端进去。
#
# 2) 核心用 CGO_ENABLED=0 编译，SQLite 走纯 Go 实现（见 migrate/db_util.go 的 !cgo 分支），
#    因此运行镜像不需要 glibc / GCC，镜像更小、跨平台更稳。

# ---------- 阶段 1：编译 Go 核心 ----------
FROM golang:1.25-alpine AS core-builder

ARG VERSION_BUILD_METADATA="+dev"

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# 先拉依赖，利用 Docker 层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . ./

# 拉取官方 sealdice-ui 产物并解压到 static/frontend
# 产物结构：static/frontend/{index.html,favicon.svg,assets/...}
RUN go generate ./static/... && \
    test -f static/frontend/index.html && \
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

# ---------- 阶段 2：运行镜像 ----------
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
