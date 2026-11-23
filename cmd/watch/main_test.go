package main

import "testing"

func TestParseBP(t *testing.T) {
	ok := map[string]int{"3%": 300, "3.5%": 350, "0.5%": 50, "300": 300, "": 0, " 1% ": 100,
		"2.675%": 268, "0.005%": 1, "100%": 10000, "99.994%": 9999, "99.995%": 10000}
	for in, want := range ok {
		if got, err := parseBP(in); err != nil || got != want {
			t.Errorf("parseBP(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "-1", "0", "0%", "-3%", "3.5", "0.004%", "0.001%"} {
		if _, err := parseBP(bad); err == nil {
			t.Errorf("parseBP(%q) 应报错", bad)
		}
	}
}

func TestParseMinutes(t *testing.T) {
	ok := map[string]int{"30": 30, "30m": 30, "2h": 120, "1d": 1440, "": 0, "4H": 240}
	for in, want := range ok {
		if got, err := parseMinutes(in); err != nil || got != want {
			t.Errorf("parseMinutes(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	if _, err := parseMinutes("x"); err == nil {
		t.Error("应报错")
	}
}

func TestWidthAndSplit(t *testing.T) {
	if width("到价") != 4 || width("abc") != 3 || width("≥ 1") != 3 {
		t.Error("width 计算错误")
	}
	pos, fl := splitLeading([]string{"BTCUSDT", "above", "70000", "--once", "--note", "x"})
	if len(pos) != 3 || len(fl) != 3 {
		t.Errorf("splitLeading: %v | %v", pos, fl)
	}
}
