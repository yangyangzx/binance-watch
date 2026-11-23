package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"binance-watch/internal/config"
	"binance-watch/internal/price"
)

func TestBuildMIME(t *testing.T) {
	msg := Message{Subject: "[BTCUSDT] 到价\r\nBcc: evil@x.com", Text: "你好 world", Card: "<b>卡片</b>"}
	raw, err := buildMIME("me@qq.com", []string{"a@outlook.com", "b@outlook.com"}, msg,
		time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), "smtp.qq.com")
	if err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("不是合法邮件: %v", err)
	}
	if m.Header.Get("Bcc") != "" {
		t.Error("主题里的换行必须被清理，否则存在头注入")
	}
	subj, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil || !strings.Contains(subj, "BTCUSDT") || !strings.Contains(subj, "到价") {
		t.Errorf("主题解码: %q %v", subj, err)
	}
	for _, k := range []string{"Date", "Message-Id", "Mime-Version", "From", "To"} {
		if m.Header.Get(k) == "" {
			t.Errorf("缺少头 %s", k)
		}
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content-type=%q %v", mt, err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	var kinds []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, p.Header.Get("Content-Type"))
	}
	if len(kinds) != 2 || !strings.HasPrefix(kinds[0], "text/plain") || !strings.HasPrefix(kinds[1], "text/html") {
		t.Errorf("parts=%v", kinds)
	}
	if _, err := buildMIME("me@qq.com\r\nX: y", []string{"a@b.c"}, msg, time.Now(), "h"); err == nil {
		t.Error("非法地址应拒绝")
	}
}

type fakeRecipients struct{ to []string }

func (f *fakeRecipients) List() []string { return append([]string(nil), f.to...) }

// 回归：面板改了收件人后，下一封邮件必须用新值；未注入或源为空时回退 .env 配置。
func TestMailerUsesRuntimeRecipients(t *testing.T) {
	cfg := &config.Config{
		SMTPHost: "smtp.example.com", SMTPPort: 587, EmailUser: "me@example.com",
		EmailRecipients: []string{"old@example.com"},
	}
	m := NewMailer(cfg)
	if got := m.recipients(); len(got) != 1 || got[0] != "old@example.com" {
		t.Fatalf("回退 .env 失败: %v", got)
	}
	rs := &fakeRecipients{to: []string{"new@example.com"}}
	m.SetRecipients(rs)
	if got := m.recipients(); len(got) != 1 || got[0] != "new@example.com" {
		t.Fatalf("应使用运行时收件人: %v", got)
	}
	rs.to = []string{"new2@example.com"}
	if got := m.recipients(); len(got) != 1 || got[0] != "new2@example.com" {
		t.Fatalf("面板改动应即时生效，无需重建 Mailer: %v", got)
	}
	rs.to = nil
	if got := m.recipients(); len(got) != 1 || got[0] != "old@example.com" {
		t.Fatalf("源为空应回退 .env: %v", got)
	}
}

