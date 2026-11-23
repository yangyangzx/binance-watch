// Package server 本机 Web 面板：看行情、管规则。
//
// 安全模型（无鉴权，所以必须只在本机可达）：
//  1. 只允许绑定回环地址（127.0.0.1 / ::1 / localhost）；
//  2. 校验 Host 头，防 DNS 重绑定；
//  3. 写操作必须带自定义头 X-BW（浏览器跨站请求带不上，且不发 CORS 头）；带 body 的 POST
//     额外要求 Content-Type=application/json，DELETE 无 body 不卡 JSON 头；
//  4. 严格 CSP，禁止内联脚本与外部资源。
//
// 想从手机/别的机器看：用 SSH 端口转发（ssh -L 8090:127.0.0.1:8090 pi），不要改成 0.0.0.0。
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"binance-watch/internal/engine"
	"binance-watch/internal/notify"
	"binance-watch/internal/recipients"
	"binance-watch/internal/rules"
)

//go:embed web
var webFS embed.FS

const maxRules = 200

// Server Web 面板。
type Server struct {
	addr   string
	eng    *engine.Engine
	rl     *rules.Store
	rs     *recipients.Store
	sender notify.Sender // 测试邮件用；nil 表示该能力不可用
	http   *http.Server
}

// New 校验监听地址（必须是回环）并构造服务。
func New(addr string, eng *engine.Engine, rl *rules.Store, rs *recipients.Store, sender notify.Sender) (*Server, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("web 地址格式应为 host:port: %w", err)
	}
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("拒绝监听 %q：面板没有鉴权，只允许 127.0.0.1/::1/localhost（远程访问请用 SSH 端口转发）", host)
	}
	s := &Server{addr: addr, eng: eng, rl: rl, rs: rs, sender: sender}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/rules", s.handleAdd)
	mux.HandleFunc("DELETE /api/rules/{id}", s.handleDelete)
	mux.HandleFunc("POST /api/rules/{id}/active", s.handleActive)
	mux.HandleFunc("GET /api/mail/recipients", s.handleGetRecipients)
	mux.HandleFunc("POST /api/mail/recipients", s.handleSetRecipients)
	mux.HandleFunc("POST /api/mail/test", s.handleTestMail)
	s.http = &http.Server{
		Handler:           s.guard(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      45 * time.Second, // 测试邮件要同步等 SMTP 会话（连接超时 30s + 余量）
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	return s, nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Run 阻塞直到 ctx 结束。
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	log.Printf("web: 面板已启动 http://%s （仅本机可访问）", ln.Addr())
	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.http.Shutdown(sctx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if !isLoopbackHost(host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		// 写操作一律要求自定义头 X-BW（浏览器跨站请求带不上，且我们不发 CORS 头）→ 防 CSRF。
		// 只有"带 body"的方法（POST）额外要求 Content-Type=application/json，
		// 因为 DELETE 没有 body、不应被 JSON 头卡住（否则面板/任何干净客户端的删除都会 403）。
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-BW") != "1" {
				writeErr(w, http.StatusForbidden, "缺少 X-BW 头（写操作需带自定义头，防跨站请求）")
				return
			}
			if r.Method == http.MethodPost &&
				!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeErr(w, http.StatusForbidden, "POST 需 Content-Type=application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.eng.Status())
}

// ------------------------------------------------------------ 收件人配置

func (s *Server) handleGetRecipients(w http.ResponseWriter, _ *http.Request) {
	if s.rs == nil {
		writeErr(w, http.StatusServiceUnavailable, "收件人配置不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"to": s.rs.List()})
}

func (s *Server) handleSetRecipients(w http.ResponseWriter, r *http.Request) {
	if s.rs == nil {
		writeErr(w, http.StatusServiceUnavailable, "收件人配置不可用")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var q struct {
		To string `json:"to"`
	}
	if err := dec.Decode(&q); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	to, err := recipients.Normalize([]string{q.To})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.rs.Set(to); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	log.Printf("web: 收件人已更新 → %s", strings.Join(to, ","))
	writeJSON(w, http.StatusOK, map[string]any{"to": to})
}

func (s *Server) handleTestMail(w http.ResponseWriter, _ *http.Request) {
	if s.sender == nil {
		writeErr(w, http.StatusServiceUnavailable, "测试邮件不可用（dry-run 模式下未接入发件器）")
		return
	}
	to := "（未配置）"
	if s.rs != nil {
		if list := s.rs.List(); len(list) > 0 {
			to = strings.Join(list, ", ")
		}
	}
	msg := notify.Message{
		Subject: "[binance-watch] 测试邮件 · 收件人配置校验",
		Text: "这是一封来自 binance-watch 的测试邮件。\n\n当前收件人：" + to +
			"\n\n收到这封邮件说明 SMTP 配置可用，且面板上的收件人设置已生效。",
	}
	if err := s.sender.SendMessage(msg); err != nil {
		writeErr(w, http.StatusBadGateway, "发送失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "已发送"})
}

type addReq struct {
	Symbol      string `json:"symbol"`
	Trigger     string `json:"trigger"`
	Dir         string `json:"dir"`
	Target      string `json:"target"`
	PctBP       int    `json:"pct_bp"`
	WindowMin   int    `json:"window_min"`
	NearBP      int    `json:"near_bp"`
	CooldownMin int    `json:"cooldown_min"`
	Once        bool   `json:"once"`
	Note        string `json:"note"`
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var q addReq
	if err := dec.Decode(&q); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	if len(s.rl.List()) >= maxRules {
		writeErr(w, http.StatusConflict, fmt.Sprintf("规则数量已达上限 %d", maxRules))
		return
	}
	if len([]rune(q.Note)) > 200 {
		writeErr(w, http.StatusBadRequest, "备注最长 200 字")
		return
	}
	rule, err := s.rl.Add(rules.Rule{
		Symbol: q.Symbol, Trigger: q.Trigger, Dir: q.Dir, Target: q.Target,
		PctBP: q.PctBP, WindowMin: q.WindowMin, NearBP: q.NearBP,
		CooldownMin: q.CooldownMin, Once: q.Once, Note: q.Note,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("web: 新增规则 %s %s %s", rule.ID, rule.Symbol, rule.Describe())
	writeJSON(w, http.StatusCreated, rule)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	rule, err := s.rl.Remove(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	log.Printf("web: 删除规则 %s %s", rule.ID, rule.Symbol)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": rule.ID})
}

func (s *Server) handleActive(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var q struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	rule, err := s.rl.SetActive(r.PathValue("id"), q.Active)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func statusFor(err error) int {
	var m *rules.ErrMissing
	if errors.As(err, &m) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}
