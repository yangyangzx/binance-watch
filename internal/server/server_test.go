package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"binance-watch/internal/config"
	"binance-watch/internal/engine"
	"binance-watch/internal/notify"
	"binance-watch/internal/recipients"
	"binance-watch/internal/rules"
	"binance-watch/internal/source"
)

type nopSink struct{}

func (nopSink) Enqueue(notify.Message) {}

func newTestServer(t *testing.T) (*Server, *rules.Store) {
	t.Helper()
	cfg := &config.Config{Symbols: []string{"BTCUSDT"}, HealthStaleSec: 90, WatchDir: t.TempDir()}
	src, _ := source.New(cfg)
	rl, err := rules.New(cfg.WatchDir)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := recipients.New(cfg.WatchDir, []string{"ops@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New("127.0.0.1:0", engine.New(cfg, src, rl, nopSink{}), rl, rs, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, rl
}

func do(s *Server, method, path, host, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, req)
	return w
}

var jsonHdr = map[string]string{"Content-Type": "application/json", "X-BW": "1"}

func TestRejectNonLoopbackBind(t *testing.T) {
	for _, a := range []string{"0.0.0.0:8090", ":8090", "192.168.1.5:8090", "example.com:80"} {
		if _, err := New(a, nil, nil, nil, nil); err == nil {
			t.Errorf("%s 必须被拒绝", a)
		}
	}
	for _, a := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		if _, err := New(a, nil, nil, nil, nil); err != nil {
			t.Errorf("%s 应允许: %v", a, err)
		}
	}
}

func TestStaticAndStatus(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(s, "GET", "/", "127.0.0.1:8090", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "binance-watch") {
		t.Fatalf("index: %d", w.Code)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("缺 CSP: %q", csp)
	}
	for _, f := range []string{"/app.js", "/style.css"} {
		if w := do(s, "GET", f, "localhost:8090", "", nil); w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("%s: %d", f, w.Code)
		}
	}
	w = do(s, "GET", "/api/status", "127.0.0.1:8090", "", nil)
	var st engine.Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || len(st.Symbols) != 1 {
		t.Fatalf("status: %v %s", err, w.Body.String())
	}
}

func TestHostGuardAgainstRebinding(t *testing.T) {
	s, _ := newTestServer(t)
	for _, h := range []string{"evil.com", "evil.com:8090", "192.168.1.10:8090", "127.0.0.1.evil.com"} {
		if w := do(s, "GET", "/api/status", h, "", nil); w.Code != http.StatusForbidden {
			t.Errorf("Host=%s 应 403，实际 %d", h, w.Code)
		}
	}
}

func TestCSRFGuard(t *testing.T) {
	s, rl := newTestServer(t)
	body := `{"symbol":"BTCUSDT","trigger":"target","dir":"above","target":"70000"}`
	if w := do(s, "POST", "/api/rules", "127.0.0.1:8090", body, map[string]string{"Content-Type": "text/plain"}); w.Code != 403 {
		t.Errorf("无 X-BW 应 403: %d", w.Code)
	}
	if w := do(s, "POST", "/api/rules", "127.0.0.1:8090", body, map[string]string{"X-BW": "1", "Content-Type": "application/x-www-form-urlencoded"}); w.Code != 403 {
		t.Errorf("表单类型应 403: %d", w.Code)
	}
	if len(rl.List()) != 0 {
		t.Error("被拒绝的请求不应写入")
	}
}

