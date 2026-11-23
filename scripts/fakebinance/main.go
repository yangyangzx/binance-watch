// 假 Binance：REST /api/v3/klines + WS combined kline_1m 流，喂给真 watchd 走全链路。
// 价格随时间做大幅正弦 + 缓慢漂移，保证 5m/15m/1h/4h 窗口都有可见涨跌幅，
// 且 near/到价规则能被真实触发（邮件 dry-run 可观察到）。
// 用法：go run ./scripts/fakebinance  → 监听 127.0.0.1:9444。
package main

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

const addr = "127.0.0.1:9444"

var (
	baseBTC = 70000.0
	baseETH = 3000.0
)

// priceAt 每分钟明显变动（±8%，周期 5400 分钟 = 90 小时）
func priceAt(t time.Time, base float64) float64 {
	mins := float64(t.Unix() / 60)
	cycle := math.Mod(mins, 5400)
	return base * (1 + 0.08*math.Sin(cycle/5400*2*math.Pi))
}

// ohlcAt 生成一根 1m K 线的 OHLC：O/C 用当前分钟价格，H/L 加 ±1% 波动（ATR 需要 H≠L）。
func ohlcAt(t time.Time, base float64) (o, h, l, c float64) {
	o = priceAt(t, base)
	c = o // 1m K 线内 O=C（无分钟内漂移）
	amp := o * 0.01 // 1% 振幅 → H-L = 2%
	h = o + amp
	l = o - amp
	return
}

func main() {
	mux := http.NewServeMux()
	up := websocket.Upgrader{}

	// REST 回填：最近 limit 根 1m（尊重 limit 参数，上限 1000，与真 Binance 一致），价格按公式生成
	mux.HandleFunc("/api/v3/klines", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		cur := now.UnixMilli() - now.UnixMilli()%60000
		base := baseBTC
		if r.URL.Query().Get("symbol") == "ETHUSDT" {
			base = baseETH
		}
		limit := 250
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
				limit = n
			}
		}
		var rows []string
		for i := 0; i < limit; i++ {
			ot := cur - int64(limit-1-i)*60000
			o, h, l, c := ohlcAt(time.UnixMilli(ot), base)
			rows = append(rows, fmt.Sprintf(
				`[%d,"%.2f","%.2f","%.2f","%.2f","1",%d,"1",1,"1","1","0"]`,
				ot, o, h, l, c, ot+59999))
		}
		fmt.Fprintf(w, "[%s]", join(rows, ","))
	})

	// WS combined kline_1m 流：每 2s 推一帧当前未收盘 K 线
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		go func() { // 排掉客户端的 ping
			for {
				if _, _, e := c.ReadMessage(); e != nil {
					return
				}
			}
		}()
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				now := time.Now()
				cur := now.UnixMilli() - now.UnixMilli()%60000
				for _, sym := range []string{"BTCUSDT", "ETHUSDT"} {
					base := baseBTC
					if sym == "ETHUSDT" {
						base = baseETH
					}
					o := priceAt(time.UnixMilli(cur), base)
					pc := priceAt(now, base)
					h := math.Max(o, pc)
					l := math.Min(o, pc)
					jit := (o + pc) / 2 * 0.005
					h += jit
					l -= jit
					msg := fmt.Sprintf(`{"stream":"%s@kline_1m","data":{"e":"kline","E":%d,"s":"%s",`+
						`"k":{"t":%d,"T":%d,"s":"%s","i":"1m","f":1,"L":2,"o":"%.2f","c":"%.2f","h":"%.2f","l":"%.2f","x":false}}}`,
						lower(sym), now.UnixMilli(), sym, cur, cur+59999, sym, o, pc, h, l)
					if c.WriteMessage(websocket.TextMessage, []byte(msg)) != nil {
						return
					}
				}
			}
		}
	})

	fmt.Printf("fake binance on http://%s (REST+WS)\n", addr)
	_ = http.ListenAndServe(addr, mux)
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

func join(a []string, sep string) string {
	out := ""
	for i, s := range a {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
