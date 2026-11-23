package engine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"binance-watch/internal/config"
	"binance-watch/internal/notify"
	"binance-watch/internal/price"
	"binance-watch/internal/rules"
	"binance-watch/internal/source"
)

type sink struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (s *sink) Enqueue(m notify.Message) { s.mu.Lock(); s.msgs = append(s.msgs, m); s.mu.Unlock() }
func (s *sink) n() int                   { s.mu.Lock(); defer s.mu.Unlock(); return len(s.msgs) }
func (s *sink) last() notify.Message     { s.mu.Lock(); defer s.mu.Unlock(); return s.msgs[len(s.msgs)-1] }

type env struct {
	e   *Engine
	src *source.Source
	rl  *rules.Store
	out *sink
	off time.Duration
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := &config.Config{
		APIBase: "http://127.0.0.1:1", StreamURL: "ws://127.0.0.1:1",
		Symbols: []string{"BTCUSDT"}, HealthStaleSec: 1_000_000, WatchDir: t.TempDir(),
	}
	src, err := source.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rl, err := rules.New(cfg.WatchDir)
	if err != nil {
		t.Fatal(err)
	}
	out := &sink{}
	en := &env{src: src, rl: rl, out: out}
	en.e = New(cfg, src, rl, out)
	base := time.Now()
	en.e.now = func() time.Time { return base.Add(en.off) }
	return en
}

func (v *env) feed(sym string, p float64) {
	ms := time.Now().UnixMilli()
	ms -= ms % 60000
	v.src.Feed(sym, source.Kline{OpenTime: ms, Open: price.New(p), High: price.New(p), Low: price.New(p), Close: price.New(p)})
}

func (v *env) eval() { v.e.syncRules(false); v.e.evaluate() }

func TestTargetEdgeTriggerAndRearm(t *testing.T) {
	v := newEnv(t)
	if _, err := v.rl.Add(rules.Rule{Symbol: "BTCUSDT", Trigger: rules.TrigTarget, Dir: "above", Target: "70000", CooldownMin: 30}); err != nil {
		t.Fatal(err)
	}
	step := func(p float64, wantTotal int, why string) {
		t.Helper()
		v.feed("BTCUSDT", p)
		v.eval()
		if v.out.n() != wantTotal {
			t.Fatalf("%s: 价格 %.0f 后累计邮件=%d，期望 %d", why, p, v.out.n(), wantTotal)
		}
	}
	step(69000, 0, "未到价")
	step(70100, 1, "首次到价应触发")
	step(70200, 1, "仍在目标之上：不能重复（原版 cooldown=0 时每 3s 一封）")
	v.off += 31 * time.Minute
	step(70300, 1, "冷却已过但价格没离开过 → 仍不触发（边沿触发）")
	step(70000, 1, "刚好在目标上：滞回区内，未复位")
	step(69850, 1, "只回落 0.21%，未超过 0.30% 滞回，仍未复位")
	step(69700, 1, "回落 0.43% → 复位")
	step(70050, 2, "复位且冷却已过 → 再次触发")
	if !strings.Contains(v.out.last().Subject, "BTCUSDT") || !strings.Contains(v.out.last().Text, "70,050") {
		t.Errorf("邮件内容: %+v", v.out.last())
	}
}

func TestCooldownFloorAndHysteresisInteraction(t *testing.T) {
	v := newEnv(t)
	v.rl.Add(rules.Rule{Symbol: "BTCUSDT", Trigger: rules.TrigTarget, Dir: "below", Target: "100", CooldownMin: 0}) // 0 也有 1 分钟兜底
	v.feed("BTCUSDT", 99)
	v.eval()
	v.feed("BTCUSDT", 101) // 复位
	v.eval()
	v.feed("BTCUSDT", 99) // 立刻又触发，但 <1 分钟 → 被冷却兜底拦住
	v.eval()
	if v.out.n() != 1 {
		t.Fatalf("冷却兜底失效: %d", v.out.n())
	}
	v.off += 2 * time.Minute
	v.eval() // 仍保持 armed → 冷却过后触发
	if v.out.n() != 2 {
		t.Fatalf("冷却过后应触发: %d", v.out.n())
	}
}