func TestRuleLifecycle(t *testing.T) {
	s, rl := newTestServer(t)
	h := "127.0.0.1:8090"
	w := do(s, "POST", "/api/rules", h, `{"symbol":"btcusdt","trigger":"pct","dir":"down","pct_bp":300,"window_min":60,"cooldown_min":30,"note":"<b>x</b>"}`, jsonHdr)
	if w.Code != 201 {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	var r rules.Rule
	json.Unmarshal(w.Body.Bytes(), &r)
	if r.Symbol != "BTCUSDT" || len(rl.List()) != 1 {
		t.Fatalf("rule: %+v", r)
	}
	// 非法请求
	for _, bad := range []string{`{"symbol":"BTC!","trigger":"target","dir":"above","target":"1"}`, `{"nope":1}`, `not json`,
		`{"symbol":"BTCUSDT","trigger":"pct","pct_bp":300,"window_min":999}`} {
		if w := do(s, "POST", "/api/rules", h, bad, jsonHdr); w.Code != 400 {
			t.Errorf("%s 应 400: %d", bad, w.Code)
		}
	}
	w = do(s, "POST", "/api/rules/"+r.ID+"/active", h, `{"active":false}`, jsonHdr)
	if w.Code != 200 {
		t.Fatalf("active: %d", w.Code)
	}
	if got, _ := rl.Get(r.ID); got.Active {
		t.Error("应已停用")
	}
	if w := do(s, "DELETE", "/api/rules/r_nope", h, `{}`, jsonHdr); w.Code != 404 {
		t.Errorf("删除不存在应 404: %d", w.Code)
	}
	// DELETE 无 body：只带 X-BW（不带 JSON Content-Type）也必须能删，不能被 JSON 头卡住
	if w := do(s, "DELETE", "/api/rules/"+r.ID, h, "", map[string]string{"X-BW": "1"}); w.Code != 200 {
		t.Errorf("DELETE 只带 X-BW 应 200: %d %s", w.Code, w.Body.String())
	}
	if len(rl.List()) != 0 {
		t.Error("应已删除")
	}
	// 但缺 X-BW 的 DELETE 仍要被 403 拦下（CSRF 防护不能放松）
	if w := do(s, "DELETE", "/api/rules/r_nope", h, "", nil); w.Code != http.StatusForbidden {
		t.Errorf("无 X-BW 的 DELETE 应 403: %d", w.Code)
	}
	_ = io.Discard
}

// 回归：ATR 卡片只显示价格单位数值（不再拼 pct_bp 百分比），且带说明文字与 hover 提示。
func TestATRDisplay(t *testing.T) {
	js, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	if strings.Contains(src, "pctText(a.pct_bp)") {
		t.Error("ATR 不应显示 ATR÷现价 的百分比（计算不准确），只显示价格单位数值")
	}
	for _, want := range []string{"ATR · 平均真实波幅", "atrLbl.title", "d.title"} {
		if !strings.Contains(src, want) {
			t.Errorf("ATR 说明信息缺失: %q", want)
		}
	}
}

func TestRecipientsAPI(t *testing.T) {
	s, _ := newTestServer(t)
	h := "127.0.0.1:8090"

	w := do(s, "GET", "/api/mail/recipients", h, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		To []string `json:"to"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.To) != 1 || got.To[0] != "ops@example.com" {
		t.Fatalf("seed 收件人: %v %s", err, w.Body.String())
	}

	w = do(s, "POST", "/api/mail/recipients", h, `{"to":"a@outlook.com; b@outlook.com"}`, jsonHdr)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if list := s.rs.List(); len(list) != 2 || list[0] != "a@outlook.com" {
		t.Fatalf("保存后内存未更新: %v", list)
	}
	// 重新读盘也必须看到新值（模拟 daemon 重启）
	rs2, err := recipients.New(filepath.Dir(s.rs.Path()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if list := rs2.List(); len(list) != 2 || list[0] != "a@outlook.com" {
		t.Fatalf("持久化失败: %v", list)
	}

	// 非法收件人必须 400（含头注入）
	for _, bad := range []string{
		`{"to":""}`,
		`{"to":"not-an-email"}`,
		`{"to":"a@b.com\r\nBcc: evil@x.com"}`,
		`{"to":"\"a@b.com\" <evil@x.com>"}`,
		`{"nope":1}`,
		`not json`,
	} {
		if w := do(s, "POST", "/api/mail/recipients", h, bad, jsonHdr); w.Code != http.StatusBadRequest {
			t.Errorf("%s 应 400，实际 %d: %s", bad, w.Code, w.Body.String())
		}
	}
	// 写操作仍受 X-BW / Content-Type 保护
	if w := do(s, "POST", "/api/mail/recipients", h, `{"to":"a@b.com"}`, nil); w.Code != http.StatusForbidden {
		t.Errorf("无 X-BW 应 403，实际 %d", w.Code)
	}
	if w := do(s, "POST", "/api/mail/recipients", h, `{"to":"a@b.com"}`, map[string]string{"X-BW": "1", "Content-Type": "text/plain"}); w.Code != http.StatusForbidden {
		t.Errorf("非 JSON 应 403，实际 %d", w.Code)
	}
	// 测试邮件未接入发件器（dry-run）时应明确报不可用，不能假装成功
	if w := do(s, "POST", "/api/mail/test", h, `{}`, jsonHdr); w.Code != http.StatusServiceUnavailable {
		t.Errorf("未接发件器应 503，实际 %d", w.Code)
	}
}
