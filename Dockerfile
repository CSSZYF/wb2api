# syntax=docker/dockerfile:1
#
# 多架构镜像：linux/amd64 + linux/arm64（buildx 一次构建）。
# 版本号由 CI 从 git tag 注入（build-arg VERSION），写进 main.appVersion ——
# 产物版本号唯一来源是 tag，不靠手改源码，所以不会出现 +dirty / +自定义后缀。
#
# 本地构建：
#   docker build --build-arg VERSION=1.9.2-panel -t wb2api:1.9.2-panel .
# 本机无 docker 时由 .github/workflows/docker.yml 在 CI 里构建并推 GHCR。

FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
# GOOS/GOARCH 必须显式从 TARGET* 传下去。
# 踩过的坑：只声明 ARG 而不喂给 go build，BUILDPLATFORM(amd64) 下编出来的就是 amd64
# 二进制，两个架构的镜像里装同一个 amd64 文件 —— 落到 arm64 节点直接
# `exec /app/wb2api: exec format error`。声明 ≠ 生效。
ENV GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    GOFLAGS=-trimpath \
    CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。
RUN LDFLAGS="-s -w -X main.appVersion=${VERSION}" \
 && go build -ldflags "$LDFLAGS" -o /out/wb2api ./cmd/server \
 && go build -ldflags "$LDFLAGS" -o /out/signin_bin ./cmd/signin \
 && go build -ldflags "$LDFLAGS" -o /out/login ./cmd/login \
 && go build -ldflags "$LDFLAGS" -o /out/credit ./cmd/credit
# 构建期门禁：逐个核对产物架构与 TARGETARCH 一致。
# 这道检查是补的——之前只验 manifest index 里有几个 platform，那只说明"层存在"，
# 不说明"层里的二进制是那个架构"，所以 exec format error 一路漏到了集群上。
# buildinfo 里带 GOOS/GOARCH，用它断言，不依赖 file(1)（alpine 没有）。
RUN set -eu; \
    fail=0; \
    for b in wb2api signin_bin login credit; do \
      info="$(go version -m /out/$b)"; \
      if printf '%s' "$info" | grep -q "GOARCH=${TARGETARCH}"; then \
        echo "  arch OK  $b -> ${TARGETOS}/${TARGETARCH}"; \
      else \
        echo "  ARCH MISMATCH  $b 期望 ${TARGETOS}/${TARGETARCH}，实际："; \
        printf '%s\n' "$info"; \
        fail=1; \
      fi; \
    done; \
    [ "$fail" = 0 ] || { echo "构建中止：产物架构与目标架构不一致（会导致 exec format error）"; exit 1; }

FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY login.sh signin.sh credit.sh /app/
COPY scripts/probe_active.py /app/scripts/probe_active.py
RUN sed -i 's/\r$//' /app/login.sh /app/signin.sh /app/credit.sh && chmod 755 /app/login.sh /app/signin.sh /app/credit.sh
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