func TestRestartDoesNotRepeat(t *testing.T) {
	v := newEnv(t)
	r, _ := v.rl.Add(rules.Rule{Symbol: "BTCUSDT", Trigger: rules.TrigTarget, Dir: "above", Target: "100", CooldownMin: 1})
	v.rl.MarkHit(r.ID, time.Now().Add(-time.Hour)) // 上个进程已经提醒过
	v.feed("BTCUSDT", 105)
	v.eval()
	if v.out.n() != 0 {
		t.Fatal("重启后条件仍满足，不应重复提醒")
	}
	v.feed("BTCUSDT", 90) // 离开 → 复位
	v.eval()
	v.feed("BTCUSDT", 101)
	v.eval()
	if v.out.n() != 1 {
		t.Fatalf("复位后应能再次提醒: %d", v.out.n())
	}
}

func TestOnceRule(t *testing.T) {
	v := newEnv(t)
	r, _ := v.rl.Add(rules.Rule{Symbol: "BTCUSDT", Trigger: rules.TrigTarget, Dir: "above", Target: "100", Once: true})
	v.feed("BTCUSDT", 101)
	v.eval()
	got, _ := v.rl.Get(r.ID)
	if v.out.n() != 1 || got.Active || got.HitCount != 1 {
		t.Fatalf("once: n=%d %+v", v.out.n(), got)
	}
	v.feed("BTCUSDT", 90)
	v.eval()
	v.feed("BTCUSDT", 101)
	v.off += time.Hour
	v.eval()
	if v.out.n() != 1 {
		t.Error("一次性规则不应再触发")
	}
}

func TestPctRule(t *testing.T) {
	v := newEnv(t)
	v.rl.Add(rules.Rule{Symbol: "BTCUSDT", Trigger: rules.TrigPct, PctBP: 300, WindowMin: 60, Dir: "down"})
	base := time.Now().UnixMilli()
	base -= base % 60000
	put := func(i int, p float64) {
		v.src.Feed("BTCUSDT", source.Kline{OpenTime: base - int64(60-i)*60000, Open: price.New(p), Close: price.New(p)})
	}
	// 数据不足：只有 10 根 → 不能判定，更不能误触发
	for i := 50; i <= 60; i++ {
		put(i, 100)
	}
	v.eval()
	if v.out.n() != 0 {
		t.Fatal("数据不足不应触发")
	}
	for i := 0; i < 50; i++ {
		put(i, 100)
	}
	put(60, 98) // 1h 内 -2%：未达 3%
	v.eval()
	if v.out.n() != 0 {
		t.Fatal("-2% 未达阈值")
	}
	put(60, 96.5) // -3.5%
	v.eval()
	if v.out.n() != 1 {
		t.Fatalf("-3.5%% 应触发: %d", v.out.n())
	}
	if !strings.Contains(v.out.last().Text, "-3.50%") {
		t.Errorf("邮件应包含涨跌幅:\n%s", v.out.last().Text)
	}
	// 上涨方向不应触发 down 规则
	put(60, 103.5)
	v.eval()
	if v.out.n() != 1 {
		t.Error("方向不符不应触发")
	}
}

func TestHotReloadSyncsSymbols(t *testing.T) {
	v := newEnv(t)
	v.e.syncRules(true)
	if got := v.src.Symbols(); len(got) != 1 {
		t.Fatalf("初始订阅: %v", got)
	}
	other, _ := rules.New(v.e.cfg.WatchDir) // 模拟 CLI 另一个进程
	other.Add(rules.Rule{Symbol: "ETHUSDT", Trigger: rules.TrigTarget, Dir: "above", Target: "5000"})
	v.e.syncRules(false)
	if got := v.src.Symbols(); len(got) != 2 {
		t.Fatalf("热加载后应自动订阅 ETHUSDT: %v", got)
	}
}

