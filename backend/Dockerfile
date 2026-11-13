# ---- 构建阶段 ----
FROM golang:1.27-alpine AS build
# 本机网络直连 proxy.golang.org 不通，构建期走国内代理（与宿主机 GOPROXY 口径一致）
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server

# ---- 运行阶段 ----
FROM alpine:3.21
# tzdata：DSN 的 TimeZone=Asia/Shanghai 依赖；ca-certificates：出站 HTTPS（goproxy/GitHub API 等）依赖
RUN apk add --no-cache tzdata ca-certificates
# 不设 TZ 的话 Go 进程 time.Now()/日志全走 UTC（与宿主机、agent 容器差 8 小时）
ENV TZ=Asia/Shanghai
WORKDIR /app
COPY --from=build /out/server /app/server
COPY configs/config.container.yaml /app/configs/config.yaml
EXPOSE 8080
ENTRYPOINT ["/app/server", "-config", "configs/config.yaml"]
