package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	content := "\ufeff# comment\n" +
		"SMTP_SERVER = \"smtp.qq.com\"\n" +
		"export SMTP_PORT=465 # inline\n" +
		"EMAIL_USER='a@qq.com'\n" +
		"EMAIL_PASS=pa#ss\n" + // # 前无空格 → 保留
		"EMAIL_TO=x@outlook.com; y@outlook.com\n" +
		"SSL_ENABLE=True\n" +
		"WATCH_SYMBOLS=btcusdt, ethusdt;BTCUSDT bad!\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.SMTPHost != "smtp.qq.com" || c.SMTPPort != 465 || !c.SSLEnabled {
		t.Errorf("smtp: %+v", c)
	}
	if c.EmailPass != "pa#ss" {
		t.Errorf("pass=%q", c.EmailPass)
	}
	if len(c.EmailRecipients) != 2 {
		t.Errorf("recipients=%v", c.EmailRecipients)
	}
	if len(c.Symbols) != 2 || c.Symbols[0] != "BTCUSDT" || c.Symbols[1] != "ETHUSDT" {
		t.Errorf("symbols=%v", c.Symbols)
	}
}

func TestNormalizeHost(t *testing.T) {
	if got := normalizeHost("http://smtp.qq.com:587"); got != "smtp.qq.com" {
		t.Errorf("got %q", got)
	}
}
