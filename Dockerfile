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

# ---- 阶段 3：最小运行时 ----
# distroless/static 含 CA 证书（Graph/token 端点 HTTPS 必需）与 tzdata；nonroot 非 root 运行。
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mailhub /mailhub
# 容器内必须监听 0.0.0.0（默认 ":8080" 亦可；此处固定 18099 与仓库部署一致）
ENV MAILHUB_LISTEN=:18099 \
    MAILHUB_DATA_DIR=/data
EXPOSE 18099
VOLUME /data
ENTRYPOINT ["/mailhub"]