func TestSendHistory(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenSendHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	fi, err := os.Stat(h.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("历史文件权限应为 0600，实际 %v", fi.Mode().Perm())
	}

	to := []string{"a@x.com", "b@x.com"}
	if err := h.Record(SendRecord{Subject: "第一封", To: to, OK: true}); err != nil {
		t.Fatal(err)
	}
	to[0] = "mutated@x.com" // Record 不应持有调用方的 slice
	if err := h.Record(SendRecord{Subject: "第二封", To: to, OK: false, Error: "auth failed"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(h.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 条记录，实际 %d: %s", len(lines), data)
	}
	var first, second SendRecord
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first.Subject != "第一封" || !first.OK || first.Time.IsZero() ||
		len(first.To) != 2 || first.To[0] != "a@x.com" {
		t.Errorf("首条记录: %+v", first)
	}
	if second.Subject != "第二封" || second.OK || second.Error != "auth failed" {
		t.Errorf("次条记录: %+v", second)
	}
}

func TestSendHistoryRotation(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenSendHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.SetRotation(260, 2) // 每条约 150B，每 1~2 条触发一次轮转

	const total = 12
	for i := 0; i < total; i++ {
		if err := h.Record(SendRecord{
			Subject: fmt.Sprintf("msg-%03d", i), To: []string{"a@x.com"}, OK: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 2; i++ {
		fi, err := os.Stat(h.archivePath(i))
		if err != nil {
			t.Fatalf("归档 %d 应存在: %v", i, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("归档 %d 权限应为 0600，实际 %v", i, fi.Mode().Perm())
		}
	}
	if _, err := os.Stat(h.archivePath(3)); !os.IsNotExist(err) {
		t.Fatalf("只保留 2 份归档，.3 不应存在: %v", err)
	}

	// 按 [.2 最老 → .1 → 当前] 读回，subject 必须连续递增、无损坏行。
	var subjects []string
	for _, p := range []string{h.archivePath(2), h.archivePath(1), h.Path()} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var rec SendRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("%s 含损坏行 %q: %v", p, line, err)
			}
			if rec.Time.IsZero() || len(rec.To) != 1 {
				t.Errorf("记录不完整: %+v", rec)
			}
			subjects = append(subjects, rec.Subject)
		}
	}
	if len(subjects) < 5 {
		t.Fatalf("轮转后应保留足够记录，实际 %d: %v", len(subjects), subjects)
	}
	for i, s := range subjects {
		want := fmt.Sprintf("msg-%03d", i+total-len(subjects))
		if s != want {
			t.Fatalf("记录应连续且按时间递增，第 %d 条为 %s，期望 %s", i, s, want)
		}
	}
}

func TestSendHistoryRotationConcurrent(t *testing.T) {
	h, err := OpenSendHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.SetRotation(512, 3)

	const workers, each = 8, 40
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := h.Record(SendRecord{
					Subject: fmt.Sprintf("w%d-%03d", w, i), To: []string{"x@y.com"}, OK: true,
				}); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发写入失败: %v", err)
	}

	total := 0
	for _, p := range []string{h.Path(), h.archivePath(1), h.archivePath(2), h.archivePath(3)} {
		data, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var rec SendRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("%s 含损坏行 %q", p, line)
			}
			total++
		}
	}
	if total < 3 || total > workers*each {
		t.Fatalf("归档记录总数异常: %d", total)
	}
}

func TestSendHistoryRotationDisabled(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenSendHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.SetRotation(0, 5) // 阈值 0 = 禁用
	for i := 0; i < 5; i++ {
		if err := h.Record(SendRecord{Subject: "x", To: []string{"a@x.com"}, OK: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(h.archivePath(1)); !os.IsNotExist(err) {
		t.Fatalf("禁用轮转不应产生归档: %v", err)
	}
	data, err := os.ReadFile(h.Path())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(strings.TrimSpace(string(data)), "\n")); n != 5 {
		t.Fatalf("禁用轮转时 5 条记录应都在主文件，实际 %d", n)
	}
}

// 回归：SendMessage 的所有失败路径都要落一条 history（这里用连不上的端口制造错误）。
func TestMailerRecordsFailure(t *testing.T) {
	h, err := OpenSendHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	cfg := &config.Config{
		SMTPHost: "127.0.0.1", SMTPPort: 1, // 几乎必定连接失败
		EmailUser: "me@example.com", EmailRecipients: []string{"to@example.com"},
	}
	m := NewMailer(cfg)
	m.SetHistory(h)
	if err := m.SendMessage(Message{Subject: "会失败", Text: "x"}); err == nil {
		t.Fatal("预期连接失败")
	}
	data, err := os.ReadFile(h.Path())
	if err != nil {
		t.Fatal(err)
	}
	var rec SendRecord
	if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err != nil {
		t.Fatalf("历史应有一条 JSON 记录: %s", data)
	}
	if rec.OK || rec.Subject != "会失败" || rec.Error == "" || len(rec.To) != 1 {
		t.Errorf("失败记录不正确: %+v", rec)
	}
}

type fakeSender struct {
	mu    sync.Mutex
	got   []Message
	fails int
}

func (f *fakeSender) SendMessage(m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return errors.New("boom")
	}
	f.got = append(f.got, m)
	return nil
}
func (f *fakeSender) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) }

