// Package recipients 管理可运行时修改的告警收件人：内存 + JSON 文件，原子落盘、并发安全。
// 与发件凭据（.env）刻意分离：这里只存"发给谁"，不碰 SMTP 账号密码。
package recipients

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxRecipients = 20
	maxAddrLen    = 254
)

type doc struct {
	To        []string  `json:"to"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Store 内存 + 文件持久化。写操作 = 文件锁 → 原子写 → 换内存；读操作返回副本。
type Store struct {
	mu   sync.RWMutex
	path string
	lock string
	to   []string
}

// New 打开（必要时创建）收件人配置。文件不存在时用 seed（来自 .env 的 EMAIL_TO）作为初始值，
// 但不立即落盘——只有用户在面板保存过才写文件，避免与继续维护 .env 的旧流程互相覆盖。
func New(dir string, seed []string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("dir 必填")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "mail.json"), lock: filepath.Join(dir, "mail.lock")}
	if err := s.load(); err != nil {
		return nil, err
	}
	if len(s.to) == 0 {
		to, err := Normalize(seed)
		if err != nil {
			return nil, fmt.Errorf("内置收件人配置无效: %w", err)
		}
		s.to = to
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	var d doc
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("%s 解析失败: %w", s.path, err)
	}
	to, err := Normalize(d.To)
	if err != nil {
		return fmt.Errorf("%s 内容无效: %w", s.path, err)
	}
	s.to = to
	return nil
}

// Normalize 拆分字符串（逗号/分号/空白）并严格校验收件人，返回去重后的副本。
// 要求 mail.ParseAddress 的解析结果与原文完全一致，杜绝 "a@b.com <evil@x.com>" 这类头注入。
func Normalize(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		for _, raw := range strings.FieldsFunc(strings.TrimSpace(item), func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
		}) {
			a := strings.TrimSpace(raw)
			if a == "" {
				continue
			}
			if strings.ContainsAny(a, "\r\n") {
				return nil, errors.New("邮箱地址不能包含换行")
			}
			if len(a) > maxAddrLen {
				return nil, fmt.Errorf("邮箱地址过长（上限 %d 字符）: %q", maxAddrLen, a)
			}
			addr, err := mail.ParseAddress(a)
			if err != nil || addr.Address != a {
				return nil, fmt.Errorf("邮箱地址不合法: %q", a)
			}
			key := strings.ToLower(addr.Address)
			if !seen[key] {
				seen[key] = true
				out = append(out, addr.Address)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("至少需要一个收件人")
	}
	if len(out) > maxRecipients {
		return nil, fmt.Errorf("收件人最多 %d 个", maxRecipients)
	}
	return out, nil
}

// List 返回收件人副本（调用方改动不影响内部状态）。
func (s *Store) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.to...)
}

// Set 全量替换收件人；先校验，再在文件锁内原子落盘，最后更新内存。
func (s *Store) Set(in []string) error {
	to, err := Normalize(in)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockFile(s.lock)
	if err != nil {
		return fmt.Errorf("获取文件锁失败: %w", err)
	}
	defer unlock()
	if err := s.persist(to); err != nil {
		return err
	}
	s.to = to
	return nil
}

func (s *Store) persist(to []string) error {
	data, err := json.MarshalIndent(doc{To: to, UpdatedAt: time.Now()}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "mail-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		cleanup()
		return err
	}
	if d, err := os.Open(dir); err == nil { // 目录 fsync：断电后 rename 不丢
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Path 返回配置文件路径。
func (s *Store) Path() string { return s.path }
