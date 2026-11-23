// Package source 行情源：Binance WS（主）+ REST（回填/兜底）。
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"binance-watch/internal/config"
	"binance-watch/internal/price"
)

const (
	// BufferBars 内存里保留的 1m K 线根数：约束来自 ATR(1h,14) —— 14 个 TR 需要 15 个完整小时桶
	// ≈900 根，另加余量。（涨跌幅窗口上限仍由 MaxWindowMin=240 决定，与缓冲区无关。）
	BufferBars = 15*60 + 10
	// MaxWindowMin 涨跌幅最大窗口（分钟）。
	MaxWindowMin = 240

	wsReadTimeout  = 75 * time.Second // Binance 每 ~20s 发 ping，且 kline 每 ~2s 推一次
	wsPingEvery    = 20 * time.Second
	wsWriteTimeout = 10 * time.Second
	fallbackEvery  = 20 * time.Second
	fallbackStale  = 45 * time.Second
	gapTolerance   = 2 * time.Minute // 窗口基准 K 线允许的时间偏差
)

// Kline 一根 1m K 线。OpenTime 为毫秒时间戳（Binance 的 k.t / REST 的 row[0]）。
type Kline struct {
	OpenTime               int64
	Open, High, Low, Close price.Price
	Closed                 bool
}

// Quote 某交易对的最新行情。
type Quote struct {
	Symbol string
	Price  price.Price
	At     time.Time // 本地收到数据的时间
	OK     bool      // 是否有数据
}

// Stats 连接统计（给 status/web 展示）。
type Stats struct {
	WSConnected bool
	WSSince     time.Time
	Reconnects  int64
	LastMsgAt   time.Time
}

// Source 行情源。
type Source struct {
	cfg    *config.Config
	client *http.Client
	dialer websocket.Dialer

	mu          sync.RWMutex
	klines      map[string][]Kline // 升序，按 OpenTime 去重
	lastPrice   map[string]price.Price
	lastAt      map[string]time.Time
	symbols     []string        // 期望订阅（已排序去重）
	invalid     map[string]bool // Binance 不认识的交易对
	backfilling map[string]bool // 该交易对是否有回填正在进行（防叠出多个并发循环）
	symCh       chan struct{}   // 订阅集合变化 → 触发 WS 重连

	wsUp       atomic.Bool
	wsSince    atomic.Int64 // unix nano
	reconnects atomic.Int64
	lastMsg    atomic.Int64 // unix nano

	// OnInvalid 在发现无效交易对时回调（只会触发一次/交易对）。
	OnInvalid func(sym string)
}

// New 创建行情源。
func New(cfg *config.Config) (*Source, error) {
	s := &Source{
		cfg:         cfg,
		klines:      map[string][]Kline{},
		lastPrice:   map[string]price.Price{},
		lastAt:      map[string]time.Time{},
		invalid:     map[string]bool{},
		backfilling: map[string]bool{},
		symCh:       make(chan struct{}, 1),
	}
	tr := &http.Transport{
		Proxy:                 nil, // 显式直连：不读 HTTP(S)_PROXY 环境变量
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	s.dialer = websocket.Dialer{HandshakeTimeout: 15 * time.Second, ReadBufferSize: 16 << 10, WriteBufferSize: 4 << 10}
	if cfg.ProxyURL != "" {
		pu, err := url.Parse(cfg.ProxyURL)
		if err != nil || pu.Host == "" {
			return nil, fmt.Errorf("代理地址不合法 %q", cfg.ProxyURL)
		}
		tr.Proxy = http.ProxyURL(pu)
		s.dialer.Proxy = http.ProxyURL(pu)
	}
	s.client = &http.Client{Transport: tr, Timeout: 20 * time.Second}
	s.SetSymbols(cfg.Symbols)
	return s, nil
}

// SetSymbols 设置订阅集合；集合变化会触发 WS 重连（并自动补回填）。
func (s *Source) SetSymbols(syms []string) {
	set := map[string]bool{}
	for _, x := range syms {
		if x = strings.ToUpper(strings.TrimSpace(x)); x != "" {
			set[x] = true
		}
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	s.mu.Lock()
	same := len(out) == len(s.symbols)
	if same {
		for i := range out {
			if out[i] != s.symbols[i] {
				same = false
				break
			}
		}
	}
	if !same {
		s.symbols = out
	}
	s.mu.Unlock()
	if !same {
		select {
		case s.symCh <- struct{}{}:
		default:
		}
	}
}

// Symbols 返回当前期望订阅的交易对（含无效的）。
func (s *Source) Symbols() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.symbols...)
}

