# 使用官方Go镜像作为基础镜像
FROM golang:1.24-alpine AS builder

# 设置工作目录
WORKDIR /app

# 复制go mod和sum文件
COPY go.mod go.sum ./

# 下载依赖
RUN go mod download

# 复制源代码
COPY . .

# 构建应用
RUN go build -o evm-trackooor .

# 使用更小的基础镜像
FROM alpine:latest

# 安装ca-certificates以支持HTTPS请求
RUN apk --no-cache add ca-certificates

# 设置工作目录
WORKDIR /root/

# 从builder阶段复制编译好的二进制文件
COPY --from=builder /app/evm-trackooor .

# 复制配置文件和数据文件
COPY --from=builder /app/data/ ./data/

# 创建目录以存储可能的输出文件
RUN mkdir -p ./output

# 暴露端口（如果应用需要）
# EXPOSE 8080

# 设置入口点，但具体的启动命令留给docker-compose.yml
ENTRYPOINT ["./evm-trackooor"]
