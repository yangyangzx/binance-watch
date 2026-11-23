package notify

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"binance-watch/internal/price"
)

// Change 一个时间窗口的涨跌幅。
type Change struct {
	Label string // "5m"
	BP    int64
	OK    bool
}

// ATRV 一个周期内的 ATR（价格单位）。
type ATRV struct {
	Label string // "5m" / "15m" / "1h"
	N     int    // TR 平均根数（ATR(n) 的 n）
	Price price.Price
	OK    bool
}

// Alert 邮件内容的结构化描述。
type Alert struct {
	Kind    string // hit | warn | ok | info
	Symbol  string
	Title   string // 例：到价 · 价格 ≥ 70,000
	Price   string // 已格式化的现价；系统消息可空
	Changes []Change
	ATRs    []ATRV
	Lines   []string // 附加说明行
	RuleID  string
	Flags   string
	At      time.Time
}

type cardView struct {
	Accent  string
	Badge   string
	Symbol  string
	Title   string
	Price   string
	Changes []changeView
	// ATR 行：「5m 1.25」= 该周期 K 线平均真实波幅（相对现价 %）
	ATRs   []atrView
	Lines  []string
	Footer string
}

type changeView struct{ Label, Text, Color string }

type atrView struct{ Label, Text, Color string }

var cardTpl = template.Must(template.New("card").Parse(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:520px;margin:0 auto 14px;background:#0f1419;border:1px solid #232b36;border-radius:12px;border-left:4px solid {{.Accent}};font-family:-apple-system,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;">
<tr><td style="padding:16px 20px 6px;">
<span style="display:inline-block;background:{{.Accent}};color:#0b0f14;font-size:11px;font-weight:700;padding:2px 8px;border-radius:4px;letter-spacing:.5px;">{{.Badge}}</span>
{{if .Symbol}}<span style="color:#e6edf3;font-size:18px;font-weight:700;margin-left:8px;">{{.Symbol}}</span>{{end}}
</td></tr>
<tr><td style="padding:2px 20px 10px;color:#9fb0c3;font-size:14px;">{{.Title}}</td></tr>
{{if .Price}}<tr><td style="padding:0 20px 8px;color:#ffffff;font-size:30px;font-weight:700;font-family:'SF Mono',Consolas,monospace;">{{.Price}}</td></tr>{{end}}
{{if .Changes}}<tr><td style="padding:2px 20px 10px;"><table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr>
{{range .Changes}}<td align="center" style="padding:8px 4px;background:#161d26;border-radius:6px;"><div style="color:#7d8da1;font-size:11px;">{{.Label}}</div><div style="color:{{.Color}};font-size:14px;font-weight:700;font-family:'SF Mono',Consolas,monospace;">{{.Text}}</div></td><td width="6"></td>{{end}}
</tr></table></td></tr>{{end}}
{{if .ATRs}}<tr><td style="padding:0 20px 10px;color:#5d6b7c;font-size:11px;"><span style="letter-spacing:.4px;">ATR</span>
{{range .ATRs}} <span style="display:inline-block;margin-left:8px;padding:2px 8px;background:#161d26;border-radius:4px;font-family:'SF Mono',Consolas,monospace;font-size:11px;color:#9fb0c3;">{{.Label}} <b style="color:{{.Color}};font-weight:600;">{{.Text}}</b></span>{{end}}
</td></tr>{{end}}
{{range .Lines}}<tr><td style="padding:0 20px 6px;color:#c9d4e0;font-size:13px;line-height:1.6;">{{.}}</td></tr>{{end}}
<tr><td style="padding:8px 20px 14px;color:#5d6b7c;font-size:11px;border-top:1px solid #1c242e;">{{.Footer}}</td></tr>
</table>`))

var pageTpl = template.Must(template.New("page").Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="color-scheme" content="dark light"></head><body style="margin:0;padding:18px 10px;background:#0b0f14;">{{.}}</body></html>`))

