# 使用 Go 官方镜像作为构建阶段的基础镜像
FROM golang:1.22 as builder

# 设置工作目录
WORKDIR /app

# 复制 go.mod 和 go.sum 文件
COPY go.mod go.sum ./

# 下载所有依赖
RUN go mod download

# 复制源代码到容器中
COPY . .

# 编译应用程序。CGO_ENABLED=0 用于静态链接，以便在不同的 Linux 发行版上运行。
# -o scaleinaksnode 指定输出的二进制文件名。
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o scaleinaksnode .

# 使用 Ubuntu 22.04 作为基础镜像
FROM ubuntu:22.04

# 安装 curl 和 bash
RUN apt-get update && apt-get install -y curl bash && rm -rf /var/lib/apt/lists/*

# 从构建容器中复制编译好的应用程序到运行时容器
COPY --from=builder /app/scaleinaksnode /scaleinaksnode

# 运行应用程序
ENTRYPOINT ["/scaleinaksnode"]