func (s *Source) activeSymbols() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.symbols))
	for _, x := range s.symbols {
		if !s.invalid[x] {
			out = append(out, x)
		}
	}
	return out
}

// IsInvalid 该交易对是否被 Binance 判定为不存在。
func (s *Source) IsInvalid(sym string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.invalid[sym]
}

// Quote 最新价与更新时间。
func (s *Source) Quote(sym string) Quote {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.lastPrice[sym]
	return Quote{Symbol: sym, Price: p, At: s.lastAt[sym], OK: p > 0}
}

// LastPrice 兼容旧接口。
func (s *Source) LastPrice(sym string) price.Price { return s.Quote(sym).Price }

// LastDataAt 兼容旧接口。
func (s *Source) LastDataAt(sym string) time.Time { return s.Quote(sym).At }

// HasData 至少一个 symbol 有数据。
func (s *Source) HasData() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.lastPrice {
		if v > 0 {
			return true
		}
	}
	return false
}

// Stats 连接统计。
func (s *Source) Stats() Stats {
	st := Stats{WSConnected: s.wsUp.Load(), Reconnects: s.reconnects.Load()}
	if v := s.wsSince.Load(); v > 0 {
		st.WSSince = time.Unix(0, v)
	}
	if v := s.lastMsg.Load(); v > 0 {
		st.LastMsgAt = time.Unix(0, v)
	}
	return st
}

