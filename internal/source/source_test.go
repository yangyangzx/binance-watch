package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"binance-watch/internal/config"
	"binance-watch/internal/price"
)

// Binance 文档里的真实 kline 帧（含 t/T、l/L、e/E 这些只差大小写的键）。
const sampleFrame = `{"stream":"btcusdt@kline_1m","data":{"e":"kline","E":1672515782136,"s":"BTCUSDT","k":{"t":1672515780000,"T":1672515839999,"s":"BTCUSDT","i":"1m","f":100,"L":200,"o":"16500.10","c":"16510.55","h":"16520.00","l":"16490.25","v":"1000","n":100,"x":false,"q":"1.0000","V":"500","Q":"0.500","B":"123123"}}}`

func TestParseFrame(t *testing.T) {
	sym, k, err := parseFrame([]byte(sampleFrame))
	if err != nil {
		t.Fatal(err)
	}
	if sym != "BTCUSDT" || k.OpenTime != 1672515780000 {
		t.Errorf("sym=%s open=%d（应取 k.t 开盘时间而不是 k.T 收盘时间）", sym, k.OpenTime)
	}
	if k.Open != price.New(16500.10) || k.Close != price.New(16510.55) ||
		k.High != price.New(16520) || k.Low != price.New(16490.25) {
		t.Errorf("OHLC 解析错误: %+v", k)
	}
	if k.Closed {
		t.Error("x=false 不应标记收盘")
	}
	if _, _, err := parseFrame([]byte(`{"result":null,"id":1}`)); err == nil {
		t.Error("非 kline 帧应报错")
	}
}

func TestParseKlines(t *testing.T) {
	now := int64(1_700_000_120_000)
	body := `[[1700000000000,"100.0","101.0","99.0","100.5","10",1700000059999,"1",5,"0.5","0.1","0"],
	          [1700000060000,"100.5","102.0","100.0","101.5","10",1700000119999,"1",5,"0.5","0.1","0"],
	          [1700000120000,"101.5","102.0","101.0","101.8","10",1700000179999,"1",5,"0.5","0.1","0"]]`
	ks, err := parseKlines([]byte(body), now)
	if err != nil || len(ks) != 3 {
		t.Fatalf("%v %d", err, len(ks))
	}
	if !ks[0].Closed || !ks[1].Closed || ks[2].Closed {
		t.Errorf("closed 标记错误: %v %v %v", ks[0].Closed, ks[1].Closed, ks[2].Closed)
	}
	if ks[2].Close != price.New(101.8) {
		t.Errorf("close=%v", ks[2].Close)
	}
}

// update() 的保护（与 merge 对齐）：已收盘的根不可被乱序/重放帧改回去；
// 迟到的更旧帧不得回退最新价与更新时间。
func TestUpdateGuards(t *testing.T) {
	s := testSource(t)
	s.update("BTCUSDT", Kline{OpenTime: 60000, Close: price.New(10), Closed: true})
	// 乱序/重放的未收盘快照：不得把已收盘根打回未收盘，也不得改动其数值
	s.update("BTCUSDT", Kline{OpenTime: 60000, Close: price.New(9), Closed: false})
	if k := s.klines["BTCUSDT"][0]; k.Close != price.New(10) || !k.Closed {
		t.Errorf("已收盘 K 线被 update 覆盖: %+v", k)
	}
	// 新的实时根
	s.update("BTCUSDT", Kline{OpenTime: 120000, Close: price.New(11), Closed: false})
	// 迟到的旧帧：插入历史可以，但最新价/时间不能回退
	s.update("BTCUSDT", Kline{OpenTime: 0, Close: price.New(999), Closed: true})
	if q := s.Quote("BTCUSDT"); q.Price != price.New(11) {
		t.Errorf("最新价被旧帧回退: %v", q.Price)
	}
	if n := s.Bars("BTCUSDT"); n != 3 {
		t.Errorf("旧帧应插入历史后共 3 根，得 %d", n)
	}
	// 实时根（未收盘）仍可被同根覆盖前进
	s.update("BTCUSDT", Kline{OpenTime: 120000, Close: price.New(12), Closed: false})
	if q := s.Quote("BTCUSDT"); q.Price != price.New(12) {
		t.Errorf("实时根应能前进: %v", q.Price)
	}
}