func TestDigestBatching(t *testing.T) {
	fs := &fakeSender{}
	n := NewNotifier(fs, 150*time.Millisecond, 30)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	for _, s := range []string{"BTCUSDT", "ETHUSDT", "SOLUSDT"} {
		n.Enqueue(Message{Tag: s, Subject: s, Text: s + " hit", Card: "<i>" + s + "</i>"})
	}
	time.Sleep(500 * time.Millisecond)
	if fs.count() != 1 {
		t.Fatalf("应合并成 1 封，实际 %d", fs.count())
	}
	g := fs.got[0]
	if !strings.Contains(g.Subject, "3 条提醒") || !strings.Contains(g.Subject, "BTCUSDT") ||
		!strings.Contains(g.Text, "ETHUSDT hit") || !strings.Contains(g.Card, "SOLUSDT") {
		t.Errorf("digest: %+v", g)
	}
	cancel()
	<-done
}

func TestRetryThenSuccess(t *testing.T) {
	fs := &fakeSender{fails: 2}
	n := NewNotifier(fs, 0, 30)
	n.retries = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)
	n.Enqueue(Message{Subject: "x", Text: "y"})
	deadline := time.Now().Add(2 * time.Second)
	for fs.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fs.count() != 1 {
		t.Fatal("重试后应成功发出")
	}
}

func TestFlushOnShutdown(t *testing.T) {
	fs := &fakeSender{}
	n := NewNotifier(fs, time.Hour, 30) // 窗口很长：模拟退出时还在攒
	n.Enqueue(Message{Subject: "a", Text: "a"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.Run(ctx) // ctx 已取消：应走 flush
	if fs.count() != 1 {
		t.Errorf("退出时应把队列里的发完，实际 %d", fs.count())
	}
}

// 回归：退出时"正在收集"的那一批（已被弹出通道、还在 Run 的 batch 里）不能丢。
// 旧实现拿已取消的 ctx 去 deliver → waitRate 直接丢弃，且这批不在通道里、flush 看不到。
func TestInFlightBatchNotLostOnShutdown(t *testing.T) {
	fs := &fakeSender{}
	n := NewNotifier(fs, 300*time.Millisecond, 30) // 窗口 300ms，留出可取消的收集期
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	// 触发一条开始收集（Run 弹出 first 进 batch，进入 300ms 收集窗口）
	n.Enqueue(Message{Subject: "in-flight", Text: "in-flight"})
	time.Sleep(30 * time.Millisecond) // 确认已进入收集窗口、还在攒
	cancel()                          // 在收集期结束前取消
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for fs.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fs.count() != 1 {
		t.Fatalf("退出时正在收集的那批被丢了：发出 %d 封（应为 1）", fs.count())
	}
	if !strings.Contains(fs.got[0].Subject, "in-flight") {
		t.Errorf("应发回正在收集的那条: %+v", fs.got[0])
	}
}

func TestRender(t *testing.T) {
	a := Alert{Kind: "hit", Symbol: "BTCUSDT", Title: "到价 · 价格 ≥ 70,000", Price: "70,012.50",
		Changes: []Change{{"5m", 12, true}, {"1h", -250, true}, {"4h", 0, false}},
		ATRs:    []ATRV{{"5m", 5, price.New(15), true}, {"1h", 14, price.New(320), true}},
		RuleID:  "r_1", Flags: "一次性", At: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	text, card := a.Render()
	for _, want := range []string{"BTCUSDT", "70,012.50", "5m +0.12%", "1h -2.50%", "4h --",
		"波动  ATR 5m 15", "1h 320"} {
		if !strings.Contains(text, want) {
			t.Errorf("text 缺 %q:\n%s", want, text)
		}
	}
	if !strings.Contains(card, "#2ee59d") || !strings.Contains(card, "#ff5c5c") {
		t.Error("card 应有涨绿跌红")
	}
	if !strings.Contains(card, "ATR") || !strings.Contains(card, "5m") {
		t.Error("card 应含 ATR 行")
	}
	a.Title = "<script>alert(1)</script>"
	if _, c := a.Render(); strings.Contains(c, "<script>") {
		t.Error("HTML 必须转义")
	}
}
