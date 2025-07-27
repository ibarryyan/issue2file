#!/bin/bash

echo "=== Issue2File 测试脚本 ==="

# 检查程序是否存在
if [ ! -f "./issue2file" ]; then
    echo "构建程序..."
    go build -o issue2file
    if [ $? -ne 0 ]; then
        echo "构建失败！"
        exit 1
    fi
fi

echo "1. 测试帮助信息..."
./issue2file

echo -e "\n2. 测试解析GitHub URL..."
echo "测试仓库: golang/go (这是一个公开仓库，用于测试)"

# 如果设置了GITHUB_TOKEN，使用它
if [ -n "$GITHUB_TOKEN" ]; then
    echo "检测到GITHUB_TOKEN，将使用认证访问"
else
    echo "未设置GITHUB_TOKEN，将使用匿名访问（有API限制）"
fi

echo "开始获取issues..."
./issue2file golang/go

echo -e "\n测试完成！"