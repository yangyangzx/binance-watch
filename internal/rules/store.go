// Package rules 保存提醒规则：内存 + JSON 文件，支持跨进程安全读写与热加载。
package rules

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"binance-watch/internal/config"
	"binance-watch/internal/price"
)

// Trigger 类型
const (
	TrigTarget = "target" // 价格 >= / <= 目标
	TrigPct    = "pct"    // 窗口涨跌幅 >= 阈值
	TrigNear   = "near"   // 距目标 <= near_bp（前置提醒）
)

const (
	// MaxWindowMin 涨跌幅窗口上限（设计上最大 4h；内存保留 ~15h 的 1m K 线是为 ATR(1h) 聚合）。
	MaxWindowMin = 240
	// MaxCooldownMin 一周。
	MaxCooldownMin = 7 * 24 * 60
	// DefaultCooldownMin 创建规则时的默认冷却（分钟）。
	DefaultCooldownMin = 30
)

// Rule 一条价格提醒规则。
type Rule struct {
	ID          string    `json:"id"`
	Symbol      string    `json:"symbol"`
	Trigger     string    `json:"trigger"`
	Dir         string    `json:"dir,omitempty"`        // target: above/below；pct: up/down/auto
	Target      string    `json:"target,omitempty"`     // "70000" 或 "70000.00000000"
	PctBP       int       `json:"pct_bp,omitempty"`     // 基点：300 = 3.00%
	WindowMin   int       `json:"window_min,omitempty"` // 分钟；0 = 整个缓冲区（4h）
	NearBP      int       `json:"near_bp,omitempty"`    // 基点：50 = 0.50%
	CooldownMin int       `json:"cooldown_min,omitempty"`
	Once        bool      `json:"once"`
	Active      bool      `json:"active"`
	Note        string    `json:"note,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	LastHitAt   time.Time `json:"last_hit_at,omitempty"`
	HitCount    int       `json:"hit_count"`
}

// TargetPrice 解析 Target 成定点价格。
func (r *Rule) TargetPrice() (price.Price, error) { return price.Parse(r.Target) }

// TargetInt 兼容旧接口。
func (r *Rule) TargetInt() (int64, error) {
	p, err := r.TargetPrice()
	return int64(p), err
}

// EffectiveWindow 返回实际使用的窗口分钟数（0 → MaxWindowMin）。
func (r *Rule) EffectiveWindow() int {
	if r.WindowMin <= 0 || r.WindowMin > MaxWindowMin {
		return MaxWindowMin
	}
	return r.WindowMin
}

// Normalize 规范化大小写与方向别名（原地修改）。
func (r *Rule) Normalize() {
	r.Symbol = strings.ToUpper(strings.TrimSpace(r.Symbol))
	r.Trigger = strings.ToLower(strings.TrimSpace(r.Trigger))
	r.Dir = strings.ToLower(strings.TrimSpace(r.Dir))
	r.Target = strings.TrimSpace(r.Target)
	r.Note = strings.TrimSpace(r.Note)
	if r.Trigger == TrigPct {
		switch r.Dir {
		case "", "auto", "both":
			r.Dir = "auto"
		case "up", "above", "rise":
			r.Dir = "up"
		case "down", "below", "fall":
			r.Dir = "down"
		}
	}
}

// Validate 校验（应先 Normalize）。
func (r *Rule) Validate() error {
	if !config.ValidSymbol(r.Symbol) {
		return fmt.Errorf("symbol %q 不合法（应形如 BTCUSDT）", r.Symbol)
	}
	if r.CooldownMin < 0 || r.CooldownMin > MaxCooldownMin {
		return fmt.Errorf("cooldown 需在 0~%d 分钟之间", MaxCooldownMin)
	}
	if r.WindowMin < 0 || r.WindowMin > MaxWindowMin {
		return fmt.Errorf("window 需在 0~%d 分钟之间（最大支持 4h 窗口）", MaxWindowMin)
	}
	switch r.Trigger {
	case TrigTarget:
		if r.Dir != "above" && r.Dir != "below" {
			return errors.New("trigger=target 需要 dir=above/below")
		}
		return r.checkTarget()
	case TrigNear:
		if r.NearBP <= 0 || r.NearBP > 10000 {
			return errors.New("trigger=near 需要 near_bp 在 1~10000 之间（50 = 0.50%）")
		}
		return r.checkTarget()
	case TrigPct:
		if r.PctBP <= 0 || r.PctBP > 100000 {
			return errors.New("trigger=pct 需要 pct_bp 在 1~100000 之间（300 = 3.00%）")
		}
		if r.Dir != "up" && r.Dir != "down" && r.Dir != "auto" {
			return errors.New("trigger=pct 需要 dir=up/down/auto")
		}
	default:
		return fmt.Errorf("未知 trigger: %q", r.Trigger)
	}
	return nil
}

func (r *Rule) checkTarget() error {
	if r.Target == "" {
		return fmt.Errorf("trigger=%s 需要 target 价格", r.Trigger)
	}
	p, err := price.Parse(r.Target)
	if err != nil {
		return fmt.Errorf("target 不是合法价格: %w", err)
	}
	if p <= 0 {
		return errors.New("target 必须 > 0")
	}
	return nil
}

// Store 内存 + 文件持久化。多进程安全：所有写操作 = 加文件锁 → 重读磁盘 → 修改 → 原子落盘。
type Store struct {
	mu    sync.RWMutex
	path  string
	lock  string
	items []Rule
	stamp fileStamp
	ver   uint64 // 每次内存规则集变化 +1（本进程写入或从磁盘重新加载）
}

type fileStamp struct {
	mod  int64
	size int64
	ok   bool
}

// New 打开（必要时创建）数据目录并加载规则。
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("dir 必填")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "rules.json"), lock: filepath.Join(dir, "rules.lock")}
	items, st, err := s.readDisk()
	if err != nil {
		return nil, err
	}
	s.items, s.stamp = items, st
	return s, nil
}

// Path 返回规则文件路径。
func (s *Store) Path() string { return s.path }

func (s *Store) readDisk() ([]Rule, fileStamp, error) {
	fi, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fileStamp{}, nil
		}
		return nil, fileStamp{}, err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fileStamp{}, err
	}
	st := fileStamp{mod: fi.ModTime().UnixNano(), size: fi.Size(), ok: true}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return nil, st, nil
	}
	var out []Rule
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, st, fmt.Errorf("%s 解析失败: %w", s.path, err)
	}
	for i := range out {
		out[i].Normalize()
	}
	return out, st, nil
}

// Refresh 若磁盘文件被外部修改则重新加载。解析失败时保留旧规则并返回错误。
func (s *Store) Refresh() (changed bool, err error) {
	fi, serr := os.Stat(s.path)
	s.mu.RLock()
	cur := s.stamp
	s.mu.RUnlock()
	switch {
	case serr == nil && cur.ok && fi.ModTime().UnixNano() == cur.mod && fi.Size() == cur.size:
		return false, nil
	case serr != nil && os.IsNotExist(serr) && !cur.ok:
		return false, nil
	case serr != nil && !os.IsNotExist(serr):
		return false, serr
	}
	items, st, rerr := s.readDisk()
	if rerr != nil {
		// 记下 stamp，避免每个 tick 都重复报同一个解析错误
		s.mu.Lock()
		s.stamp = st
		s.mu.Unlock()
		return false, rerr
	}
	s.mu.Lock()
	s.items, s.stamp = items, st
	s.ver++
	s.mu.Unlock()
	return true, nil
}

// Version 规则集版本号：任何变化（含本进程自己的增删改）都会递增，用于变更检测。
func (s *Store) Version() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ver
}

// mutate 在文件锁内完成 重读 → 修改 → 落盘。
func (s *Store) mutate(fn func(items []Rule) ([]Rule, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockFile(s.lock)
	if err != nil {
		return fmt.Errorf("获取文件锁失败: %w", err)
	}
	defer unlock()
	cur, _, err := s.readDisk()
	if err != nil {
		return err
	}
	next, err := fn(cur)
	if errors.Is(err, errSkip) {
		return nil // 无需改动（例如规则已被外部删除）
	}
	if err != nil {
		return err
	}
	st, err := s.persist(next)
	if err != nil {
		return err
	}
	s.items, s.stamp = next, st
	s.ver++
	return nil
}

func (s *Store) persist(items []Rule) (fileStamp, error) {
	if items == nil {
		items = []Rule{}
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return fileStamp{}, err
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "rules-*.tmp")
	if err != nil {
		return fileStamp{}, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fileStamp{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fileStamp{}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fileStamp{}, err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		cleanup()
		return fileStamp{}, err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		cleanup()
		return fileStamp{}, err
	}
	if d, err := os.Open(dir); err == nil { // 目录 fsync：断电后 rename 不丢
		_ = d.Sync()
		_ = d.Close()
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{mod: fi.ModTime().UnixNano(), size: fi.Size(), ok: true}, nil
}

// List 返回副本。
func (s *Store) List() []Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Rule, len(s.items))
	copy(out, s.items)
	return out
}

// Get 按完整 ID 或唯一前缀查找。
func (s *Store) Get(id string) (Rule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i, err := indexOf(s.items, id); err == nil {
		return s.items[i], true
	}
	return Rule{}, false
}

// Add 添加规则；返回最终写入的规则（含生成的 ID）。
func (s *Store) Add(r Rule) (Rule, error) {
	r.Normalize()
	if err := r.Validate(); err != nil {
		return Rule{}, err
	}
	if r.ID == "" {
		r.ID = newID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	r.Active = true
	err := s.mutate(func(items []Rule) ([]Rule, error) {
		for _, x := range items {
			if x.ID == r.ID {
				return nil, &ErrExists{ID: r.ID}
			}
		}
		return append(items, r), nil
	})
	return r, err
}

// Remove 删除规则（支持唯一前缀）。返回被删除的规则。
func (s *Store) Remove(id string) (Rule, error) {
	var removed Rule
	err := s.mutate(func(items []Rule) ([]Rule, error) {
		i, err := indexOf(items, id)
		if err != nil {
			return nil, err
		}
		removed = items[i]
		return append(items[:i:i], items[i+1:]...), nil
	})
	return removed, err
}

// SetActive 开关；重新启用时清除 once 触发过的痕迹，让它能再次提醒。
func (s *Store) SetActive(id string, active bool) (Rule, error) {
	var out Rule
	err := s.mutate(func(items []Rule) ([]Rule, error) {
		i, err := indexOf(items, id)
		if err != nil {
			return nil, err
		}
		items[i].Active = active
		if active && items[i].Once {
			items[i].LastHitAt = time.Time{}
		}
		out = items[i]
		return items, nil
	})
	return out, err
}

// MarkHit 记录一次触发；规则若已被外部删除则静默忽略。
func (s *Store) MarkHit(id string, at time.Time) error {
	return s.mutate(func(items []Rule) ([]Rule, error) {
		for i := range items {
			if items[i].ID == id {
				items[i].LastHitAt = at
				items[i].HitCount++
				if items[i].Once {
					items[i].Active = false
				}
				return items, nil
			}
		}
		return nil, errSkip
	})
}

var errSkip = errors.New("skip")

func indexOf(items []Rule, id string) (int, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return -1, &ErrMissing{ID: id}
	}
	match := -1
	for i, x := range items {
		if x.ID == id {
			return i, nil
		}
		if strings.HasPrefix(x.ID, id) {
			if match >= 0 {
				return -1, fmt.Errorf("前缀 %q 匹配多条规则，请写完整 ID", id)
			}
			match = i
		}
	}
	if match < 0 {
		return -1, &ErrMissing{ID: id}
	}
	return match, nil
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "r_" + hex.EncodeToString(b)
}

// ErrExists / ErrMissing
type ErrExists struct{ ID string }

func (e *ErrExists) Error() string { return "规则已存在: " + e.ID }

type ErrMissing struct{ ID string }

func (e *ErrMissing) Error() string { return "找不到规则: " + e.ID }
