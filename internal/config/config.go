// Package config 解析 .env（兼容 python-dotenv 写法）并生成运行配置。
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Config 程序全部配置。
type Config struct {
	// Binance
	APIBase   string // https://api.binance.com
	StreamURL string // wss://stream.binance.com:9443
	ProxyURL  string // http://127.0.0.1:7890（空 = 直连，且不读取环境变量代理）
	Symbols   []string

	// SMTP（沿用 ~/script/.env 的命名）
	SMTPHost             string
	SMTPPort             int
	EmailUser            string
	EmailPass            string
	EmailTo              string   // 原始字符串
	EmailRecipients      []string // 拆分后的收件人
	SSLEnabled           bool     // true=465 直连 TLS；false=587 STARTTLS
	EmailRateLimitPerMin int
	EmailBatchWindowSec  int // 多条提醒合并为一封的时间窗

	// 运行
	WatchDir       string // 数据目录（rules.json 在这里）
	HealthStaleSec int    // 行情超过 N 秒没更新视为 STALE
}

var symbolRe = regexp.MustCompile(`^[A-Z0-9]{4,24}$`)

// ValidSymbol 校验交易对写法（如 BTCUSDT）。
func ValidSymbol(s string) bool { return symbolRe.MatchString(s) }

// Load 从 .env 文件 + CLI 参数加载。
func Load(envPath, proxy, watchDir string) (*Config, error) {
	env, err := parseEnv(envPath)
	if err != nil {
		return nil, fmt.Errorf("读取 .env 失败: %w", err)
	}
	c := &Config{
		APIBase:              strings.TrimRight(getStr(env, "BINANCE_API_BASE", "https://api.binance.com"), "/"),
		StreamURL:            strings.TrimRight(getStr(env, "BINANCE_STREAM_URL", "wss://stream.binance.com:9443"), "/"),
		ProxyURL:             strings.TrimSpace(proxy),
		WatchDir:             watchDir,
		HealthStaleSec:       getInt(env, "HEALTH_STALE_SEC", 90),
		EmailRateLimitPerMin: getInt(env, "EMAIL_RATE_LIMIT_PER_MIN", 30),
		EmailBatchWindowSec:  getInt(env, "EMAIL_BATCH_WINDOW_SEC", 5),
		SMTPPort:             getInt(env, "SMTP_PORT", 587),
	}
	c.Symbols = splitSymbols(getStr(env, "WATCH_SYMBOLS", "BTCUSDT,ETHUSDT,SOLUSDT,DOTUSDT"))
	c.SMTPHost = normalizeHost(getStr(env, "SMTP_SERVER", ""))
	c.EmailUser = getStr(env, "EMAIL_USER", "")
	c.EmailPass = getStr(env, "EMAIL_PASS", "")
	c.EmailTo = getStr(env, "EMAIL_TO", "")
	c.EmailRecipients = splitList(c.EmailTo)
	c.SSLEnabled = parseBool(getStr(env, "SSL_ENABLE", "false"))
	if c.SMTPPort == 465 {
		c.SSLEnabled = true
	}
	if c.HealthStaleSec < 15 {
		c.HealthStaleSec = 15
	}
	if c.EmailRateLimitPerMin < 1 {
		c.EmailRateLimitPerMin = 1
	}
	if c.EmailBatchWindowSec < 0 {
		c.EmailBatchWindowSec = 0
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.WatchDir == "" {
		return errors.New("数据目录未设置")
	}
	if c.SMTPHost == "" || c.EmailUser == "" || len(c.EmailRecipients) == 0 {
		return errors.New("SMTP/邮件配置不完整（需 SMTP_SERVER EMAIL_USER EMAIL_PASS EMAIL_TO）")
	}
	return nil
}

// parseEnv 兼容 python-dotenv：`KEY = "value"`、`export KEY=value`、行内 ` # 注释`。
func parseEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff") // UTF-8 BOM
			first = false
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		out[k] = parseValue(v)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseValue 处理引号与未加引号时的行内注释。
func parseValue(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		q := v[0]
		if end := strings.IndexByte(v[1:], q); end >= 0 {
			return v[1 : 1+end]
		}
		return v[1:]
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func getStr(m map[string]string, k, def string) string {
	if v, ok := m[k]; ok && v != "" {
		return v
	}
	return def
}

func getInt(m map[string]string, k string, def int) int {
	if v, ok := m[k]; ok && v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}

// normalizeHost 把 "http://smtp.qq.com:587" 归一化成 "smtp.qq.com"。
func normalizeHost(s string) string {
	for _, p := range []string{"http://", "https://", "smtp://", "smtps://"} {
		s = strings.TrimPrefix(s, p)
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(strings.TrimLeft(s, "."))
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitSymbols(s string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range splitList(s) {
		p = strings.ToUpper(p)
		if ValidSymbol(p) && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
