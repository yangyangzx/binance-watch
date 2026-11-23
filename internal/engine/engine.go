// Package engine 评估循环：判定规则、发提醒、监控行情健康、同步订阅集合。
package engine

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"binance-watch/internal/config"
	"binance-watch/internal/notify"
	"binance-watch/internal/price"
	"binance-watch/internal/rules"
	"binance-watch/internal/source"
)

const (
	evalEvery      = 2 * time.Second
	healthEvery    = 15 * time.Second
	summaryEvery   = 10 * time.Minute // 日志里的汇总行（SD 卡友好：不再 30s 一行）
	minCooldown    = time.Minute      // 任何规则的最小冷却，兜底防止邮件风暴
	symbolStaleMin = 5 * time.Minute  // 单个交易对停更多久后单独告警
	recoverChecks  = 2                // 连续几次健康检查正常才算恢复，避免抖动
)

// Enqueuer 抽象发送队列，便于测试。
type Enqueuer interface{ Enqueue(notify.Message) }

// Engine 主循环。
type Engine struct {
	cfg  *config.Config
	src  *source.Source
	rl   *rules.Store
	out  Enqueuer
	now  func() time.Time
	boot time.Time

	ver     uint64 // 已同步的规则集版本（仅 Run 协程访问）
	lastSig string // 上次日志里的规则概况，避免重复刷日志

	mu    sync.RWMutex
	state map[string]*ruleState // 规则 ID → 运行时状态（不落盘）
	skip  map[string]time.Time  // 规则 ID → 上次"数据不足"日志时间（限频）

	// 健康状态（仅 health 协程写；Status 读）
	hmu       sync.RWMutex
	down      bool
	downSince time.Time
	okStreak  int
	symDown   map[string]time.Time // 单个交易对停更起点（已告警的）
	symState  map[string]string
	lastSum   time.Time
}

type ruleState struct{ armed bool }

// New 创建引擎。
func New(cfg *config.Config, src *source.Source, rl *rules.Store, out Enqueuer) *Engine {
	return &Engine{
		cfg: cfg, src: src, rl: rl, out: out,
		now: time.Now, boot: time.Now(),
		state:    map[string]*ruleState{},
		skip:     map[string]time.Time{},
		symDown:  map[string]time.Time{},
		symState: map[string]string{},
	}
}

func (e *Engine) staleDur() time.Duration { return time.Duration(e.cfg.HealthStaleSec) * time.Second }

// Run 阻塞运行直到 ctx 结束。
func (e *Engine) Run(ctx context.Context) {
	e.syncRules(true)
	tick := time.NewTicker(evalEvery)
	defer tick.Stop()
	health := time.NewTicker(healthEvery)
	defer health.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.syncRules(false)
			e.evaluate()
		case <-health.C:
			e.healthCheck()
		}
	}
}

// syncRules 热加载 rules.json，并按"基础交易对 ∪ 激活规则的交易对"调整行情订阅。
func (e *Engine) syncRules(first bool) {
	if _, err := e.rl.Refresh(); err != nil {
		log.Printf("rules: 重新加载失败（继续使用旧规则）: %v", err)
	}
	if v := e.rl.Version(); !first && v == e.ver {
		return
	} else {
		e.ver = v
	}
	list := e.rl.List()
	syms := append([]string(nil), e.cfg.Symbols...)
	active := 0
	live := map[string]bool{}
	for _, r := range list {
		live[r.ID] = true
		if r.Active {
			active++
			syms = append(syms, r.Symbol)
		}
	}
	e.src.SetSymbols(syms)
	e.mu.Lock()
	for id := range e.state { // 清理已删除规则的状态
		if !live[id] {
			delete(e.state, id)
			delete(e.skip, id)
		}
	}
	e.mu.Unlock()
	if sig := fmt.Sprintf("%d/%d/%d", len(list), active, len(e.src.Symbols())); sig != e.lastSig {
		e.lastSig = sig
		log.Printf("rules: 共 %d 条规则（%d 条激活），订阅 %d 个交易对", len(list), active, len(e.src.Symbols()))
	}
}