// Change 返回 sym 在「最近 minutes 分钟」内的涨跌幅（bp）。
//
// 基准 = 「最后一根 K 线开盘时间 − minutes」，按时间戳（不是下标）定位；「最后一根」是 K 线序列末端
// 的那根——它既可能是 WS 正在更新的未收盘 K 线，也可能是刚回填进来的最近一根，二者时间戳对齐。
// 这样 WS 断线造成的 K 线空洞不会把窗口算歪，也不会在数据缺口时拿更短的窗口冒充。
// 数据停滞超过一个 1m K 线周期（>75s，即最后一根已明显旧于当前分钟）时返回 ok=false，
// 让调用方跳过判定而不是拿陈旧 K 线冒充实时窗口。minutes<=0 表示整个 4h 缓冲。
func (s *Source) Change(sym string, minutes int) (bp int64, ok bool) {
	if minutes <= 0 || minutes > MaxWindowMin {
		minutes = MaxWindowMin
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	arr := s.klines[sym]
	n := len(arr)
	if n == 0 {
		return 0, false
	}
	// 数据停滞闸门：最后一根 K 线（WS 实时或 REST 回填）若已明显旧于当前分钟，窗口不可信。
	// 75s = 一个 1m 周期 + 15s 余量；正常 WS 每分钟至少推一根，不会误伤。
	if time.Now().UnixMilli()-arr[n-1].OpenTime > 75_000 {
		return 0, false
	}
	cur := arr[n-1]
	ref, found := refKline(arr, cur.OpenTime-int64(minutes)*60_000)
	if !found || ref.Open <= 0 {
		return 0, false
	}
	return price.PctBP(cur.Close, ref.Open), true
}

// ATRPeriods 展示 ATR 时用的周期与 TR 根数（与涨跌幅窗口一一对应）。
// ATR(n) = 最近 n 根该周期 K 线真实波幅（TR）的简单均值（Wilder 初始值算法）。
var ATRPeriods = []struct {
	Min   int    // 聚合周期（分钟）
	Label string // 展示标签，与涨跌幅窗口一致
	N     int    // TR 平均根数
}{{5, "5m", 5}, {15, "15m", 14}, {60, "1h", 14}}

// ATR 返回某交易对最近 n 根 minutes 周期 K 线的平均真实波幅（ATR，价格单位）；
// 数据不足或行情停更（同 Change 的 75s 闸门）时 ok=false。
// TR = max(H-L, |H-前开|, |L-前开|)，K 线由 1m 缓冲聚合而来；分钟数与 N 见 ATRPeriods。
func (s *Source) ATR(sym string, minutes, n int) (price.Price, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.atrLocked(sym, minutes, n)
}

// atrLocked 调用方需持有读锁。

func (s *Source) atrLocked(sym string, minutes, n int) (price.Price, bool) {
	arr := s.klines[sym]
	if len(arr) == 0 {
		return 0, false
	}
	// 与 Change 相同的停滞闸门：最后一根 1m K 线已明显旧于当前分钟 → 数据不可信
	if time.Now().UnixMilli()-arr[len(arr)-1].OpenTime > 75_000 {
		return 0, false
	}
	kms := int64(minutes) * 60_000
	if kms <= 0 || n <= 0 {
		return 0, false
	}
	// 把 1m K 线聚合成 minutes 周期的桶：同桶内取 max(High)/min(Low)，Open=桶首根开盘，Close=桶末根收盘。
	// 按「当前根属于哪个桶」归位（而不是看下一根在哪）；末根未收盘（WS 正在更新）→ 它所在的末桶不完整，丢弃。
	buckets := make([]Kline, 0, len(arr)/minutes+2)
	for _, k := range arr {
		id := k.OpenTime / kms * kms
		if m := len(buckets); m > 0 && buckets[m-1].OpenTime == id {
			b := &buckets[m-1]
			if k.High > b.High {
				b.High = k.High
			}
			if k.Low < b.Low {
				b.Low = k.Low
			}
			b.Close = k.Close
			continue
		}
		buckets = append(buckets, Kline{OpenTime: id, Open: k.Open, High: k.High, Low: k.Low, Close: k.Close})
	}
	// 未收盘的实时根所在的桶不完整 → 丢弃：TR 需要整桶。
	if last := arr[len(arr)-1]; !last.Closed && len(buckets) > 0 && buckets[len(buckets)-1].OpenTime == last.OpenTime/kms*kms {
		buckets = buckets[:len(buckets)-1]
	}
	if len(buckets) < n+1 { // 需要 n 个 TR，每个 TR 用到前一根的开盘价
		return 0, false
	}
	var sum int64
	for i := len(buckets) - n; i < len(buckets); i++ {
		b := buckets[i]
		prevOpen := buckets[i-1].Open
		tr := int64(b.High - b.Low)
		if d := int64(b.High - prevOpen); d > tr {
			tr = d
		}
		if d := int64(prevOpen - b.Low); d > tr {
			tr = d
		}
		sum += tr
	}
	return price.Price(sum / int64(n)), true
}

// refKline 在升序 arr 中找开盘时间最接近 target 的 K 线，偏差超过 gapTolerance 视为没找到。
func refKline(arr []Kline, target int64) (Kline, bool) {
	tol := gapTolerance.Milliseconds()
	i := sort.Search(len(arr), func(i int) bool { return arr[i].OpenTime >= target })
	best, bestD := -1, tol+1
	for _, j := range []int{i - 1, i} {
		if j < 0 || j >= len(arr) {
			continue
		}
		d := arr[j].OpenTime - target
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = j, d
		}
	}
	if best < 0 || bestD > tol {
		return Kline{}, false
	}
	return arr[best], true
}

// Bars 返回某交易对当前缓冲的 K 线根数。
func (s *Source) Bars(sym string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.klines[sym])
}

