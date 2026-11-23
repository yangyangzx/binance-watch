package engine

import (
	"binance-watch/internal/price"
	"binance-watch/internal/rules"
)

const (
	// 价格类规则复位的滞回幅度：触发后价格必须回到目标另一侧 0.30% 之外才允许下一次触发，
	// 防止价格贴着阈值来回抖动而反复提醒。
	rearmMarginBP = 30
	// 涨跌幅类：回落到阈值的一半以下才复位。
	rearmPctRatio = 2
	// near 类：离开到 2 倍触发距离之外才复位。
	rearmNearMul = 2
)

// changeFn 返回最近 minutes 分钟的涨跌幅（bp）；ok=false 表示数据不足。
type changeFn func(minutes int) (bp int64, ok bool)

// verdict 一次判定结果。
type verdict struct {
	ok    bool  // 能否判定（数据不足时为 false，调用方应跳过而不是当作未触发）
	hit   bool  // 当前满足触发条件
	clear bool  // 当前已离开足够远，可以复位
	val   int64 // 展示用：pct=窗口涨跌 bp；target/near=相对目标的偏离 bp（有符号：价格-目标）
}

// decide 纯函数：给定规则/现价/涨跌幅数据，判断是否命中与是否可复位。
func decide(r *rules.Rule, p price.Price, change changeFn) verdict {
	switch r.Trigger {
	case rules.TrigTarget:
		t, err := r.TargetPrice()
		if err != nil || t <= 0 {
			return verdict{}
		}
		dev := price.PctBP(p, t) // 现价相对目标
		v := verdict{ok: true, val: dev}
		switch r.Dir {
		case "above":
			v.hit = p >= t
			v.clear = dev <= -rearmMarginBP
		case "below":
			v.hit = p <= t
			v.clear = dev >= rearmMarginBP
		default:
			return verdict{}
		}
		return v

	case rules.TrigNear:
		t, err := r.TargetPrice()
		if err != nil || t <= 0 || r.NearBP <= 0 {
			return verdict{}
		}
		dev := price.PctBP(p, t)
		d := dev
		if d < 0 {
			d = -d
		}
		return verdict{ok: true, val: dev, hit: d <= int64(r.NearBP), clear: d >= int64(r.NearBP)*rearmNearMul}

	case rules.TrigPct:
		pct, ok := change(r.WindowMin)
		if !ok || r.PctBP <= 0 {
			return verdict{}
		}
		th := int64(r.PctBP)
		half := th / rearmPctRatio
		v := verdict{ok: true, val: pct}
		switch r.Dir {
		case "up":
			v.hit, v.clear = pct >= th, pct < half
		case "down":
			v.hit, v.clear = pct <= -th, pct > -half
		default: // auto
			abs := pct
			if abs < 0 {
				abs = -abs
			}
			v.hit, v.clear = abs >= th, abs < half
		}
		return v
	}
	return verdict{}
}
