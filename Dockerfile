# 面板镜像。前端是纯 ES 模块，没有构建步骤，所以这里不需要 Node。
FROM golang:1.26-alpine AS build

WORKDIR /src

# 先只拷依赖描述，让 go mod download 这一层能被缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0：SQLite 用的是纯 Go 驱动，静态编译后能跑在 scratch 里。
# 顺手把四个架构的探针也编出来，面板会通过 /agent/download 分发给节点。
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/panel ./cmd/panel && \
    for arch in amd64 arm64 arm 386; do \
        CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" \
            -o /out/agents/vps-agent-linux-$arch ./cmd/agent; \
    done

FROM alpine:3.20

# ca-certificates：调用 Cloudflare / 腾讯云 / 阿里云 API 要校验证书
# tzdata：虽然二进制里已经内嵌了时区库，装上便于在容器里查时间
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 -h /data panel

COPY --from=build /out/panel /usr/local/bin/panel
COPY --from=build /out/agents /agents

# 探针二进制放到数据目录下，一键安装脚本从这里取
RUN mkdir -p /data/agents && cp /agents/* /data/agents/ && \
    chown -R panel:panel /data

USER panel
WORKDIR /data
VOLUME /data
EXPOSE 8080

ENV PANEL_LISTEN=0.0.0.0:8080 \
    PANEL_DATA_DIR=/data

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
    CMD wget -qO- http://127.0.0.1:8080/api/health || exit 1

ENTRYPOINT ["/usr/local/bin/panel"]
