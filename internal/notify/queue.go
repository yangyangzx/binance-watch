package notify

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// Sender 抽象，便于测试。
type Sender interface {
	SendMessage(Message) error
}

// Notifier 异步发送队列：
//   - 合并：窗口期内到达的多条提醒合成一封摘要邮件（QQ/Outlook 对短时间同主题多封很敏感）
//   - 限流：每分钟最多 N 封
//   - 重试：失败按 5s/20s/60s 退避重试，仍失败才放弃并写日志
//   - 退出：ctx 取消后把队列里剩余的尽力发完（≤15s）
type Notifier struct {
	sender  Sender
	window  time.Duration
	perMin  int
	ch      chan Message
	retries []time.Duration
	sent    []time.Time
}

// NewNotifier 创建队列。window=0 表示不合并。
func NewNotifier(s Sender, window time.Duration, perMin int) *Notifier {
	if perMin < 1 {
		perMin = 1
	}
	return &Notifier{
		sender:  s,
		window:  window,
		perMin:  perMin,
		ch:      make(chan Message, 256),
		retries: []time.Duration{5 * time.Second, 20 * time.Second, 60 * time.Second},
	}
}

// Enqueue 非阻塞入队；队列满则丢弃并告警（正常不会发生）。
func (n *Notifier) Enqueue(m Message) {
	select {
	case n.ch <- m:
	default:
		log.Printf("notify: 队列已满，丢弃一条: %s", m.Subject)
	}
}

// Pending 队列中等待发送的条数。
func (n *Notifier) Pending() int { return len(n.ch) }

// Run 阻塞运行直到 ctx 结束并完成收尾。
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			n.flush()
			return
		case first := <-n.ch:
			batch := []Message{first}
			if n.window > 0 {
				timer := time.NewTimer(n.window)
			collect:
				for len(batch) < 30 {
					select {
					case m := <-n.ch:
						batch = append(batch, m)
					case <-timer.C:
						break collect
					case <-ctx.Done():
						break collect
					}
				}
				timer.Stop()
			}
			// 退出中（ctx 已被取消）：不能拿已取消的 ctx 去 deliver（waitRate 会直接丢弃）。
			// 把整批塞回通道，让下轮循环的 flush() 用 15s 收尾 ctx 统一尽力发完。
			// 已在退出路径上，阻塞写是安全的（engine 已停，不会再有 Enqueue 竞争）。
			if ctx.Err() != nil {
				for _, m := range batch {
					n.ch <- m
				}
				continue
			}
			n.deliver(ctx, digest(batch), true)
		}
	}
}

// flush 退出前尽力发完剩余消息，整体不超过 15s，不重试。
func (n *Notifier) flush() {
	var rest []Message
	for {
		select {
		case m := <-n.ch:
			rest = append(rest, m)
			continue
		default:
		}
		break
	}
	if len(rest) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	n.deliver(ctx, digest(rest), false)
}

func (n *Notifier) deliver(ctx context.Context, m Message, retry bool) {
	if !n.waitRate(ctx) {
		log.Printf("notify: 退出中，未能发送: %s", m.Subject)
		return
	}
	attempt := 0
	for {
		err := n.sender.SendMessage(m)
		n.sent = append(n.sent, time.Now())
		if err == nil {
			return
		}
		if !retry || attempt >= len(n.retries) {
			log.Printf("notify: 发送失败，已放弃: %q: %v", m.Subject, err)
			return
		}
		wait := n.retries[attempt]
		attempt++
		log.Printf("notify: 发送失败（%v），%s 后第 %d 次重试", err, wait, attempt)
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			// 收到退出信号：再尝试最后一次就走
			if err := n.sender.SendMessage(m); err != nil {
				log.Printf("notify: 退出前最后一次发送失败: %v", err)
			}
			return
		}
	}
}

// waitRate 滑动窗口限流。
func (n *Notifier) waitRate(ctx context.Context) bool {
	for {
		cut := time.Now().Add(-time.Minute)
		i := 0
		for i < len(n.sent) && n.sent[i].Before(cut) {
			i++
		}
		n.sent = n.sent[i:]
		if len(n.sent) < n.perMin {
			return true
		}
		wait := time.Until(n.sent[0].Add(time.Minute)) + 100*time.Millisecond
		log.Printf("notify: 达到每分钟 %d 封上限，等待 %s", n.perMin, wait.Round(time.Second))
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return false
		}
	}
}

// digest 把多条合成一封；单条原样返回。
func digest(batch []Message) Message {
	if len(batch) == 1 {
		return batch[0]
	}
	tags := map[string]bool{}
	for _, m := range batch {
		if m.Tag != "" {
			tags[m.Tag] = true
		}
	}
	names := make([]string, 0, len(tags))
	for t := range tags {
		names = append(names, t)
	}
	sort.Strings(names)
	subject := fmt.Sprintf("[binance-watch] %d 条提醒", len(batch))
	if len(names) > 0 {
		if len(names) > 4 {
			names = append(names[:4], "…")
		}
		subject += " · " + strings.Join(names, " ")
	}
	var text, cards strings.Builder
	allCards := true
	for i, m := range batch {
		if i > 0 {
			text.WriteString("\n" + strings.Repeat("─", 28) + "\n\n")
		}
		text.WriteString(m.Text)
		if m.Card == "" {
			allCards = false
		}
		cards.WriteString(m.Card)
	}
	out := Message{Subject: subject, Text: text.String()}
	if allCards {
		out.Card = cards.String()
	}
	return out
}
