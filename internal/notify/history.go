package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SendRecord 一条邮件发送记录（每次尝试一条，失败重试会留多条，便于追溯）。
type SendRecord struct {
	Time    time.Time `json:"time"`
	Subject string    `json:"subject"`
	To      []string  `json:"to"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
}

// 轮转默认：单文件 5 MiB，保留 5 份归档（.1 最新 … .5 最老）。
const (
	defaultHistoryMaxBytes int64 = 5 << 20
	defaultHistoryKeep           = 5
)

// SendHistory 追加写发送历史到 JSONL 文件。写入失败只记日志，绝不影响发信本身。
type SendHistory struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	maxBytes int64 // <=0 表示禁用轮转
	keep     int   // 归档份数，0 表示不保留归档
}

// OpenSendHistory 打开（必要时创建）发送历史文件，权限 0600。
func OpenSendHistory(dir string) (*SendHistory, error) {
	if dir == "" {
		return nil, errors.New("dir 必填")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	h := &SendHistory{
		path:     filepath.Join(dir, "send-history.jsonl"),
		maxBytes: defaultHistoryMaxBytes,
		keep:     defaultHistoryKeep,
	}
	if err := h.reopen(); err != nil {
		return nil, err
	}
	return h, nil
}

// SetRotation 调整轮转参数（须在 Record 前调用）。maxBytes<=0 禁用轮转；keep=0 不保留归档。
func (h *SendHistory) SetRotation(maxBytes int64, keep int) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.maxBytes = maxBytes
	if keep < 0 {
		keep = 0
	}
	h.keep = keep
}

// reopen 打开（或轮转后重新打开）当前历史文件，强制权限 0600。
func (h *SendHistory) reopen() error {
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil { // 文件可能已存在且权限过宽
		_ = f.Close()
		return err
	}
	h.f = f
	return nil
}

// archivePath 第 i 份归档的路径，i 越大越老：send-history.1.jsonl / .2.jsonl / …
func (h *SendHistory) archivePath(i int) string {
	ext := filepath.Ext(h.path)
	return strings.TrimSuffix(h.path, ext) + fmt.Sprintf(".%d%s", i, ext)
}

// rotate 在写入前检查大小，达标则滚动归档。调用方须持有 h.mu。
func (h *SendHistory) rotate() error {
	if h.maxBytes <= 0 || h.f == nil {
		return nil
	}
	fi, err := h.f.Stat()
	if err != nil || fi.Size() < h.maxBytes {
		return err
	}
	if err := h.f.Close(); err != nil {
		return err
	}
	h.f = nil // 关闭期间禁止写入，任何失败路径都要通过 fail 重开
	fail := func(err error) error {
		if reErr := h.reopen(); reErr != nil {
			return fmt.Errorf("%v（重开历史文件也失败: %v）", err, reErr)
		}
		return err
	}
	if h.keep == 0 {
		_ = os.Remove(h.path) // 不保留归档，直接丢弃
		return h.reopen()
	}
	if err := os.Remove(h.archivePath(h.keep)); err != nil && !os.IsNotExist(err) {
		return fail(err)
	}
	for i := h.keep - 1; i >= 1; i-- { // 由老到新滚动，避免覆盖尚未移动的归档
		src := h.archivePath(i)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fail(err)
		}
		if err := os.Rename(src, h.archivePath(i+1)); err != nil {
			return fail(err)
		}
	}
	if err := os.Rename(h.path, h.archivePath(1)); err != nil && !os.IsNotExist(err) {
		return fail(err)
	}
	return h.reopen()
}

// Record 追加一条记录；线程安全（Web 面板的测试邮件与提醒队列可能并发写）。
func (h *SendHistory) Record(r SendRecord) error {
	if h == nil {
		return nil
	}
	r.Time = time.Now()
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.rotate(); err != nil {
		log.Printf("mail: 发送历史轮转失败（不影响发信）: %v", err)
	}
	if h.f == nil {
		return errors.New("发送历史文件不可用")
	}
	_, err = h.f.Write(data)
	return err
}

// Path 返回历史文件路径。
func (h *SendHistory) Path() string {
	if h == nil {
		return ""
	}
	return h.path
}

// Close 关闭文件。
func (h *SendHistory) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return nil
	}
	return h.f.Close()
}
