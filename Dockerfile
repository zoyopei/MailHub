# MailHub 多阶段构建（TECH.md §5）：node 前端 → Go 静态二进制 → distroless 运行时。
# 纯 Go 无 CGO（modernc.org/sqlite），单二进制内嵌 Web UI。

# ---- 阶段 1：前端（Vue 3 + vite；产物为架构无关静态文件，固定在构建机架构上构建）----
FROM --platform=$BUILDPLATFORM node:20-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- 阶段 2：Go 二进制（固定在构建机原生架构，按 TARGETOS/TARGETARCH 交叉编译，arm64 无需 QEMU）----
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 用真实前端产物覆盖占位 index.html（web/embed.go 内嵌）
COPY --from=web /src/web/dist web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/mailhub ./cmd/mailhub
# 预建空 /data（属主在最终阶段按运行 UID 授予）
RUN mkdir -p /out/data

# ---- 阶段 3：最小运行时 ----
# distroless/static 含 CA 证书（Graph/token 端点 HTTPS 必需）与 tzdata。
# 镜像不绑定运行用户（base 用默认 tag，非 nonroot）：UID/GID 完全由部署侧指定——
# docker-compose 默认 user: "1000:1000"，裸 docker run 请自行 -u <uid>:<gid>（默认 root）。
# /data 预授权 1000:1000 与 compose 默认一致：named volume 首次初始化继承镜像内属主。
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/mailhub /mailhub
COPY --from=build --chown=1000:1000 /out/data /data
ENV MAILHUB_LISTEN=:18099 \
    MAILHUB_DATA_DIR=/data
EXPOSE 18099
VOLUME /data
ENTRYPOINT ["/mailhub"]