func accent(kind string) (color, badge string) {
	switch kind {
	case "warn":
		return "#ff5c5c", "告警"
	case "ok":
		return "#2ee59d", "恢复"
	case "info":
		return "#4da3ff", "通知"
	}
	return "#f0b90b", "提醒"
}

// Render 返回 纯文本 与 HTML 卡片片段。
func (a Alert) Render() (text string, card string) {
	var t strings.Builder
	if a.Symbol != "" {
		fmt.Fprintf(&t, "%s  %s\n", a.Symbol, a.Title)
	} else {
		fmt.Fprintf(&t, "%s\n", a.Title)
	}
	if a.Price != "" {
		fmt.Fprintf(&t, "现价  %s\n", a.Price)
	}
	if len(a.Changes) > 0 {
		var parts []string
		for _, c := range a.Changes {
			if c.OK {
				parts = append(parts, fmt.Sprintf("%s %s", c.Label, price.PctSigned(c.BP)))
			} else {
				parts = append(parts, fmt.Sprintf("%s --", c.Label))
			}
		}
		fmt.Fprintf(&t, "涨跌  %s\n", strings.Join(parts, "   "))
	}
	if len(a.ATRs) > 0 {
		var parts []string
		for _, a := range a.ATRs {
			if a.OK {
				parts = append(parts, fmt.Sprintf("%s %s", a.Label, atrPct(a.Price)))
			} else {
				parts = append(parts, fmt.Sprintf("%s --", a.Label))
			}
		}
		fmt.Fprintf(&t, "波动  ATR %s\n", strings.Join(parts, "   "))
	}
	for _, l := range a.Lines {
		fmt.Fprintf(&t, "%s\n", l)
	}
	foot := []string{a.At.Format("2006-01-02 15:04:05")}
	if a.RuleID != "" {
		foot = append(foot, "规则 "+a.RuleID)
	}
	if a.Flags != "" {
		foot = append(foot, a.Flags)
	}
	fmt.Fprintf(&t, "\n%s\n", strings.Join(foot, " · "))

	col, badge := accent(a.Kind)
	v := cardView{Accent: col, Badge: badge, Symbol: a.Symbol, Title: a.Title, Price: a.Price,
		Lines: a.Lines, Footer: strings.Join(foot, " · ")}
	for _, c := range a.Changes {
		cv := changeView{Label: c.Label, Text: "--", Color: "#7d8da1"}
		if c.OK {
			cv.Text = price.PctSigned(c.BP)
			switch {
			case c.BP > 0:
				cv.Color = "#2ee59d"
			case c.BP < 0:
				cv.Color = "#ff5c5c"
			default:
				cv.Color = "#c9d4e0"
			}
		}
		v.Changes = append(v.Changes, cv)
	}
	for _, a := range a.ATRs {
		av := atrView{Label: a.Label, Text: "--", Color: "#7d8da1"}
		if a.OK {
			av.Text = atrPct(a.Price)
			av.Color = "#4da3ff"
		}
		v.ATRs = append(v.ATRs, av)
	}
	var b bytes.Buffer
	if err := cardTpl.Execute(&b, v); err != nil {
		return t.String(), ""
	}
	return t.String(), b.String()
}

// atrPct 把 ATR（价格单位）格式化成人类可读数值：量级越大小数位越少（与 Price.Format 同风格）。
func atrPct(p price.Price) string { return p.Format() }

// Message 渲染成 Message（含 HTML 外壳）。
func (a Alert) Message(subject string) Message {
	text, card := a.Render()
	return Message{Tag: a.Symbol, Subject: subject, Text: text, Card: card}
}

// PageHTML 给（一张或多张）卡片片段包上完整 HTML 外壳（发送时统一包一次，摘要里多张卡片共用）。
func PageHTML(cards string) string {
	var b bytes.Buffer
	if err := pageTpl.Execute(&b, template.HTML(cards)); err != nil {
		return cards
	}
	return b.String()
}
