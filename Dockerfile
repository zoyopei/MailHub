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
# 非 root 运行：默认 UID 1000，构建时可 --build-arg MAILHUB_UID=<n> 自定义；
# 运行时亦可用 docker run -u / compose user: 覆盖（bind mount 时需宿主目录属主匹配）。
FROM gcr.io/distroless/static-debian12:nonroot
ARG MAILHUB_UID=1000
COPY --from=build /out/mailhub /mailhub
# named volume 首次初始化会继承镜像内目录的属主，保证非 root 也可写 /data
COPY --from=build --chown=${MAILHUB_UID}:${MAILHUB_UID} /out/data /data
ENV MAILHUB_LISTEN=:18099 \
    MAILHUB_DATA_DIR=/data
EXPOSE 18099
VOLUME /data
USER ${MAILHUB_UID}
ENTRYPOINT ["/mailhub"]