// evaluate 评估所有激活规则。
func (e *Engine) evaluate() {
	now := e.now()
	list := e.rl.List()
	for i := range list {
		r := &list[i]
		if !r.Active {
			e.mu.Lock()
			delete(e.state, r.ID) // 停用后重新启用时重新初始化
			e.mu.Unlock()
			continue
		}
		q := e.src.Quote(r.Symbol)
		if !q.OK || now.Sub(q.At) > e.staleDur() {
			continue // 没数据/数据过期：不拿旧价格做判断，断流由 healthCheck 负责告警
		}
		change := func(min int) (int64, bool) { return e.src.Change(r.Symbol, min) }
		v := decide(r, q.Price, change)
		if !v.ok {
			e.noteInsufficient(r, now)
			continue
		}

		e.mu.Lock()
		st := e.state[r.ID]
		if st == nil {
			// 首次见到这条规则。若它以前触发过、此刻条件仍满足 → 视为"已提醒过"，等复位后再响，
			// 避免 daemon 重启后对同一个行情重复轰炸。
			st = &ruleState{armed: r.LastHitAt.IsZero() || !v.hit}
			e.state[r.ID] = st
		}
		fire := false
		switch {
		case v.hit && st.armed && e.cooldownOK(r, now):
			st.armed = false
			fire = true
		case !v.hit && !st.armed && v.clear:
			st.armed = true
		}
		e.mu.Unlock()

		if fire {
			e.fire(r, q.Price, v, now)
		}
	}
}

func (e *Engine) cooldownOK(r *rules.Rule, now time.Time) bool {
	if r.LastHitAt.IsZero() {
		return true
	}
	cd := time.Duration(r.CooldownMin) * time.Minute
	if cd < minCooldown {
		cd = minCooldown
	}
	return now.Sub(r.LastHitAt) >= cd
}

func (e *Engine) noteInsufficient(r *rules.Rule, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if last, ok := e.skip[r.ID]; ok && now.Sub(last) < 30*time.Minute {
		return
	}
	e.skip[r.ID] = now
	log.Printf("rules: %s %s 暂时无法判定（K 线数据不足，已有 %d 根）", r.Symbol, r.ID, e.src.Bars(r.Symbol))
}

// windows 邮件里展示的涨跌幅窗口。
var windows = []struct {
	label string
	min   int
}{{"5m", 5}, {"15m", 15}, {"1h", 60}, {"4h", 240}}

func (e *Engine) changes(sym string) []notify.Change {
	out := make([]notify.Change, 0, len(windows))
	for _, w := range windows {
		bp, ok := e.src.Change(sym, w.min)
		out = append(out, notify.Change{Label: w.label, BP: bp, OK: ok})
	}
	return out
}

// atrs 邮件/面板展示的 ATR（价格单位，bp 相对现价）：ATR(5m)=5根、ATR(15m/1h)=14根。
func (e *Engine) atrs(sym string) []notify.ATRV {
	out := make([]notify.ATRV, 0, len(source.ATRPeriods))
	for _, p := range source.ATRPeriods {
		v, ok := e.src.ATR(sym, p.Min, p.N)
		out = append(out, notify.ATRV{Label: p.Label, N: p.N, Price: v, OK: ok})
	}
	return out
}