// Start 启动后台协程：回填、WS、REST 兜底。立即返回。
func (s *Source) Start(ctx context.Context) {
	go s.backfillAll(ctx, false) // 不依赖 WS：即使 WS 一直连不上也先拿到历史与最新价
	go s.wsLoop(ctx)
	go s.fallbackLoop(ctx)
}

// ---------------------------------------------------------------- WS

func (s *Source) wsLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		select { // 清掉启动前/上次遗留的变更信号，避免白白多重连一次
		case <-s.symCh:
		default:
		}
		syms := s.activeSymbols()
		if len(syms) == 0 { // 没有要订阅的：等订阅集合变化
			select {
			case <-s.symCh:
			case <-ctx.Done():
				return
			}
			continue
		}
		started := time.Now()
		conn, err := s.wsDial(ctx, syms)
		if err != nil {
			wait := jitter(backoff)
			log.Printf("ws: dial 失败: %v（%s 后重试）", err, wait.Round(100*time.Millisecond))
			if !sleep(ctx, wait) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		log.Printf("ws: 已连接（%d 个交易对）", len(syms))
		s.wsUp.Store(true)
		s.wsSince.Store(time.Now().UnixNano())
		go s.backfillAll(ctx, false) // 每次（重）连都补一遍，覆盖断线期间的 K 线空洞
		s.wsConsume(ctx, conn)
		s.wsUp.Store(false)
		_ = conn.Close()
		s.reconnects.Add(1)
		if time.Since(started) > 30*time.Second {
			backoff = time.Second // 连接稳定过，重置退避；秒断的话继续指数退避，避免死循环
		} else {
			if !sleep(ctx, jitter(backoff)) {
				return
			}
			backoff = nextBackoff(backoff)
		}
	}
}

func nextBackoff(b time.Duration) time.Duration {
	if b *= 2; b > time.Minute {
		return time.Minute
	}
	return b
}

func jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int63n(int64(d/4)+1))
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Source) wsDial(ctx context.Context, syms []string) (*websocket.Conn, error) {
	streams := make([]string, 0, len(syms))
	for _, sym := range syms {
		streams = append(streams, strings.ToLower(sym)+"@kline_1m")
	}
	u := s.cfg.StreamURL + "/stream?streams=" + strings.Join(streams, "/")
	conn, _, err := s.dialer.DialContext(ctx, u, nil)
	return conn, err
}

// 注意：Binance 帧里同时有 t/T、l/L、e/E 这种只差大小写的键。
// encoding/json 对键名大小写不敏感，只有"每个键都有精确匹配的字段"才不会串位 —— 所以这里全部显式声明。
type wsCombined struct {
	Stream string  `json:"stream"`
	Data   wsEvent `json:"data"`
}

type wsEvent struct {
	EventType string  `json:"e"`
	EventTime int64   `json:"E"`
	Symbol    string  `json:"s"`
	K         wsKline `json:"k"`
}

type wsKline struct {
	StartTime int64  `json:"t"`
	CloseTime int64  `json:"T"`
	Symbol    string `json:"s"`
	Interval  string `json:"i"`
	FirstID   int64  `json:"f"`
	LastID    int64  `json:"L"`
	Open      string `json:"o"`
	Close     string `json:"c"`
	High      string `json:"h"`
	Low       string `json:"l"`
	Final     bool   `json:"x"`
}

// parseFrame 解 combined-stream 帧。
func parseFrame(msg []byte) (string, Kline, error) {
	var cf wsCombined
	if err := json.Unmarshal(msg, &cf); err != nil {
		return "", Kline{}, err
	}
	k := cf.Data.K
	sym := k.Symbol
	if sym == "" {
		sym = cf.Data.Symbol
	}
	if sym == "" || cf.Data.EventType != "kline" || k.StartTime == 0 {
		return "", Kline{}, errors.New("not a kline frame")
	}
	var kl Kline
	var err error
	kl.OpenTime, kl.Closed = k.StartTime, k.Final
	if kl.Open, err = price.Parse(k.Open); err != nil {
		return "", Kline{}, err
	}
	if kl.High, err = price.Parse(k.High); err != nil {
		return "", Kline{}, err
	}
	if kl.Low, err = price.Parse(k.Low); err != nil {
		return "", Kline{}, err
	}
	if kl.Close, err = price.Parse(k.Close); err != nil {
		return "", Kline{}, err
	}
	return strings.ToUpper(sym), kl, nil
}

