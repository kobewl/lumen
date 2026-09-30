# lumen-server 镜像
#
# 安全要点：
#   - 多阶段构建，最终镜像只含编译好的二进制和一个空配置目录；
#   - 绝不把 .env、密钥或代码复制进镜像；
#   - 以非 root 用户运行。
# 构建镜像的 Go 版本必须 >= server/go.mod 中声明的版本，否则 go mod download 会失败。
FROM golang:1.25-alpine AS builder

WORKDIR /src

# 先复制依赖清单，利用层缓存加速重复构建。
COPY server/go.mod server/go.sum ./
RUN go mod download

COPY server/ ./
# CGO_ENABLED=0 让二进制静态链接，可以直接跑在 scratch/alpine 上。
# 使用 modernc.org/sqlite（纯 Go 实现），因此不需要 C 编译器。
#
# VERSION 由构建命令传入（--build-arg VERSION=0.1.8），注入后 healthz 报告的
# 版本就与镜像标签一致，部署后能从接口确认"线上跑的是哪一版"。
# 忘记传不会构建失败，只是显示 dev。
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X lumen/server/internal/api.Version=${VERSION}" \
    -o /out/lumen-server ./cmd/lumen-server

FROM alpine:3.20

# tzdata 用于正确解析 LUMEN_TIMEZONE；ca-certificates 用于 HTTPS 调用 DeepSeek。
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 -h /app lumen \
    && mkdir -p /data /app \
    && chown -R lumen:lumen /data /app

COPY --from=builder /out/lumen-server /usr/local/bin/lumen-server

USER lumen
WORKDIR /app

# 数据目录以卷方式挂载，镜像内不保存任何业务数据。
VOLUME ["/data"]

ENV LUMEN_DB_PATH=/data/lumen.db \
    LUMEN_BACKUP_DIR=/data/backups \
    LUMEN_LISTEN_ADDR=0.0.0.0:8787

EXPOSE 8787

HEALTHCHECK --interval=60s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8787/api/v1/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/lumen-server"]