func (e *Engine) fire(r *rules.Rule, p price.Price, v verdict, now time.Time) {
	if err := e.rl.MarkHit(r.ID, now); err != nil {
		log.Printf("rules: 记录命中失败（仍会发送提醒）: %v", err)
	}
	a := notify.Alert{
		Kind: "hit", Symbol: r.Symbol,
		Title: fmt.Sprintf("%s · %s", r.TypeLabel(), r.Describe()),
		Price: p.Format(), Changes: e.changes(r.Symbol), ATRs: e.atrs(r.Symbol),
		RuleID: r.ID, Flags: r.Flags(), At: now,
	}
	switch r.Trigger {
	case rules.TrigTarget:
		a.Lines = append(a.Lines, fmt.Sprintf("目标 %s，现价已%s %s", fmtTarget(r), overUnder(v.val), price.PctString(abs64(v.val))))
	case rules.TrigNear:
		a.Lines = append(a.Lines, fmt.Sprintf("目标 %s，现价距目标 %s", fmtTarget(r), price.PctString(abs64(v.val))))
	case rules.TrigPct:
		a.Lines = append(a.Lines, fmt.Sprintf("%s窗口涨跌 %s（阈值 %s）", r.WindowLabel(), price.PctSigned(v.val), price.PctString(int64(r.PctBP))))
	}
	if r.Note != "" {
		a.Lines = append(a.Lines, "备注：", r.Note)
	}
	subject := fmt.Sprintf("🔔 %s %s · 现价 %s", r.Symbol, r.Describe(), p.Format())
	log.Printf("alert: %s %s %s → %s", r.ID, r.Symbol, r.Describe(), p.Format())
	e.out.Enqueue(a.Message(subject))
}

func fmtTarget(r *rules.Rule) string {
	if p, err := r.TargetPrice(); err == nil {
		return p.Format()
	}
	return r.Target
}

