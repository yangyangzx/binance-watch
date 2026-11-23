package recipients

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedFallback(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, []string{"ops@qq.com;dev@outlook.com"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.List()
	if len(got) != 2 || got[0] != "ops@qq.com" || got[1] != "dev@outlook.com" {
		t.Fatalf("seed=%v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "mail.json")); !os.IsNotExist(err) {
		t.Error("seed 不应立即落盘，否则会覆盖用户在 .env 里的后续修改")
	}
}

func TestPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, []string{"seed@qq.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set([]string{"a@outlook.com, b@outlook.com"}); err != nil {
		t.Fatal(err)
	}
	got := s.List()
	if len(got) != 2 || got[0] != "a@outlook.com" || got[1] != "b@outlook.com" {
		t.Fatalf("set=%v", got)
	}
	// 新实例（模拟 daemon 重启）必须读到面板保存的值，而不是 seed。
	s2, err := New(dir, []string{"seed@qq.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got2 := s2.List(); len(got2) != 2 || got2[0] != "a@outlook.com" {
		t.Fatalf("reload=%v", got2)
	}
	fi, err := os.Stat(filepath.Join(dir, "mail.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("收件人文件权限应为 0600，实际 %v", fi.Mode().Perm())
	}
}

func TestListReturnsCopy(t *testing.T) {
	s, err := New(t.TempDir(), []string{"a@b.com"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.List()
	got[0] = "evil@x.com"
	if s.List()[0] != "a@b.com" {
		t.Error("List 必须返回副本，否则调用方可绕过锁篡改内部状态")
	}
}

func TestNormalizeRejectsInjection(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"not-an-email",
		"a@b.com\r\nBcc: evil@x.com",
		"\"a@b.com\" <evil@x.com>",
		"a@b.com, " + strings.Repeat("x", 300) + "@c.com",
	}
	for _, in := range bad {
		if _, err := Normalize([]string{in}); err == nil {
			t.Errorf("应拒绝 %q", in)
		}
	}
	got, err := Normalize([]string{"a@b.com; A@B.COM, c@d.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("大小写不敏感去重后应有 2 个: %v", got)
	}
}

func TestNormalizeMaxRecipients(t *testing.T) {
	var many []string
	for i := 0; i < maxRecipients+1; i++ {
		many = append(many, "u"+string(rune('a'+i))+"@x.com")
	}
	if _, err := Normalize(many); err == nil {
		t.Error("超过上限应拒绝")
	}
}
