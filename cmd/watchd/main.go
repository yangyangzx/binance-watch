// watchd：币价哨兵 daemon。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"binance-watch/internal/config"
	"binance-watch/internal/engine"
	"binance-watch/internal/notify"
	"binance-watch/internal/recipients"
	"binance-watch/internal/rules"
	"binance-watch/internal/server"
	"binance-watch/internal/source"
)

var version = "dev"

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// dryRun 只打印将要发送的邮件，不真发（方便调阈值）。
type dryRun struct{}

func (dryRun) SendMessage(m notify.Message) error {
	log.Printf("dry-run: 邮件（未发送）\n  主题: %s\n%s", m.Subject, indent(m.Text))
	return nil
}

func indent(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("  | " + l + "\n")
	}
	return b.String()
}

func main() {
	home, _ := os.UserHomeDir()
	envPath := flag.String("env", envOr("BW_ENV", filepath.Join(home, "script", ".env")), ".env 文件路径（$BW_ENV）")
	proxy := flag.String("proxy", envOr("BW_PROXY", "http://127.0.0.1:7890"), "代理地址，空 = 直连（$BW_PROXY）")
	dataDir := flag.String("data", envOr("BW_DATA", filepath.Join(home, "go", "binance-watch", "data")), "数据目录（$BW_DATA）")
	web := flag.String("web", envOr("BW_WEB", "127.0.0.1:8090"), "本机 Web 面板监听地址（仅限回环地址），空 = 关闭（$BW_WEB）")
	testMail := flag.Bool("test-mail", false, "发一封测试邮件后退出")
	dry := flag.Bool("dry-run", false, "触发时只打印邮件内容，不真发")
	showVer := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVer {
		fmt.Println("watchd", version)
		return
	}
	log.SetFlags(log.LstdFlags)

	cfg, err := config.Load(*envPath, *proxy, *dataDir)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	mailer := notify.NewMailer(cfg)
	// 收件人可在 Web 面板修改，独立于 .env；Mailer 每封邮件都读最新值，无需重启。
	rs, err := recipients.New(*dataDir, cfg.EmailRecipients)
	if err != nil {
		log.Fatalf("recipients: %v", err)
	}
	mailer.SetRecipients(rs)
	// 发送历史落盘（含失败），失败仅告警不影响发信。
	hist, err := notify.OpenSendHistory(*dataDir)
	if err != nil {
		log.Printf("mail: 发送历史不可用（不影响发信）: %v", err)
	} else {
		defer hist.Close()
		mailer.SetHistory(hist)
		log.Printf("mail: 发送历史 → %s", hist.Path())
	}

	if *testMail {
		a := notify.Alert{Kind: "info", Title: "测试邮件 · SMTP 配置可用", At: time.Now(), Lines: []string{
			fmt.Sprintf("SMTP：%s:%d（SSL=%v）", cfg.SMTPHost, cfg.SMTPPort, cfg.SSLEnabled),
			"发件人：" + cfg.EmailUser,
			"收件人：" + strings.Join(rs.List(), ", "),
		}}
		if err := mailer.SendMessage(a.Message("[binance-watch] test")); err != nil {
			log.Fatalf("test mail: %v", err)
		}
		log.Printf("测试邮件已发送")
		return
	}

	rl, err := rules.New(*dataDir)
	if err != nil {
		log.Fatalf("rules: %v", err)
	}
	src, err := source.New(cfg)
	if err != nil {
		log.Fatalf("source: %v", err)
	}

	var sender notify.Sender = mailer
	if *dry {
		sender = dryRun{}
		log.Printf("watchd: dry-run 模式，不会真正发邮件")
	}
	nt := notify.NewNotifier(sender, time.Duration(cfg.EmailBatchWindowSec)*time.Second, cfg.EmailRateLimitPerMin)
	eng := engine.New(cfg, src, rl, nt)

	proxyShow := cfg.ProxyURL
	if proxyShow == "" {
		proxyShow = "直连"
	}
	log.Printf("watchd %s: 基础交易对=%v 代理=%s 数据=%s 规则=%d", version, cfg.Symbols, proxyShow, *dataDir, len(rl.List()))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	src.OnInvalid = func(sym string) {
		a := notify.Alert{Kind: "warn", Symbol: sym, Title: "交易对不存在", At: time.Now(),
			Lines: []string{"Binance 现货没有这个交易对，已停止订阅；涉及它的规则永远不会触发。", "请检查拼写（例如 BTCUSDT），用 `watch rm <id>` 删除对应规则。"}}
		nt.Enqueue(a.Message("⚠️ [binance-watch] 交易对 " + sym + " 不存在"))
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); nt.Run(ctx) }() // ctx 结束后会把队列里剩余邮件尽力发完

	if *web != "" {
		// dry-run 下不暴露"发测试邮件"接口，避免前端误以为真的发了。
		var mailAPI notify.Sender
		if !*dry {
			mailAPI = mailer
		}
		srv, err := server.New(*web, eng, rl, rs, mailAPI)
		if err != nil {
			log.Fatalf("web: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Run(ctx); err != nil {
				log.Printf("web: 面板退出: %v（不影响提醒功能）", err)
			}
		}()
	}

	src.Start(ctx)
	go func() { // 第一次信号开始优雅退出；再来一次就立即强退
		<-ctx.Done()
		stop()
		log.Printf("watchd: 正在退出（再按一次 Ctrl-C 强制退出）…")
	}()
	eng.Run(ctx)
	wg.Wait()
	log.Printf("watchd: 已退出")
}