func TestUpsertOrderedAndTrim(t *testing.T) {
	var arr []Kline
	for _, ts := range []int64{3, 1, 2, 2, 5, 4} {
		arr = upsert(arr, Kline{OpenTime: ts * 60000, Close: price.Price(ts)})
	}
	if len(arr) != 5 {
		t.Fatalf("len=%d", len(arr))
	}
	for i := 1; i < len(arr); i++ {
		if arr[i-1].OpenTime >= arr[i].OpenTime {
			t.Fatal("not sorted")
		}
	}
	arr = nil
	for i := 0; i < BufferBars+50; i++ {
		arr = upsert(arr, Kline{OpenTime: int64(i) * 60000})
	}
	if len(arr) != BufferBars || arr[len(arr)-1].OpenTime != int64(BufferBars+49)*60000 {
		t.Errorf("trim wrong: len=%d", len(arr))
	}
}

func testSource(t *testing.T) *Source {
	t.Helper()
	s, err := New(&config.Config{APIBase: "http://127.0.0.1:1", StreamURL: "ws://127.0.0.1:1", Symbols: []string{"BTCUSDT"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestChangeTimeAligned(t *testing.T) {
	s := testSource(t)
	// 锚定到「现在」：最后一根 K 线的开盘时间 = now，窗口基准才在缓冲里。
	now := time.Now().UnixMilli()
	now -= now % 60000
	// 60 根 1m，价格线性 100 → 159
	for i := 0; i < 60; i++ {
		p := price.New(float64(100 + i))
		s.update("BTCUSDT", Kline{OpenTime: now - int64(59-i)*60000, Open: p, Close: p})
	}
	if bp, ok := s.Change("BTCUSDT", 10); !ok || bp != price.PctBP(price.New(159), price.New(149)) {
		t.Errorf("10m: %d %v", bp, ok)
	}
	if _, ok := s.Change("BTCUSDT", 240); ok {
		t.Error("数据不足 240m 时必须返回 ok=false，不能拿短窗口冒充")
	}
	// 制造空洞：删掉 [25,35) 根。10m 基准落在空洞外 → 仍可用 35m 基准
	arr := s.klines["BTCUSDT"]
	s.klines["BTCUSDT"] = append(append([]Kline{}, arr[:25]...), arr[35:]...)
	if _, ok := s.Change("BTCUSDT", 30); ok {
		t.Error("基准落在 10 分钟空洞中应返回 ok=false")
	}
	// 5m 窗口：基准 now-5m 在空洞外 → 仍正常
	if bp, ok := s.Change("BTCUSDT", 5); !ok || bp == 0 {
		t.Errorf("5m 未受空洞影响: %d %v", bp, ok)
	}
}

// 数据停滞（WS 断线、最后一根 K 线已陈旧）时，75s 停滞闸门让所有窗口返回 ok=false，
// 不拿「最后一根 K 线往前 N 分钟」的陈旧区间冒充实时窗口。
func TestChangeStaleData(t *testing.T) {
	s := testSource(t)
	now := time.Now().UnixMilli()
	now -= now % 60000
	// 60 根，最后一根开盘时间在 40 分钟**前**（now 往前推 40min，符合 K 线时间轴），远超 75s 闸门
	for i := 0; i < 60; i++ {
		p := price.New(float64(100 + i))
		s.update("BTCUSDT", Kline{OpenTime: now - int64(40)*60000 - int64(59-i)*60000, Open: p, Close: p})
	}
	for _, m := range []int{5, 60, 240} {
		if _, ok := s.Change("BTCUSDT", m); ok {
			t.Errorf("数据停滞 40m 后 Change(%dm) 应返回 ok=false，不能拿陈旧 K 线冒充", m)
		}
	}
}

func TestMergeKeepsClosedWS(t *testing.T) {
	s := testSource(t)
	s.update("BTCUSDT", Kline{OpenTime: 60000, Close: price.New(10), Closed: true})
	s.merge("BTCUSDT", []Kline{{OpenTime: 60000, Close: price.New(99), Closed: false}, {OpenTime: 120000, Close: price.New(11)}}, false)
	if got := s.klines["BTCUSDT"][0].Close; got != price.New(10) {
		t.Errorf("已收盘 K 线不应被未收盘数据覆盖: %v", got)
	}
	if q := s.Quote("BTCUSDT"); q.Price != price.New(10) {
		t.Errorf("touch=false 不应改最新价: %v", q.Price)
	}
	// 未收盘的实时 K 线：回填快照不得覆盖，REST 兜底（touch=true）可以
	s.update("BTCUSDT", Kline{OpenTime: 180000, Close: price.New(20)})
	s.merge("BTCUSDT", []Kline{{OpenTime: 180000, Close: price.New(19)}}, false)
	if got := s.klines["BTCUSDT"][2].Close; got != price.New(20) {
		t.Errorf("回填快照覆盖了 WS 实时数据: %v", got)
	}
	s.merge("BTCUSDT", []Kline{{OpenTime: 180000, Close: price.New(21)}}, true)
	if got := s.klines["BTCUSDT"][2].Close; got != price.New(21) {
		t.Errorf("兜底应能刷新: %v", got)
	}
}

// 端到端：假 Binance（REST + combined WS），验证 回填 → WS 更新 → 最新价/窗口涨跌幅 → 订阅变更重连。
func TestEndToEnd(t *testing.T) {
	var wsConns atomic.Int32
	var lastQuery atomic.Value
	nowMs := time.Now().UnixMilli()
	cur := nowMs - nowMs%60000

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/klines", func(w http.ResponseWriter, r *http.Request) {
		sym := r.URL.Query().Get("symbol")
		if sym == "BADUSDT" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"code":-1121,"msg":"Invalid symbol."}`)
			return
		}
		var rows []string
		for i := 0; i < BufferBars; i++ {
			ot := cur - int64(BufferBars-1-i)*60000
			rows = append(rows, fmt.Sprintf(`[%d,"100.0","100.0","100.0","100.0","1",%d,"1",1,"1","1","0"]`, ot, ot+59999))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(rows, ","))
	})
	up := websocket.Upgrader{}
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		wsConns.Add(1)
		lastQuery.Store(r.URL.Query().Get("streams"))
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		go func() { // 排空客户端的 ping/pong
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}()
		for i := 0; i < 200; i++ {
			frame, _ := json.Marshal(map[string]any{"stream": "btcusdt@kline_1m", "data": map[string]any{
				"e": "kline", "E": 1, "s": "BTCUSDT",
				"k": map[string]any{"t": cur, "T": cur + 59999, "s": "BTCUSDT", "i": "1m", "f": 1, "L": 2,
					"o": "100.0", "c": "103.0", "h": "103.0", "l": "100.0", "x": false},
			}})
			if c.WriteMessage(websocket.TextMessage, frame) != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s, err := New(&config.Config{
		APIBase: srv.URL, StreamURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
		Symbols: []string{"BTCUSDT"},
	})
	if err != nil {
		t.Fatal(err)
	}
	invalid := make(chan string, 1)
	s.OnInvalid = func(sym string) { invalid <- sym }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if cond() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("超时等待: %s", what)
	}
	waitFor("WS 价格 103", func() bool { return s.Quote("BTCUSDT").Price == price.New(103) })
	waitFor("回填满缓冲", func() bool { return s.Bars("BTCUSDT") >= BufferBars-1 })
	// 回填的当前根与 WS 的当前根必须是同一根（OpenTime 对齐），不能出现重复根
	if n := s.Bars("BTCUSDT"); n > BufferBars {
		t.Errorf("出现重复 K 线: %d", n)
	}
	if bp, ok := s.Change("BTCUSDT", 60); !ok || bp != 300 {
		t.Errorf("60m 涨幅应为 +3.00%%: bp=%d ok=%v", bp, ok)
	}
	if bp, ok := s.Change("BTCUSDT", 240); !ok || bp != 300 {
		t.Errorf("240m 涨幅应为 +3.00%%: bp=%d ok=%v", bp, ok)
	}
	if !s.Stats().WSConnected {
		t.Error("WS 应处于已连接")
	}

	// 动态增加订阅：一个有效、一个无效
	before := wsConns.Load()
	s.SetSymbols([]string{"BTCUSDT", "BADUSDT"})
	select {
	case sym := <-invalid:
		if sym != "BADUSDT" {
			t.Errorf("invalid=%s", sym)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("无效交易对未被识别")
	}
	waitFor("订阅变化后重连", func() bool { return wsConns.Load() > before })
	if q, _ := lastQuery.Load().(string); strings.Contains(q, "badusdt") && wsConns.Load() > before+1 {
		t.Errorf("无效交易对不应出现在订阅里: %s", q)
	}
	if !s.IsInvalid("BADUSDT") {
		t.Error("IsInvalid 应为 true")
	}
}