func (s *Source) wsConsume(ctx context.Context, c *websocket.Conn) {
	done := make(chan struct{})
	defer close(done)

	extend := func() { _ = c.SetReadDeadline(time.Now().Add(wsReadTimeout)) }
	extend()
	c.SetReadLimit(1 << 20)
	c.SetPingHandler(func(data string) error {
		extend()
		err := c.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(wsWriteTimeout))
		var ne interface{ Timeout() bool }
		if errors.As(err, &ne) && ne.Timeout() {
			return nil
		}
		return err
	})
	c.SetPongHandler(func(string) error { extend(); return nil })

	// 主动心跳 + ctx 取消 + 订阅变化时关闭连接，让 ReadMessage 立刻返回。
	go func() {
		t := time.NewTicker(wsPingEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				_ = c.Close()
				return
			case <-s.symCh:
				log.Printf("ws: 订阅集合变化，重连")
				_ = c.Close()
				return
			case <-t.C:
				if err := c.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout)); err != nil {
					_ = c.Close()
					return
				}
			}
		}
	}()

	for ctx.Err() == nil {
		_, msg, err := c.ReadMessage()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("ws: 读取失败: %v（将重连）", err)
			}
			return
		}
		extend()
		sym, k, err := parseFrame(msg)
		if err != nil {
			continue
		}
		s.lastMsg.Store(time.Now().UnixNano())
		s.update(sym, k)
	}
}

// Feed 手工注入一根 K 线（测试/回放用，等价于收到一帧 WS 数据）。
func (s *Source) Feed(sym string, k Kline) { s.update(sym, k) }

// update 写入一根 K 线（同根覆盖，新根追加），并刷新最新价。
// 保护与 merge 对齐：已收盘的根不会被乱序/重放的帧改回未收盘快照或改动其数值；
// 迟到的更旧帧只入历史，不回退最新价与更新时间。
func (s *Source) update(sym string, k Kline) {
	s.mu.Lock()
	arr := s.klines[sym]
	i := sort.Search(len(arr), func(i int) bool { return arr[i].OpenTime >= k.OpenTime })
	if i < len(arr) && arr[i].OpenTime == k.OpenTime && arr[i].Closed {
		s.mu.Unlock()
		return
	}
	s.klines[sym] = upsert(arr, k)
	if len(arr) == 0 || k.OpenTime >= arr[len(arr)-1].OpenTime {
		s.lastPrice[sym] = k.Close
		s.lastAt[sym] = time.Now()
	}
	s.mu.Unlock()
}

// upsert 按 OpenTime 有序插入/覆盖，并裁剪到 BufferBars。
func upsert(arr []Kline, k Kline) []Kline {
	n := len(arr)
	if n == 0 || arr[n-1].OpenTime < k.OpenTime { // 最常见：追加
		arr = append(arr, k)
	} else {
		i := sort.Search(n, func(i int) bool { return arr[i].OpenTime >= k.OpenTime })
		if i < n && arr[i].OpenTime == k.OpenTime {
			arr[i] = k
		} else {
			arr = append(arr, Kline{})
			copy(arr[i+1:], arr[i:])
			arr[i] = k
		}
	}
	if over := len(arr) - BufferBars; over > 0 {
		// 重新分配，避免旧元素残留在底层数组里被后续 append 复用
		trimmed := make([]Kline, 0, BufferBars)
		trimmed = append(trimmed, arr[over:]...)
		arr = trimmed
	}
	return arr
}

