package source

import (
	"testing"
	"time"

	"binance-watch/internal/price"
)

func TestATRBasic(t *testing.T) {
	s := testSource(t)
	now := time.Now().UnixMilli()
	now -= now % 60000
	// 造 30 根 1m K 线：Open=Close=i*1, High=i*1+0.5, Low=i*1-0.5
	// 每根 1m K 线的 TR = 1.0（H-L）；跨桶后 TR 由跨桶价差决定
	for i := 0; i < 30; i++ {
		p := price.New(float64(i) + 1)
		h := price.New(float64(i) + 1.5)
		l := price.New(float64(i) + 0.5)
		s.update("TESTUSDT", Kline{
			OpenTime: now - int64(29-i)*60000,
			Open:     p, High: h, Low: l, Close: p,
			Closed: true,
		})
	}
	// 5m 周期：30根1m → 6 个桶；ATR(5) 需要 ≥6 根 5m 桶（n+1）
	if v, ok := s.ATR("TESTUSDT", 5, 5); !ok {
		t.Error("5m 应有足够 1m K 线算 ATR(5)")
	} else if v <= 0 {
		t.Errorf("ATR 应为正值，得 %v", v)
	}
	// 1h 周期：30根1m → 只有 1 桶；ATR(14) 需 15 桶 → 必须 ok=false
	if _, ok := s.ATR("TESTUSDT", 60, 14); ok {
		t.Error("数据不足时 1h ATR(14) 必须 ok=false")
	}
	// 不存在的交易对
	if _, ok := s.ATR("NOSYMBOL", 5, 5); ok {
		t.Error("无数据交易对必须 ok=false")
	}
}

