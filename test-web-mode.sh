#!/bin/bash

# Web 模式测试脚本
# 使用方法: ./test-web-mode.sh

set -e

echo "=== Clash-SpeedTest Web 模式测试 ==="
echo ""

# 检查是否已编译
if [ ! -f "./clash-speedtest" ]; then
    echo "❌ 未找到 clash-speedtest 可执行文件"
    echo "请先运行: go build -o clash-speedtest"
    exit 1
fi

# 设置 AUTH_KEY
export AUTH_KEY="test-secret-key-123"
PORT=18080

echo "1️⃣  启动 Web 服务器（端口 $PORT）..."
./clash-speedtest -web -port $PORT &
SERVER_PID=$!

# 等待服务器启动
sleep 2

echo "✅ Web 服务器已启动 (PID: $SERVER_PID)"
echo ""

# 测试健康检查
echo "2️⃣  测试健康检查接口..."
HEALTH_RESPONSE=$(curl -s http://localhost:$PORT/health)
echo "响应: $HEALTH_RESPONSE"
echo ""

# 测试身份验证失败
echo "3️⃣  测试身份验证失败（错误的 AUTH_KEY）..."
AUTH_FAIL_RESPONSE=$(curl -s -w "\n状态码: %{http_code}" -X POST http://localhost:$PORT/speedtest \
  -H "Authorization: Bearer wrong-key" \
  -H "Content-Type: text/yaml" \
  --data-binary @test.yaml)
echo "$AUTH_FAIL_RESPONSE"
echo ""

# 测试身份验证成功
echo "4️⃣  测试测速接口（正确的 AUTH_KEY）..."
echo "发送 test.yaml 配置文件..."
SPEEDTEST_RESPONSE=$(curl -s -w "\n状态码: %{http_code}" -X POST http://localhost:$PORT/speedtest \
  -H "Authorization: Bearer $AUTH_KEY" \
  -H "Content-Type: text/yaml" \
  --data-binary @test.yaml)
echo "$SPEEDTEST_RESPONSE"
echo ""

# 停止服务器
echo "5️⃣  停止 Web 服务器..."
kill $SERVER_PID
wait $SERVER_PID 2>/dev/null || true

echo ""
echo "✅ 测试完成！"
echo ""
echo "📖 使用说明："
echo "   启动服务器: AUTH_KEY=\"your-key\" ./clash-speedtest -web -port 8080"
echo "   调用 API:    curl -X POST http://localhost:8080/speedtest \\"
echo "                  -H \"Authorization: Bearer your-key\" \\"
echo "                  -H \"Content-Type: text/yaml\" \\"
echo "                  --data-binary @config.yaml"
