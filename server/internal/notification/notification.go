// Package notification 定义通知渠道抽象。
//
// V0.1 只有飞书一个 provider，但接口化后：
//   - 总结推送逻辑不依赖飞书 SDK，便于测试与将来替换渠道；
//   - 飞书不可用时，总结保存流程不受影响。
package notification

import (
	"context"
	"errors"
)

// Sender 是消息发送渠道。
type Sender interface {
	// Name 返回渠道名称，用于 delivery 去重键。
	Name() string
	// SendText 向指定用户发送纯文本消息。
	SendText(ctx context.Context, userID, text string) error
	// Enabled 表示渠道是否具备发送条件。
	Enabled() bool
}

// ErrDisabled 表示渠道未启用。
var ErrDisabled = errors.New("notification channel disabled")

// SenderFunc 用函数实现 Sender，便于测试注入与轻量适配。
type SenderFunc struct {
	NameFunc    func() string
	EnabledFunc func() bool
	SendFunc    func(ctx context.Context, userID, text string) error
}

// Name 实现 Sender。
func (f SenderFunc) Name() string {
	if f.NameFunc == nil {
		return "func"
	}
	return f.NameFunc()
}

// Enabled 实现 Sender。
func (f SenderFunc) Enabled() bool {
	if f.EnabledFunc == nil {
		return true
	}
	return f.EnabledFunc()
}

// SendText 实现 Sender。
func (f SenderFunc) SendText(ctx context.Context, userID, text string) error {
	if f.SendFunc == nil {
		return nil
	}
	return f.SendFunc(ctx, userID, text)
}

// MultiSender 依次向多个渠道发送，任一成功即算成功。
type MultiSender struct {
	senders []Sender
}

// NewMultiSender 创建多渠道路由。
func NewMultiSender(senders ...Sender) *MultiSender {
	out := make([]Sender, 0, len(senders))
	for _, s := range senders {
		if s != nil {
			out = append(out, s)
		}
	}
	return &MultiSender{senders: out}
}

// Name 返回组合渠道名称。
func (m *MultiSender) Name() string { return "multi" }

// Enabled 表示是否至少有一个可用渠道。
func (m *MultiSender) Enabled() bool {
	for _, s := range m.senders {
		if s.Enabled() {
			return true
		}
	}
	return false
}

// SendText 依次尝试各渠道，全部失败时返回最后一个错误。
func (m *MultiSender) SendText(ctx context.Context, userID, text string) error {
	if len(m.senders) == 0 {
		return ErrDisabled
	}
	var lastErr error
	for _, s := range m.senders {
		if !s.Enabled() {
			continue
		}
		if err := s.SendText(ctx, userID, text); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		return ErrDisabled
	}
	return lastErr
}
