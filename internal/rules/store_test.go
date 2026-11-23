package rules

import (
	"os"
	"strings"
	"testing"
	"time"
)

func mk(sym string) Rule {
	return Rule{Symbol: sym, Trigger: TrigTarget, Dir: "above", Target: "70000", CooldownMin: 30}
}

func TestAddValidate(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bad := []Rule{
		{Symbol: "btc!", Trigger: TrigTarget, Dir: "above", Target: "1"},
		{Symbol: "BTCUSDT", Trigger: TrigTarget, Dir: "up", Target: "1"},
		{Symbol: "BTCUSDT", Trigger: TrigTarget, Dir: "above", Target: "0"},
		{Symbol: "BTCUSDT", Trigger: TrigTarget, Dir: "above", Target: "abc"},
		{Symbol: "BTCUSDT", Trigger: TrigNear, Target: "100"},             // 缺 near_bp
		{Symbol: "BTCUSDT", Trigger: TrigNear, Target: "0", NearBP: 50},   // 除零
		{Symbol: "BTCUSDT", Trigger: TrigPct, PctBP: 300, WindowMin: 241}, // 窗口过大
		{Symbol: "BTCUSDT", Trigger: TrigPct, PctBP: 0},                   // 缺阈值
		{Symbol: "BTCUSDT", Trigger: TrigTarget, Dir: "above", Target: "1", CooldownMin: -1},
	}
	for i, r := range bad {
		if _, err := s.Add(r); err == nil {
			t.Errorf("bad[%d] should fail: %+v", i, r)
		}
	}
	r, err := s.Add(Rule{Symbol: "ethusdt", Trigger: TrigPct, PctBP: 300, Dir: "below", WindowMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if r.Symbol != "ETHUSDT" || r.Dir != "down" || !r.Active || !strings.HasPrefix(r.ID, "r_") {
		t.Errorf("normalize: %+v", r)
	}
	if got := r.Describe(); got != "1 小时内下跌 ≥ 3.00%" {
		t.Errorf("describe: %q", got)
	}
}

func TestPersistAndPrefix(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	r, _ := s.Add(mk("BTCUSDT"))
	s2, err := New(dir)
	if err != nil || len(s2.List()) != 1 {
		t.Fatalf("reload: %v %v", err, s2.List())
	}
	if _, ok := s2.Get(r.ID[:4]); !ok {
		t.Error("prefix get failed")
	}
	if _, err := s2.Remove("zzz"); err == nil {
		t.Error("remove missing should fail")
	}
	if _, err := s2.Remove(r.ID); err != nil || len(s2.List()) != 0 {
		t.Errorf("remove: %v", err)
	}
}

// 两个 Store（模拟 CLI 与 daemon 两个进程）交替写，互不覆盖。
func TestNoLostUpdate(t *testing.T) {
	dir := t.TempDir()
	daemon, _ := New(dir)
	cli, _ := New(dir)
	a, _ := daemon.Add(mk("BTCUSDT"))
	if _, err := cli.Add(mk("ETHUSDT")); err != nil { // CLI 在 daemon 不知情时加了一条
		t.Fatal(err)
	}
	if err := daemon.MarkHit(a.ID, time.Now()); err != nil { // daemon 随后落盘命中记录
		t.Fatal(err)
	}
	got, _ := New(dir)
	if len(got.List()) != 2 {
		t.Fatalf("lost update: %d rules", len(got.List()))
	}
	if r, _ := got.Get(a.ID); r.HitCount != 1 {
		t.Errorf("hit not saved: %+v", r)
	}
}

func TestRefreshAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	daemon, _ := New(dir)
	cli, _ := New(dir)
	if ch, _ := daemon.Refresh(); ch {
		t.Error("no change expected")
	}
	cli.Add(mk("BTCUSDT"))
	if ch, err := daemon.Refresh(); !ch || err != nil || len(daemon.List()) != 1 {
		t.Fatalf("refresh: %v %v", ch, err)
	}
	if ch, _ := daemon.Refresh(); ch {
		t.Error("second refresh should be no-op")
	}
	// 手工改坏文件：保留旧规则，不 panic
	os.WriteFile(daemon.Path(), []byte("{not json"), 0o644)
	if _, err := daemon.Refresh(); err == nil {
		t.Error("corrupt should error")
	}
	if len(daemon.List()) != 1 {
		t.Error("old rules must be kept")
	}
}

func TestOnceDeactivates(t *testing.T) {
	s, _ := New(t.TempDir())
	r := mk("BTCUSDT")
	r.Once = true
	r, _ = s.Add(r)
	s.MarkHit(r.ID, time.Now())
	if got, _ := s.Get(r.ID); got.Active {
		t.Error("once rule should deactivate")
	}
	got, _ := s.SetActive(r.ID, true)
	if !got.Active || !got.LastHitAt.IsZero() {
		t.Errorf("re-enable should rearm: %+v", got)
	}
	if err := s.MarkHit("r_gone", time.Now()); err != nil {
		t.Errorf("MarkHit on missing rule should be silent: %v", err)
	}
}