// 桶聚合正确性的回归锁：与独立参考实现（按桶 id 归位聚合）逐值对比。
// 旧实现按「下一根是否同桶」归位，导致每桶末根单独成桶、下桶首根混入上桶，ATR 系统性失真
// （旧实现下本测试报 got≠want，已验证）。
func TestATRAgainstReferenceAggregation(t *testing.T) {
	s := testSource(t)
	now := time.Now().UnixMilli()
	now -= now % 60000
	var arr []Kline
	for i := 0; i < 30; i++ {
		k := Kline{
			OpenTime: now - int64(29-i)*60000,
			Open:     price.New(float64(i) * 10),
			High:     price.New(float64(i*10 + 7)),
			Low:      price.New(float64(i*10 - 3)),
			Close:    price.New(float64(i*10 + 5)),
			Closed:   true,
		}
		arr = append(arr, k)
		s.update("TESTUSDT", k)
	}

	// 独立参考聚合：按桶 id 归位；末桶因末根未收盘会被丢弃，但这里末根 Closed=true → 保留。
	var buckets []Kline
	for _, k := range arr {
		id := k.OpenTime / 300000 * 300000
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
	n := 5
	if len(buckets) < n+1 {
		t.Fatalf("参考桶数不足: %d", len(buckets))
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
	want := price.Price(sum / int64(n))
	if got, ok := s.ATR("TESTUSDT", 5, n); !ok || got != want {
		t.Errorf("ATR(5m,%d) 与参考实现不一致: got=%v ok=%v want=%v", n, got.Float(), ok, want.Float())
	}
}

// BufferBars 回归锁：ATR(1h,14) 需要 15 个完整小时桶（≥900 根 1m）。
// 旧缓冲只有 250 根 → 永远 ok=false，README 承诺的 1h ATR 从未展示过。
func TestATR1hNeedsFullBuffer(t *testing.T) {
	if BufferBars < 15*60 {
		t.Fatalf("BufferBars=%d：ATR(1h,14) 至少要 900 根 1m", BufferBars)
	}
	s := testSource(t)
	now := time.Now().UnixMilli()
	now -= now % 60000
	// 塞满缓冲：所有桶 H=105/L=95/O=100 → 每桶 TR=10，ATR 与对齐无关
	for i := 0; i < BufferBars; i++ {
		s.update("TESTUSDT", Kline{
			OpenTime: now - int64(BufferBars-1-i)*60000,
			Open:     price.New(100), High: price.New(105), Low: price.New(95), Close: price.New(100),
			Closed: true,
		})
	}
	v, ok := s.ATR("TESTUSDT", 60, 14)
	if !ok {
		t.Fatal("满缓冲下 ATR(1h,14) 必须可算（ok=true）")
	}
	if v != price.New(10) {
		t.Errorf("ATR(1h,14) 应=10，得 %v", v.Float())
	}
}

// 数据停滞时（最后一根 1m K 线明显旧于当前分钟）所有 ATR 都不可信。
func TestATRStaleData(t *testing.T) {
	s := testSource(t)
	now := time.Now().UnixMilli()
	now -= now % 60000
	// 30 根 1m K 线，最后一根开盘时间 40 分钟**前**（远超 75s 闸门）
	for i := 0; i < 30; i++ {
		p := price.New(float64(i) + 1)
		s.update("TESTUSDT", Kline{
			OpenTime: now - int64(40)*60000 - int64(29-i)*60000,
			Open:     p, High: p, Low: p, Close: p,
			Closed: true,
		})
	}
	for _, m := range []int{5, 15, 60} {
		if _, ok := s.ATR("TESTUSDT", m, 5); ok {
			t.Errorf("数据停滞 40m 后 ATR(%dm,5) 应 ok=false", m)
		}
	}
}

// ATR 必须用到 High/Low（TR = max(H-L, |H-前开|, |L-前开|)），
// 不能只看 Open/Close。
func TestATRUsesHighLow(t *testing.T) {
	s := testSource(t)
	now := time.Now().UnixMilli()
	now -= now % 60000
	// 造 25 根 1m K 线，Open/Close 全相等（100），但 High/Low 跨度为 10
	// 5m 周期 → 5 个完整桶；ATR(4) 需 5 桶 → 恰好够用
	for i := 0; i < 25; i++ {
		p := price.New(100)
		h := price.New(105)
		l := price.New(95)
		s.update("TESTUSDT", Kline{
			OpenTime: now - int64(24-i)*60000,
			Open:     p, High: h, Low: l, Close: p,
			Closed: true,
		})
	}
	v, ok := s.ATR("TESTUSDT", 5, 4)
	if !ok {
		t.Fatal("应有数据")
	}
	// 桶内 H-L = 10；相邻桶价差为 0（所有 O=C=100），故 TR=10
	// ATR = 10（4 个桶都 TR=10）
	if v != price.New(10) {
		t.Errorf("ATR 应=10（H-L 跨度），得 %v", v.Float())
	}
}

// 末桶未收盘时丢弃，避免 TR 用半根 K 线。
func TestATRDiscardsOpenLastBucket(t *testing.T) {
	s := testSource(t)
	now := time.Now().UnixMilli()
	now -= now % 60000
	// 30 根 1m K 线：i=0..28 Closed=true，i=29（OpenTime=now）Closed=false（WS 正在更新）
	for i := 0; i < 34; i++ {
		p := price.New(float64(i) + 1)
		s.update("TESTUSDT", Kline{
			OpenTime: now - int64(34-i)*60000,
			Open:     p, High: p, Low: p, Close: p,
			Closed: true,
		})
	}
	// 未收盘的最后一根：当前分钟（now）
	openP := price.New(35)
	s.update("TESTUSDT", Kline{
		OpenTime: now,
		Open:     openP, High: openP, Low: openP, Close: openP,
		Closed: false,
	})
	// 35 根连续 1m（含 1 根未收盘）：任意墙钟对齐下跨 7~8 个 5m 桶，
	// 未收盘根所在的末桶被丢弃 → 稳定剩 6~7 个可用桶。
	// ATR(5) 需 6 桶 → 任何对齐都恰好够用
	if _, ok := s.ATR("TESTUSDT", 5, 5); !ok {
		t.Error("丢弃末桶后仍有 ≥6 个桶，ATR(5) 应 ok=true")
	}
	// ATR(7) 需 8 桶 → 最多 7 个 → ok=false
	if _, ok := s.ATR("TESTUSDT", 5, 7); ok {
		t.Error("可用桶 < 8 → ATR(7) 必须 ok=false")
	}
}
