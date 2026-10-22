@echo off
rem 构建双端 Go 桥（Windows 本地构建 win + linux）
setlocal
cd /d "%~dp0..\bridge"

where go >nul 2>nul
if errorlevel 1 (
  echo 未找到 go，请安装 Go 或指定完整路径：set PATH=D:\soft\go\bin;%%PATH%%
  exit /b 1
)

echo [1/2] 构建 Windows x64 ...
go build -trimpath -ldflags "-s -w" -o cloud_bridge_win.exe cloud_bridge_full.go
if errorlevel 1 exit /b 1

echo [2/2] 构建 Linux amd64 ...
set GOOS=linux
set GOARCH=amd64
go build -trimpath -ldflags "-s -w" -o cloud_bridge_linux cloud_bridge_full.go

echo 完成：bridge/cloud_bridge_win.exe + bridge/cloud_bridge_linux
endlocal
