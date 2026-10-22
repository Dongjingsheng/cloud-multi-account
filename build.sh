#!/usr/bin/env bash
# 构建双端 Go 桥（Linux 上构建 win + linux；需要 Go 工具链）
set -e
cd "$(dirname "$0")/bridge"

echo "[1/2] 构建 Windows x64 ..."
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o cloud_bridge_win.exe cloud_bridge_full.go

echo "[2/2] 构建 Linux amd64 ..."
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o cloud_bridge_linux cloud_bridge_full.go

echo "完成：bridge/cloud_bridge_win.exe + bridge/cloud_bridge_linux"
chmod +x cloud_bridge_linux