// merge 合并一批历史 K 线。touch=true 时（REST 兜底）用最新一根刷新最新价/时间。
func (s *Source) merge(sym string, ks []Kline, touch bool) {
	if len(ks) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	arr := s.klines[sym]
	for _, k := range ks {
		if n := len(arr); n > 0 {
			i := sort.Search(n, func(i int) bool { return arr[i].OpenTime >= k.OpenTime })
			if i < n && arr[i].OpenTime == k.OpenTime {
				// 已有这根：已收盘的不再改；回填拿到的"未收盘快照"比 WS 旧，不得覆盖
				// （只有 REST 兜底，即 WS 已断时，才允许用快照刷新未收盘的这根）
				if arr[i].Closed || (!k.Closed && !touch) {
					continue
				}
			}
		}
		arr = upsert(arr, k)
	}
	s.klines[sym] = arr
	if last := ks[len(ks)-1]; touch || s.lastAt[sym].IsZero() {
		s.lastPrice[sym] = last.Close
		s.lastAt[sym] = time.Now()
	}
}

// ---------------------------------------------------------------- REST

type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("http %d: %s", e.Status, e.Body) }

func (s *Source) httpGet(ctx context.Context, path string, q url.Values) ([]byte, error) {
	u := s.cfg.APIBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "binance-watch/2")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b := strings.TrimSpace(string(body))
		if len(b) > 200 {
			b = b[:200]
		}
		return nil, &httpError{Status: resp.StatusCode, Body: b}
	}
	return body, nil
}

// fetchKlines 拉取最近 limit 根 1m K 线。
func (s *Source) fetchKlines(ctx context.Context, sym string, limit int) ([]Kline, error) {
	body, err := s.httpGet(ctx, "/api/v3/klines", url.Values{
		"symbol": {sym}, "interval": {"1m"}, "limit": {strconv.Itoa(limit)},
	})
	if err != nil {
		return nil, err
	}
	return parseKlines(body, time.Now().UnixMilli())
}

// parseKlines 解析 REST klines 响应。
// 行格式: [openTime, open, high, low, close, vol, closeTime, quoteVol, trades, takerBase, takerQuote, ignore]
// 注意 row[9] 是 takerBuyBaseVolume（字符串），不是"是否收盘"；收盘与否要用 closeTime(row[6]) 与当前时间比较。
func parseKlines(body []byte, nowMs int64) ([]Kline, error) {
	var rows [][]json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, err
	}
	out := make([]Kline, 0, len(rows))
	for _, row := range rows {
		if len(row) < 7 {
			continue
		}
		var openT, closeT int64
		if json.Unmarshal(row[0], &openT) != nil || json.Unmarshal(row[6], &closeT) != nil {
			continue
		}
		var o, h, l, c string
		if json.Unmarshal(row[1], &o) != nil || json.Unmarshal(row[2], &h) != nil ||
			json.Unmarshal(row[3], &l) != nil || json.Unmarshal(row[4], &c) != nil {
			continue
		}
		po, e1 := price.Parse(o)
		ph, e2 := price.Parse(h)
		pl, e3 := price.Parse(l)
		pc, e4 := price.Parse(c)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			continue
		}
		out = append(out, Kline{OpenTime: openT, Open: po, High: ph, Low: pl, Close: pc, Closed: closeT < nowMs})
	}
	return out, nil
}

