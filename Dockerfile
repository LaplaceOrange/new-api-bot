# syntax=docker/dockerfile:1

# 使用 Go 1.23 Alpine 镜像编译静态二进制。
FROM golang:1.23-alpine AS builder

# 安装证书和版本控制工具，供依赖下载使用。
RUN apk add --no-cache ca-certificates git

# 设置构建工作目录。
WORKDIR /src

# 先复制依赖描述文件，以便复用 Docker 构建缓存。
COPY go.mod go.sum ./

# 下载并校验 Go 模块依赖。
RUN go mod download && go mod verify

# 复制项目源代码。
COPY . .

# 构建不依赖 CGO 的精简 Linux 可执行文件。
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/new-api-bot ./cmd/bot

# 使用仅包含 CA 证书的精简运行时镜像。
FROM gcr.io/distroless/static-debian12:latest

# 设置容器工作目录。
WORKDIR /app

# 从构建阶段复制机器人可执行文件。
COPY --from=builder /out/new-api-bot /app/new-api-bot

# 声明健康检查 HTTP 服务端口。
EXPOSE 8080

# 声明默认数据卷目录。
VOLUME ["/data"]

# 启动 QQ × New API 机器人服务。
ENTRYPOINT ["/app/new-api-bot"]