func overUnder(dev int64) string {
	if dev >= 0 {
		return "高于目标"
	}
	return "低于目标"
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ---------------------------------------------------------------- 健康检查

func (e *Engine) watched() []string {
	var out []string
	for _, s := range e.src.Symbols() {
		if !e.src.IsInvalid(s) {
			out = append(out, s)
		}
	}
	return out
}

// healthCheck 判断全局断流/恢复，以及单个交易对停更。
func (e *Engine) healthCheck() {
	now := e.now()
	syms := e.watched()
	e.hmu.Lock()
	defer e.hmu.Unlock()

	freshN := 0
	var latest time.Time
	var lines []string
	for _, sym := range syms {
		q := e.src.Quote(sym)
		state := "ok"
		switch {
		case !q.OK:
			state = "nodata"
		case now.Sub(q.At) > e.staleDur():
			state = "stale"
		default:
			freshN++
		}
		if q.At.After(latest) {
			latest = q.At
		}
		if prev := e.symState[sym]; prev != state {
			if prev != "" || state != "ok" {
				log.Printf("health: %s %s → %s", sym, prev, state)
			}
			e.symState[sym] = state
		}
		age := "--"
		if !q.At.IsZero() {
			age = fmt.Sprintf("%ds", int(now.Sub(q.At).Seconds()))
		}
		lines = append(lines, fmt.Sprintf("%s=%s(%s)", sym, q.Price.Format(), age))
	}
	if now.Sub(e.lastSum) >= summaryEvery {
		e.lastSum = now
		st := e.src.Stats()
		log.Printf("health: ws=%v 重连=%d 待发邮件=%d | %s", st.WSConnected, st.Reconnects, e.pending(), strings.Join(lines, " "))
	}

	allStale := len(syms) > 0 && freshN == 0
	// 启动宽限：刚启动、还没数据时不立刻报；超过一个 stale 周期 + 30s 仍无数据才算断流
	if latest.IsZero() && now.Sub(e.boot) < e.staleDur()+30*time.Second {
		allStale = false
	}

	switch {
	case allStale && !e.down:
		e.down = true
		e.okStreak = 0
		e.downSince = latest
		if e.downSince.IsZero() {
			e.downSince = e.boot
		}
		e.sendDown(now, latest)
	case !allStale && e.down && freshN > 0:
		e.okStreak++
		if e.okStreak >= recoverChecks {
			dur := now.Sub(e.downSince).Round(time.Second)
			e.down = false
			e.okStreak = 0
			log.Printf("health: 行情恢复（断流约 %s）", dur)
			a := notify.Alert{Kind: "ok", Title: "行情数据已恢复",
				Lines: []string{fmt.Sprintf("断流持续约 %s（%s → %s）", dur, e.downSince.Format("15:04:05"), now.Format("15:04:05"))},
				At:    now}
			e.out.Enqueue(a.Message("✅ [binance-watch] 行情已恢复"))
		}
	case allStale:
		e.okStreak = 0
	}

	// 单个交易对停更（整体没断流时才单独报，避免与全局告警重复）
	if !e.down {
		for _, sym := range syms {
			q := e.src.Quote(sym)
			stale := q.OK && now.Sub(q.At) > symbolStaleMin
			since, alerted := e.symDown[sym]
			switch {
			case stale && !alerted:
				e.symDown[sym] = q.At
				a := notify.Alert{Kind: "warn", Symbol: sym, Title: "该交易对行情停更",
					Lines: []string{fmt.Sprintf("最后更新 %s（%s 前），其它交易对仍正常。", q.At.Format("15:04:05"), now.Sub(q.At).Round(time.Second)),
						"可能已下架/暂停交易，涉及它的规则不会触发。"}, At: now}
				e.out.Enqueue(a.Message(fmt.Sprintf("⚠️ [binance-watch] %s 行情停更", sym)))
			case !stale && alerted && q.OK && now.Sub(q.At) < e.staleDur():
				delete(e.symDown, sym)
				a := notify.Alert{Kind: "ok", Symbol: sym, Title: "该交易对行情已恢复",
					Lines: []string{fmt.Sprintf("停更约 %s。", now.Sub(since).Round(time.Second))}, At: now}
				e.out.Enqueue(a.Message(fmt.Sprintf("✅ [binance-watch] %s 行情已恢复", sym)))
			}
		}
	}
}

func (e *Engine) sendDown(now, latest time.Time) {
	st := e.src.Stats()
	last := "从未收到数据"
	if !latest.IsZero() {
		last = fmt.Sprintf("最后一次数据 %s（%s 前）", latest.Format("15:04:05"), now.Sub(latest).Round(time.Second))
	}
	proxy := e.cfg.ProxyURL
	if proxy == "" {
		proxy = "直连"
	}
	log.Printf("health: 所有交易对行情断流（%s），发送告警", last)
	a := notify.Alert{Kind: "warn", Title: "行情数据断流",
		Lines: []string{
			last,
			fmt.Sprintf("WebSocket：%s，累计重连 %d 次；REST 兜底也未能取到数据", map[bool]string{true: "已连接但无数据", false: "未连接"}[st.WSConnected], st.Reconnects),
			"代理：" + proxy,
			"请检查 clash/mihomo 与网络，以及 binance-watch 服务日志（journalctl -u binance-watch）。",
		}, At: now}
	e.out.Enqueue(a.Message("🚨 [binance-watch] 行情数据断流"))
}

func (e *Engine) pending() int {
	if p, ok := e.out.(interface{ Pending() int }); ok {
		return p.Pending()
	}
	return 0
}

// ---------------------------------------------------------------- 状态快照

// ChangeStatus 某窗口涨跌幅。
type ChangeStatus struct {
	Label string `json:"label"`
	BP    int64  `json:"bp"`
	OK    bool   `json:"ok"`
}

// ATRStatus 某周期 ATR（价格单位 + 相对现价的百分比）。
type ATRStatus struct {
	Label string `json:"label"`  // "5m" / "15m" / "1h"
	N     int    `json:"n"`      // TR 平均根数
	Price string `json:"price"`  // ATR 价格单位（如 "312.4000"）
	PctBP int64  `json:"pct_bp"` // 相对现价基点
	OK    bool   `json:"ok"`
}

// SymbolStatus 交易对状态。
type SymbolStatus struct {
	Symbol  string         `json:"symbol"`
	Price   string         `json:"price"`
	PriceF  float64        `json:"price_f"`
	AgeSec  int            `json:"age_sec"`
	State   string         `json:"state"` // ok | stale | nodata | invalid（nodata = 从未有价，或有过价但已 4h 无数据）
	Changes []ChangeStatus `json:"changes"`
	ATRs    []ATRStatus    `json:"atrs"`
}

// RuleStatus 规则状态。
type RuleStatus struct {
	rules.Rule
	Desc  string `json:"desc"`
	Type  string `json:"type_label"`
	Flags string `json:"flags"`
	Armed bool   `json:"armed"`
	Gap   string `json:"gap"` // 当前离触发还有多远（展示用）
}

// Status 整体状态。
type Status struct {
	Now        time.Time      `json:"now"`
	UptimeSec  int64          `json:"uptime_sec"`
	WS         bool           `json:"ws"`
	WSSinceSec int64          `json:"ws_since_sec"`
	Reconnects int64          `json:"reconnects"`
	Down       bool           `json:"down"`
	Pending    int            `json:"pending_mail"`
	Proxy      string         `json:"proxy"`
	Symbols    []SymbolStatus `json:"symbols"`
	Rules      []RuleStatus   `json:"rules"`
}

// Status 生成快照（给 Web/CLI 展示）。
func (e *Engine) Status() Status {
	now := e.now()
	st := e.src.Stats()
	e.hmu.RLock()
	down := e.down
	e.hmu.RUnlock()
	out := Status{
		Now: now, UptimeSec: int64(now.Sub(e.boot).Seconds()), WS: st.WSConnected,
		Reconnects: st.Reconnects, Down: down, Pending: e.pending(), Proxy: e.cfg.ProxyURL,
	}
	if !st.WSSince.IsZero() && st.WSConnected {
		out.WSSinceSec = int64(now.Sub(st.WSSince).Seconds())
	}

	all := e.src.Symbols()
	sort.Strings(all)
	for _, sym := range all {
		ss := SymbolStatus{Symbol: sym, State: "nodata"}
		if e.src.IsInvalid(sym) {
			ss.State = "invalid"
		} else if q := e.src.Quote(sym); q.OK {
			ss.Price, ss.PriceF, ss.AgeSec = q.Price.Format(), q.Price.Float(), int(now.Sub(q.At).Seconds())
			ss.State = "ok"
			if now.Sub(q.At) > e.staleDur() {
				ss.State = "stale"
			}
		}
		for _, c := range e.changes(sym) {
			ss.Changes = append(ss.Changes, ChangeStatus{Label: c.Label, BP: c.BP, OK: c.OK})
		}
		for _, a := range e.atrs(sym) {
			as := ATRStatus{Label: a.Label, N: a.N, OK: a.OK}
			if a.OK {
				as.Price = a.Price.Format()
				if q := e.src.Quote(sym); q.OK {
					as.PctBP = price.PctBP(a.Price, q.Price)
				}
			}
			ss.ATRs = append(ss.ATRs, as)
		}
		out.Symbols = append(out.Symbols, ss)
	}

	for _, r := range e.rl.List() {
		rs := RuleStatus{Rule: r, Desc: r.Describe(), Type: r.TypeLabel(), Flags: r.Flags()}
		e.mu.RLock()
		if s := e.state[r.ID]; s != nil {
			rs.Armed = s.armed
		} else {
			rs.Armed = r.Active
		}
		e.mu.RUnlock()
		if q := e.src.Quote(r.Symbol); q.OK && r.Active {
			rr := r
			v := decide(&rr, q.Price, func(m int) (int64, bool) { return e.src.Change(r.Symbol, m) })
			if v.ok {
				rs.Gap = gapText(&rr, v)
			}
		}
		out.Rules = append(out.Rules, rs)
	}
	return out
}

// gapText 展示"现在离触发还差多少"。
func gapText(r *rules.Rule, v verdict) string {
	switch r.Trigger {
	case rules.TrigTarget:
		if v.hit {
			return "已满足"
		}
		return "距目标 " + price.PctString(abs64(v.val))
	case rules.TrigNear:
		if v.hit {
			return "已进入区间"
		}
		return "距目标 " + price.PctString(abs64(v.val))
	case rules.TrigPct:
		return fmt.Sprintf("当前 %s / 阈值 %s", price.PctSigned(v.val), price.PctString(int64(r.PctBP)))
	}
	return ""
}