func TestHealthDownAndRecover(t *testing.T) {
	v := newEnv(t)
	v.e.cfg.HealthStaleSec = 15
	v.e.boot = time.Now().Add(-10 * time.Minute) // 已过启动宽限，且从未收到数据
	v.e.healthCheck()
	if v.out.n() != 1 || !strings.Contains(v.out.last().Subject, "断流") {
		t.Fatalf("启动后一直没数据也应告警（原版漏掉）: n=%d", v.out.n())
	}
	v.e.healthCheck()
	if v.out.n() != 1 {
		t.Fatal("断流告警不应重复")
	}
	v.feed("BTCUSDT", 100)
	v.e.healthCheck()
	if v.out.n() != 1 {
		t.Fatal("需要连续 2 次正常才恢复")
	}
	v.e.healthCheck()
	if v.out.n() != 2 || !strings.Contains(v.out.last().Subject, "恢复") {
		t.Fatalf("应发恢复邮件: n=%d", v.out.n())
	}
}

func TestStartupGraceNoFalseAlarm(t *testing.T) {
	v := newEnv(t)
	v.e.cfg.HealthStaleSec = 15
	v.e.healthCheck() // 刚启动，还没数据：宽限期内不报
	if v.out.n() != 0 {
		t.Fatal("启动宽限期内不应告警")
	}
}

func TestDecideNear(t *testing.T) {
	r := &rules.Rule{Trigger: rules.TrigNear, Target: "150", NearBP: 50}
	no := func(int) (int64, bool) { return 0, false }
	if v := decide(r, price.New(149.5), no); !v.hit { // 距 0.33%
		t.Error("应在区间内")
	}
	if v := decide(r, price.New(148), no); v.hit || v.clear { // 距 1.33%：不在区间；clear 需 ≥ 1.00% → 应为 true
		if v.hit {
			t.Error("不应命中")
		}
	}
	if v := decide(r, price.New(148), no); !v.clear {
		t.Error("离开 2 倍距离应可复位")
	}
}

// ATR 出现在 fire 的邮件里，且 Status 快照里有 ATR 字段。
func TestFireIncludesATR(t *testing.T) {
	v := newEnv(t)
	// 造 35 根 1m K 线（H/L 有跨度），让 5m ATR(5) 可算。
	// 35 根连续分钟在任何墙钟对齐下都跨 7~8 个 5m 桶；实时根所在的末桶被丢弃后仍剩 6~7 个（≥ n+1=6）。
	// （只用 30 根时"剩 6 个桶"仅在墙钟分钟 mod 5 ≠ 4 时成立，会随机失败。）
	now := time.Now().UnixMilli()
	now -= now % 60000
	for i := 0; i < 35; i++ {
		p := price.New(float64(i) + 100)
		h := price.New(float64(i) + 102)
		l := price.New(float64(i) + 98)
		v.src.Feed("BTCUSDT", source.Kline{
			OpenTime: now - int64(34-i)*60000,
			Open:     p, High: h, Low: l, Close: p,
		})
	}
	v.rl.Add(rules.Rule{Symbol: "BTCUSDT", Trigger: rules.TrigTarget, Dir: "above", Target: "100", CooldownMin: 30})
	v.feed("BTCUSDT", 130)
	v.eval()
	if v.out.n() != 1 {
		t.Fatalf("应触发 1 封邮件: %d", v.out.n())
	}
	// 邮件文本里应有 ATR 行
	msg := v.out.last()
	if !strings.Contains(msg.Text, "ATR") {
		t.Errorf("邮件应含 ATR 行:\n%s", msg.Text)
	}
	// HTML 卡片里也应有 ATR
	if !strings.Contains(msg.Card, "ATR") {
		t.Error("HTML 卡片应含 ATR 行")
	}
	// Status 快照里应有 ATR 数据
	st := v.e.Status()
	if len(st.Symbols) == 0 {
		t.Fatal("Status 应有 symbol")
	}
	sym := st.Symbols[0]
	if len(sym.ATRs) != 3 {
		t.Errorf("应有 3 个 ATR 周期（5m/15m/1h），得 %d", len(sym.ATRs))
	}
	if !sym.ATRs[0].OK || sym.ATRs[0].Label != "5m" {
		t.Errorf("5m ATR 应 OK 且标签正确: %+v", sym.ATRs[0])
	}
}
