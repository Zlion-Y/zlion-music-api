# ---------- 构建阶段 ----------
# 音源脚本宿主用的是纯 Go 的 goja，无需 CGO；goja 目前要求 go1.25+
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go script.go sources.go source.example.js ./
COPY ui ./ui
ARG VERSION=v1.0.0
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/zlion-music-api .

# ---------- 运行阶段 ----------
FROM alpine:3.20
LABEL org.opencontainers.image.title="zlion-music-api" \
      org.opencontainers.image.description="音乐直链代理：搜索与取流，不中转音频流"

COPY --from=build /out/zlion-music-api /usr/local/bin/zlion-music-api
RUN mkdir -p /data
# 示例脚本先放成默认脚本，首次 docker run 就能跑；上传新脚本会覆盖它
RUN mkdir -p /data/sources
COPY source.example.js /data/sources/source.example.js

WORKDIR /data
VOLUME ["/data"]
EXPOSE 8787

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:8787/api/health" >/dev/null 2>&1 || exit 1

# 容器里监听 0.0.0.0，由外层反代/端口映射控制暴露面；白名单用 -allow 传
ENTRYPOINT ["/usr/local/bin/zlion-music-api"]
CMD ["-addr", "0.0.0.0:8787", "-sources-dir", "/data/sources", "-config", "/data/config.json"]
