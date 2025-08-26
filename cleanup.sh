#!/bin/bash

echo "正在清理异常文件..."

# 清理可能的异常文件
rm -rf "'[builder'"
rm -rf "'[stage-1'"
rm -rf "[builder"
rm -rf "[stage-1"

# 清理Docker构建缓存
echo "清理Docker构建缓存..."
docker system prune -f

# 重新构建镜像
echo "重新构建Docker镜像..."
docker build -t webssh-go:latest .

# 验证构建结果
if [ $? -eq 0 ]; then
    echo "✓ Docker镜像构建成功"
    echo "镜像信息:"
    docker images webssh-go:latest
else
    echo "✗ Docker镜像构建失败"
    exit 1
fi

echo "清理和重建完成！" 