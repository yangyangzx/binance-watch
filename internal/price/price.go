// Package price 提供定点价格（int64 × 1e8）及涨跌幅（基点 bp）工具。
package price

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Scale 是定点放大倍数（8 位小数）。
const Scale = 100_000_000

// Price = int64，单位 = 币价 × 1e8。
type Price int64

// Parse 把 "64992.12000000" 这类十进制字符串精确解析为定点价格。
// 不经过 float64，避免 0.29*1e8 = 28999999.999… 这类截断误差；
// 超过 8 位的小数直接截断；遇到科学计数法时退回 ParseFloat（并四舍五入）。
func Parse(s string) (Price, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty price")
	}
	if strings.ContainsAny(s, "eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("bad price %q: %w", s, err)
		}
		return fromFloat(s, f)
	}
	if s[0] == '-' {
		return 0, fmt.Errorf("negative price %q", s)
	}
	s = strings.TrimPrefix(s, "+")
	intPart, frac, _ := strings.Cut(s, ".")
	if intPart == "" && frac == "" {
		return 0, fmt.Errorf("bad price %q", s)
	}
	if len(frac) > 8 {
		frac = frac[:8]
	}
	frac += strings.Repeat("0", 8-len(frac))
	var ip int64
	if intPart != "" {
		v, err := strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad price %q: %w", s, err)
		}
		ip = v
	}
	fp, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad price %q: %w", s, err)
	}
	if ip > (math.MaxInt64-fp)/Scale {
		return 0, fmt.Errorf("price overflow %q", s)
	}
	return Price(ip*Scale + fp), nil
}

// FromBinance 保留旧名字，等价于 Parse。
func FromBinance(s string) (Price, error) { return Parse(s) }

func fromFloat(src string, f float64) (Price, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("bad price %q", src)
	}
	if f < 0 {
		return 0, fmt.Errorf("negative price %q", src)
	}
	if f*Scale >= math.MaxInt64 {
		return 0, fmt.Errorf("price overflow %q", src)
	}
	return Price(math.Round(f * Scale)), nil
}

// New 从 float 构造（测试/展示用），四舍五入。
func New(v float64) Price { return Price(math.Round(v * Scale)) }

// Float 展示用。
func (p Price) Float() float64 { return float64(int64(p)) / Scale }

// PctBP = (cur-prev)/prev × 10000（万分之一，即基点）。1234 表示 12.34%，300 表示 3.00%。
// 用 float64 做除法并四舍五入，避免 int64 乘法在大价格上溢出。
func PctBP(cur, prev Price) int64 {
	if prev <= 0 {
		return 0
	}
	return int64(math.Round(float64(int64(cur)-int64(prev)) * 10000 / float64(int64(prev))))
}

// PctString 基点(bp, 1%=100bp) → "+1.23%" / "-0.50%"（带符号）。
func PctString(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%02d%%", sign, v/100, v%100)
}

// PctSigned 与 PctString 相同，但正数带 "+"。
func PctSigned(v int64) string {
	if v > 0 {
		return "+" + PctString(v)
	}
	return PctString(v)
}

// Format 按量级自适应小数位：
//
//	≥1000 → 2 位并带千分位   64,992.12
//	≥1    → 4 位            148.3250
//	<1    → 最多 8 位，去掉尾部 0   0.00001234
func (p Price) Format() string {
	f := p.Float()
	switch {
	case f >= 1000:
		return groupThousands(strconv.FormatFloat(f, 'f', 2, 64))
	case f >= 1:
		return strconv.FormatFloat(f, 'f', 4, 64)
	case f == 0:
		return "0"
	default:
		s := strconv.FormatFloat(f, 'f', 8, 64)
		s = strings.TrimRight(s, "0")
		return strings.TrimSuffix(s, ".")
	}
}

func groupThousands(s string) string {
	intPart, frac, hasFrac := strings.Cut(s, ".")
	n := len(intPart)
	if n <= 3 {
		return s
	}
	var b strings.Builder
	pre := n % 3
	if pre > 0 {
		b.WriteString(intPart[:pre])
	}
	for i := pre; i < n; i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(intPart[i : i+3])
	}
	if hasFrac {
		b.WriteByte('.')
		b.WriteString(frac)
	}
	return b.String()
}
