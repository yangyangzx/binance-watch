package price

import "testing"

func TestParseExact(t *testing.T) {
	cases := map[string]Price{
		"64992.00000000": 6499200000000,
		"0.29":           29_000_000, // float 会得到 28999999
		"0.00001234":     1234,
		"1":              100_000_000,
		".5":             50_000_000,
		"1.123456789":    112_345_678, // 超长小数截断
		"1e-3":           100_000,
	}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %d,%v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "abc", "1.2.3", ".", "NaN"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestPctBP(t *testing.T) {
	if got := PctBP(New(103), New(100)); got != 300 {
		t.Errorf("up: got %d want 300", got)
	}
	if got := PctBP(New(97), New(100)); got != -300 {
		t.Errorf("down: got %d want -300", got)
	}
	if got := PctBP(New(100), New(100)); got != 0 {
		t.Errorf("flat: got %d want 0", got)
	}
	if got := PctBP(New(1), 0); got != 0 {
		t.Errorf("prev=0: got %d want 0", got)
	}
	// 大价格不溢出
	if got := PctBP(New(2_000_000), New(1_000_000)); got != 10000 {
		t.Errorf("big: got %d want 10000", got)
	}
}

func TestPctString(t *testing.T) {
	if got := PctString(300); got != "3.00%" {
		t.Errorf("300bp: got %s", got)
	}
	if got := PctString(-50); got != "-0.50%" {
		t.Errorf("-50bp: got %s", got)
	}
	if got := PctSigned(125); got != "+1.25%" {
		t.Errorf("signed: got %s", got)
	}
}

func TestFormat(t *testing.T) {
	cases := map[float64]string{
		64992.1:    "64,992.10",
		999999.999: "1,000,000.00",
		148.325:    "148.3250",
		0.00001234: "0.00001234",
		0.5:        "0.5",
		1000:       "1,000.00",
	}
	for in, want := range cases {
		if got := New(in).Format(); got != want {
			t.Errorf("Format(%v) = %q want %q", in, got, want)
		}
	}
}