// needBackfill 判断是否需要补：缓冲不足，或最后一根与现在之间有 ≥1 根 K 线的空洞。
func (s *Source) needBackfill(sym string, force bool) bool {
	if force {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	arr := s.klines[sym]
	if len(arr) < BufferBars-15 {
		return true
	}
	return time.Now().UnixMilli()-arr[len(arr)-1].OpenTime > 2*60_000
}

// backfillAll 并发（≤3）回填所有需要的交易对；失败的指数退避重试。
// 同一交易对在已有回填进行时跳过（inflight 集合），避免启动 + 每次重连 + REST 兜底
// 三处对同一交易对叠出多个并发重试循环。
func (s *Source) backfillAll(ctx context.Context, force bool) {
	var todo []string
	for _, sym := range s.activeSymbols() {
		if s.needBackfill(sym, force) {
			todo = append(todo, sym)
		}
	}
	if len(todo) == 0 {
		return
	}
	// 抢 in-flight 标记：已被别的 backfillAll 拿走的跳过
	s.mu.Lock()
	for i, sym := range todo {
		if s.backfilling[sym] {
			todo[i] = ""
		} else {
			s.backfilling[sym] = true
		}
	}
	s.mu.Unlock()

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for _, sym := range todo {
		if sym == "" {
			continue
		}
		wg.Add(1)
		go func(sym string) {
			defer wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.backfilling, sym)
				s.mu.Unlock()
			}()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			s.backfillOne(ctx, sym)
		}(sym)
	}
	wg.Wait()
}

// tryBackfill 原子地为 sym 抢占 in-flight 标记并异步跑 backfillOne。
// 成功抢占返回 true（调用方无需再做 REST 兜底）；标记已被占用返回 false。
func (s *Source) tryBackfill(ctx context.Context, sym string, force bool) bool {
	s.mu.Lock()
	if s.backfilling[sym] {
		s.mu.Unlock()
		return false
	}
	s.backfilling[sym] = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.backfilling, sym)
			s.mu.Unlock()
		}()
		s.backfillOne(ctx, sym)
	}()
	return true
}

func (s *Source) backfillOne(ctx context.Context, sym string) {
	wait := 2 * time.Second
	for attempt := 1; attempt <= 5 && ctx.Err() == nil; attempt++ {
		ks, err := s.fetchKlines(ctx, sym, BufferBars)
		if err == nil {
			s.merge(sym, ks, false)
			log.Printf("backfill %-10s %d 根 K 线", sym, len(ks))
			return
		}
		var he *httpError
		if errors.As(err, &he) && he.Status == http.StatusBadRequest && strings.Contains(he.Body, "-1121") {
			s.markInvalid(sym)
			return
		}
		log.Printf("backfill %s 第 %d 次失败: %v", sym, attempt, err)
		if !sleep(ctx, wait) {
			return
		}
		wait *= 2
	}
}

func (s *Source) markInvalid(sym string) {
	s.mu.Lock()
	already := s.invalid[sym]
	s.invalid[sym] = true
	s.mu.Unlock()
	if already {
		return
	}
	log.Printf("source: 交易对 %s 在 Binance 不存在，已停止订阅", sym)
	if s.OnInvalid != nil {
		go s.OnInvalid(sym)
	}
	select { // 触发 WS 用新的订阅集合重连
	case s.symCh <- struct{}{}:
	default:
	}
}

// fallbackLoop WS 异常时用 REST 拉最近几根 K 线，保证价格与窗口数据都不断档。
func (s *Source) fallbackLoop(ctx context.Context) {
	t := time.NewTicker(fallbackEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var stale []string
		for _, sym := range s.activeSymbols() {
			q := s.Quote(sym)
			if q.At.IsZero() && s.Bars(sym) == 0 {
				// 首次回填一直没成功（例如启动时代理还没起来）：继续补，但复用 in-flight 标记，
				// 避免与 backfillAll 叠出两个并发重试循环。
				if s.tryBackfill(ctx, sym, true) {
					continue
				}
				stale = append(stale, sym) // 已有回填在跑则并入本轮 REST 兜底
				continue
			}
			if time.Since(q.At) > fallbackStale {
				stale = append(stale, sym)
			}
		}
		ok := 0
		for _, sym := range stale {
			ks, err := s.fetchKlines(ctx, sym, 5)
			if err != nil {
				log.Printf("rest 兜底 %s: %v", sym, err)
				continue
			}
			s.merge(sym, ks, true)
			ok++
		}
		if len(stale) > 0 {
			log.Printf("rest 兜底: %d/%d 个交易对已刷新", ok, len(stale))
		}
	}
}
