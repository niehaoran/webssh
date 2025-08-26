# 使用官方 Go 1.22 运行时镜像作为构建阶段
FROM golang:1.22 AS builder

# 设置工作目录
WORKDIR /app

# 复制 go.mod 和 go.sum 文件
COPY go.mod go.sum ./

# 下载依赖
RUN go mod download

# 复制源代码
COPY . .

# 构建应用
RUN CGO_ENABLED=0 GOOS=linux go build -o webssh-go .

# 使用轻量级的 alpine 镜像作为最终镜像
FROM alpine:latest  

# 安装 ca-certificates 以支持 HTTPS 连接
RUN apk --no-cache add ca-certificates

# 创建非 root 用户
RUN adduser -D appuser

WORKDIR /app

# 从构建阶段复制编译好的二进制文件
COPY --from=builder /app/webssh-go .

# 复制配置文件
COPY config.yaml .

# 更改所有权
RUN chown -R appuser:appuser /app

# 切换到非 root 用户
USER appuser

# 暴露端口
EXPOSE 8080

# 运行应用
CMD ["./webssh-go"]