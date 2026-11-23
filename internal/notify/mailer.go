// Package notify 邮件发送：规范 MIME、直连 SMTP（不走代理）、带重试/限流/合并的发送队列。
package notify

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"

	"binance-watch/internal/config"
)

// Message 一封待发邮件。
type Message struct {
	Tag     string // 摘要标题里用的短标签，如交易对
	Subject string
	Text    string // text/plain 正文
	Card    string // HTML 卡片片段（可空；为空则只发纯文本）
}

// RecipientSource 提供当前收件人（由 internal/recipients.Store 实现）。
// 每封邮件发送时重新读取，使面板上的修改无需重启即可生效。
type RecipientSource interface {
	List() []string
}

// Mailer 邮件发送器。连接策略沿用 ~/script/utils/notifier.py：
//   - SMTP_PORT == 465 或 SSL_ENABLE=true → 直接 TLS（SMTPS）
//   - 否则                                 → 587 + STARTTLS
type Mailer struct {
	cfg     *config.Config
	rs      RecipientSource
	history *SendHistory
}

func NewMailer(cfg *config.Config) *Mailer { return &Mailer{cfg: cfg} }

// SetRecipients 注入运行时可变的收件人来源；未注入时回退到 .env 里的配置。
func (m *Mailer) SetRecipients(rs RecipientSource) { m.rs = rs }

// SetHistory 注入发送历史记录器；未注入时不落盘。
func (m *Mailer) SetHistory(h *SendHistory) { m.history = h }

func (m *Mailer) recipients() []string {
	if m.rs != nil {
		if to := m.rs.List(); len(to) > 0 {
			return to
		}
	}
	return m.cfg.EmailRecipients
}

const smtpTimeout = 30 * time.Second

// Send 发纯文本（阻塞）。
func (m *Mailer) Send(subject, body string) error {
	return m.SendMessage(Message{Subject: subject, Text: body})
}

// SendMessage 发一封邮件（阻塞）。每次尝试（含失败）都会记一条发送历史。
func (m *Mailer) SendMessage(msg Message) (err error) {
	host, user := m.cfg.SMTPHost, m.cfg.EmailUser
	to := m.recipients()
	defer func() {
		if m.history == nil {
			return
		}
		rec := SendRecord{Subject: oneLine(msg.Subject), To: append([]string(nil), to...), OK: err == nil}
		if err != nil {
			rec.Error = err.Error()
		}
		if werr := m.history.Record(rec); werr != nil {
			log.Printf("mail: 写发送历史失败: %v", werr)
		}
	}()
	raw, err := buildMIME(user, to, msg, time.Now(), host)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(host, fmt.Sprint(m.cfg.SMTPPort))
	var conn net.Conn
	if m.cfg.SSLEnabled || m.cfg.SMTPPort == 465 {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: smtpTimeout}, "tcp", addr,
			&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = net.DialTimeout("tcp", addr, smtpTimeout)
	}
	if err != nil {
		return fmt.Errorf("连接 %s: %w", addr, err)
	}
	// 整个会话（含 STARTTLS 握手、DATA）的总时限，避免对端挂起导致协程永久阻塞
	_ = conn.SetDeadline(time.Now().Add(2 * smtpTimeout))

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp 握手: %w", err)
	}
	defer c.Close()

	if !(m.cfg.SSLEnabled || m.cfg.SMTPPort == 465) {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("服务器不支持 STARTTLS（拒绝明文发送凭据）")
		}
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if user != "" && m.cfg.EmailPass != "" {
		if err := c.Auth(smtp.PlainAuth("", user, m.cfg.EmailPass, host)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := c.Mail(user); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("rcpt %s: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("data close: %w", err)
	}
	_ = c.Quit()
	log.Printf("mail: 已发送 %q → %s", oneLine(msg.Subject), strings.Join(to, ","))
	return nil
}

// buildMIME 生成符合 RFC 5322/2045/2047 的邮件：
// 中文主题用 RFC2047 编码；带 Date/Message-ID/MIME-Version；正文 base64（避免超长行/8bit 被网关破坏）。
func buildMIME(from string, to []string, msg Message, now time.Time, idHost string) ([]byte, error) {
	if from == "" || len(to) == 0 {
		return nil, errors.New("发件人/收件人为空")
	}
	for _, a := range append([]string{from}, to...) {
		if strings.ContainsAny(a, "\r\n") {
			return nil, fmt.Errorf("非法地址: %q", a)
		}
	}
	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", fmt.Sprintf("%s <%s>", mime.BEncoding.Encode("utf-8", "binance-watch"), from))
	h("To", strings.Join(to, ", "))
	h("Subject", mime.BEncoding.Encode("utf-8", oneLine(msg.Subject)))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", fmt.Sprintf("<%s@%s>", randHex(12), idHost))
	h("MIME-Version", "1.0")
	h("X-Mailer", "binance-watch")
	h("Auto-Submitted", "auto-generated")

	if msg.Card == "" {
		h("Content-Type", "text/plain; charset=utf-8")
		h("Content-Transfer-Encoding", "base64")
		b.WriteString("\r\n")
		writeB64(&b, msg.Text)
		return b.Bytes(), nil
	}
	boundary := "bw_" + randHex(12)
	h("Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", boundary))
	b.WriteString("\r\n")
	part := func(ctype, body string) {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", boundary, ctype)
		writeB64(&b, body)
	}
	part("text/plain", msg.Text)
	part("text/html", PageHTML(msg.Card))
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes(), nil
}

func writeB64(b *bytes.Buffer, s string) {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteString("\r\n")
}

func oneLine(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	return strings.TrimSpace(s)
}

func randHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
