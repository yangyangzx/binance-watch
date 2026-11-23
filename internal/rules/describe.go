package rules

import (
	"fmt"
	"strings"

	"binance-watch/internal/price"
)

func fmtTarget(s string) string {
	if p, err := price.Parse(s); err == nil {
		return p.Format()
	}
	return s
}

// Describe 人类可读的一句话描述（不含交易对）。
//
//	价格 ≥ 70,000
//	60 分钟内下跌 ≥ 3.00%
//	距 150.0000 不足 0.50%
func (r *Rule) Describe() string {
	switch r.Trigger {
	case TrigTarget:
		op := "≥"
		if r.Dir == "below" {
			op = "≤"
		}
		return fmt.Sprintf("价格 %s %s", op, fmtTarget(r.Target))
	case TrigPct:
		return fmt.Sprintf("%s内%s ≥ %s", r.WindowLabel(), r.DirLabel(), price.PctString(int64(r.PctBP)))
	case TrigNear:
		return fmt.Sprintf("距 %s 不足 %s", fmtTarget(r.Target), price.PctString(int64(r.NearBP)))
	}
	return r.Trigger
}

// WindowLabel "60 分钟" / "4 小时"
func (r *Rule) WindowLabel() string {
	w := r.EffectiveWindow()
	if w%60 == 0 {
		return fmt.Sprintf("%d 小时", w/60)
	}
	return fmt.Sprintf("%d 分钟", w)
}

// DirLabel 涨跌方向的中文。
func (r *Rule) DirLabel() string {
	switch r.Dir {
	case "down", "below":
		return "下跌"
	case "up", "above":
		return "上涨"
	}
	return "涨跌(双向)"
}

// TypeLabel 触发类型的中文。
func (r *Rule) TypeLabel() string {
	switch r.Trigger {
	case TrigTarget:
		return "到价"
	case TrigPct:
		return "涨跌幅"
	case TrigNear:
		return "接近"
	}
	return r.Trigger
}

// Flags 附加属性：once / 冷却。
func (r *Rule) Flags() string {
	var f []string
	if r.Once {
		f = append(f, "一次性")
	}
	if r.CooldownMin > 0 {
		f = append(f, fmt.Sprintf("冷却 %dm", r.CooldownMin))
	}
	return strings.Join(f, " · ")
}
