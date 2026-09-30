// Package initiative 是 Lumen 的**低打扰主动关怀**本地闭环。
//
// 目标很具体：让助手在合适的时机、以有依据的方式主动问一句关心的话，
// 同时保证"少打扰"不是一句提示词，而是代码守门的策略：
//
//	开关（默认关）→ 静默时段（22:30–08:30 不打扰）→ 每天最多 2 次
//	→ 两次尝试间隔至少 4 小时 → 幂等 → 出站草稿/outbox → 审计
//
// 模型只做语义判断（基于可信时间、有限对话状态与已核实事实，产出 0 或 1 条
// 关怀问题）；频率、投递、证据等级全部由本包与工具执行器强制。
// 默认 dry-run：写了 outbox 但不真发，真实发送需要显式配置两道开关。
package initiative

import (
	"fmt"
	"time"

	"lumen/server/internal/temporal"
)

// Policy 是主动关怀的频率与打扰策略（纯代码，无 IO，可穷举测试）。
//
// 字段全部有默认值（DefaultPolicy）；刻意不给运行时改频率的口子——
// "低打扰"是产品承诺，不是可调参数。
type Policy struct {
	// Enabled 是总开关。false 时一切都不发生。
	Enabled bool
	// QuietStart/QuietEnd 是静默时段（本地时间，分钟数）。
	// 默认 22:30–08:30：深夜与清晨绝不打扰。
	QuietStartMinutes int
	QuietEndMinutes   int
	// MaxPerDay 是每天最多"发出"（含演练）的条数，按用户计。
	MaxPerDay int
	// MinInterval 是两次尝试之间的最短间隔。
	MinInterval time.Duration
}

// DefaultPolicy 返回产品默认策略：开关关闭（由配置显式打开）、
// 22:30–08:30 静默、每天最多 2 次、间隔至少 4 小时。
func DefaultPolicy() Policy {
	return Policy{
		Enabled:           false,
		QuietStartMinutes: 22*60 + 30, // 22:30
		QuietEndMinutes:   8*60 + 30,  // 08:30
		MaxPerDay:         2,
		MinInterval:       4 * time.Hour,
	}
}

// Decision 是策略判定结果。
type Decision struct {
	// Allowed 表示可以继续（取事实、调模型）。
	Allowed bool
	// Reason 是不允许的原因（可读中文，进日志与端点响应）。
	Reason string
}

// Decide 按可信时间与历史用量判定本次触发是否允许。
//
// force 跳过**频率类**限制（静默/当日上限/间隔），供本地验收连跑；
// 它跳不过总开关——Enabled=false 时 force 也无济于事，
// "功能关闭"不是一个可以被绕过的频率限制。
func (p Policy) Decide(tc temporal.Context, sentToday int, lastAttempt time.Time, haveLast bool, force bool) Decision {
	if !p.Enabled {
		return Decision{Reason: "主动关怀未启用"}
	}
	if !tc.Valid() {
		return Decision{Reason: "缺少可信时间"}
	}
	if force {
		return Decision{Allowed: true}
	}

	minutes := tc.Now.Hour()*60 + tc.Now.Minute()
	if inQuiet(minutes, p.QuietStartMinutes, p.QuietEndMinutes) {
		return Decision{Reason: fmt.Sprintf("静默时段（%02d:%02d–%02d:%02d）不打扰",
			p.QuietStartMinutes/60, p.QuietStartMinutes%60,
			p.QuietEndMinutes/60, p.QuietEndMinutes%60)}
	}
	if sentToday >= p.MaxPerDay {
		return Decision{Reason: fmt.Sprintf("今天已发送 %d 次，达到每日上限", sentToday)}
	}
	if haveLast && !lastAttempt.IsZero() {
		// 间隔必须以**可信时间**为基准，而不是真实墙钟（time.Since）：
		// 否则固定时钟测试与本地验收的时钟覆盖都会失真。
		if elapsed := tc.Now.Sub(lastAttempt); elapsed < p.MinInterval {
			return Decision{Reason: fmt.Sprintf("间隔限制：距上次尝试不足 %s", p.MinInterval)}
		}
	}
	return Decision{Allowed: true}
}

// inQuiet 判断是否处于静默时段。区间跨午夜（22:30–08:30）：
// "不早于开始 或 早于结束"。
func inQuiet(now, start, end int) bool {
	if start == end {
		return false
	}
	if start < end {
		return now >= start && now < end
	}
	return now >= start || now < end
}
