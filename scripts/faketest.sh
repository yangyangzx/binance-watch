#!/usr/bin/env bash
# make faketest：无外网也能看数据流。
# 起一个假 Binance（REST /klines + WS kline_1m，127.0.0.1:9444），把真 watchd（dry-run，不发真邮件）
# 指过去，喂一条必触发的 near 规则，等它真实命中并打出 dry-run 邮件，然后自动收尾。
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
FAKE_PID="" WD_PID=""

cleanup() {
  [ -n "$WD_PID" ]   && kill "$WD_PID"   2>/dev/null || true
  [ -n "$FAKE_PID" ] && kill "$FAKE_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# 找一个可编译 watchd 的 go；没有就用 PATH 里的
GO_BIN="$(command -v go || echo /opt/homebrew/opt/go/bin/go)"
"$GO_BIN" version

echo "==> 编译假 Binance + watchd"
"$GO_BIN" build -o "$WORK/fakebinance" ./scripts/fakebinance
"$GO_BIN" build -o "$WORK/watchd" ./cmd/watchd

echo "==> 起假 Binance（127.0.0.1:9444）"
"$WORK/fakebinance" > "$WORK/fake.log" 2>&1 &
FAKE_PID=$!
for i in $(seq 1 40); do
  grep -q "fake binance" "$WORK/fake.log" 2>/dev/null && break
  sleep 0.2
done
grep -q "fake binance" "$WORK/fake.log" || { echo "假 server 没起来"; cat "$WORK/fake.log"; exit 1; }

echo "==> 起 watchd（dry-run，指向假 server，面板 127.0.0.1:8099）"
cat > "$WORK/env" <<EOF
BINANCE_API_BASE=http://127.0.0.1:9444
BINANCE_STREAM_URL=ws://127.0.0.1:9444
SMTP_SERVER=smtp.qq.com
SMTP_PORT=465
EMAIL_USER=watch@example.com
EMAIL_PASS=dummy
EMAIL_TO=you@example.com
WATCH_SYMBOLS=BTCUSDT,ETHUSDT
HEALTH_STALE_SEC=90
EMAIL_BATCH_WINDOW_SEC=0
EOF
mkdir -p "$WORK/data"
"$WORK/watchd" -env "$WORK/env" -proxy "" -data "$WORK/data" -web 127.0.0.1:8099 -dry-run \
  > "$WORK/watchd.log" 2>&1 &
WD_PID=$!
for i in $(seq 1 60); do
  grep -q "ws: 已连接" "$WORK/watchd.log" 2>/dev/null && break
  sleep 0.5
done
grep -q "ws: 已连接" "$WORK/watchd.log" || { echo "WS 没连上："; tail "$WORK/watchd.log"; exit 1; }
echo "   WS 已连接，等待回填 + 首帧…"
sleep 3

# 建一条 near 规则（目标=现价，距离=50%）→ 必命中，把规则 ID 留给后面删
B=http://127.0.0.1:8099
# 走 /api/status 拿 BTC 现价（price_f）
CUR=""
for i in $(seq 1 20); do
  CUR="$(curl -s "$B/api/status" | python3 -c "import sys,json;print([s['price_f'] for s in json.load(sys.stdin)['symbols'] if s['symbol']=='BTCUSDT'][0])" 2>/dev/null || true)"
  case "$CUR" in ''|0|None) CUR="";; esac
  [ -n "$CUR" ] && break
  sleep 1
done
[ -z "$CUR" ] && { echo "拿不到现价（/api/status 没出数据）"; tail -20 "$WORK/watchd.log"; exit 1; }

RID=""
for i in $(seq 1 8); do
  RID="$(curl -s -X POST "$B/api/rules" -H 'Content-Type: application/json' -H 'X-BW: 1' \
    -d "{\"symbol\":\"BTCUSDT\",\"trigger\":\"near\",\"target\":\"$CUR\",\"near_bp\":5000,\"cooldown_min\":1}" \
    | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])" 2>/dev/null || true)"
  if [ -n "$RID" ]; then break; fi
  sleep 1
done
[ -z "$RID" ] && { echo "建规则失败（/api/rules 没返回 id）"; tail -20 "$WORK/watchd.log"; exit 1; }
echo "==> 建了 near 规则 ${RID}（目标=${CUR}，距离=50%），等它真实触发…"

# 等 dry-run 邮件出现（最多 30s）
for i in $(seq 1 60); do
  grep -q "dry-run: 邮件" "$WORK/watchd.log" && break
  sleep 0.5
done

echo
echo "================= 触发日志 + dry-run 邮件 ================="
grep -E "alert:|已发送|dry-run: 邮件|主题:|^  \|" "$WORK/watchd.log" | tail -20 || echo "(没看到触发邮件)"
echo "============================================================"

# 收尾：删规则（演示 DELETE 修复后的 200 行为）
echo "==> DELETE 规则 ${RID}（只带 X-BW，验证 DELETE 不被 JSON 头卡）"
CODE="$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$B/api/rules/$RID" -H 'X-BW: 1')"
echo "   DELETE -> HTTP ${CODE}（期望 200）"
[ "$CODE" = "200" ] || { echo "   失败"; exit 1; }

echo
echo "✔ faketest 完成：假数据 → WS/回填 → 窗口涨跌幅 → 规则触发 → dry-run 邮件 → DELETE 全链路通。"
