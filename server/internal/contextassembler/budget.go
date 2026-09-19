package contextassembler

// Budget 是一轮上下文的硬预算（runes）。
//
// 它是"送多少内容进模型"的唯一裁决点：所有段的上限与整包上限都在这里，
// 截断按固定优先级执行并留下元信息。零值字段在 Normalize 时取默认值，
// 因此测试只需要覆盖关心的字段。
type Budget struct {
	// ProfileRunes 是单条已确认信息值的上限。
	ProfileRunes int
	// ProfileMaxEntries 是注入的已确认信息条数上限。
	ProfileMaxEntries int
	// SummaryRunes 是会话摘要的上限。
	SummaryRunes int
	// RecentTurns 是注入的近期轮次数上限。
	RecentTurns int
	// TurnRunes 是单轮近期对话（用户话+回答合计）的上限。
	TurnRunes int
	// RecentTurnsRunes 是近期轮次合计上限。
	RecentTurnsRunes int
	// ToolResultRunes 是单个工具结果的上限（沿用旧提示词限额）。
	ToolResultRunes int
	// EvidenceRunes 是工具证据合计上限（沿用旧提示词限额）。
	EvidenceRunes int
	// UserTextRunes 是用户消息上限（沿用旧提示词限额）。
	UserTextRunes int
	// TotalRunes 是整包上限；超限按"证据 > 轮次 > 摘要 > Profile"顺序丢弃。
	TotalRunes int
}

// DefaultBudget 返回与旧提示词限额兼容、并加上新段上限的默认预算。
//
// 数值取向：旧链路里用户消息 2000、单工具 8000、工具合计 20000 的限额
// 保持不变（不回归既有行为）；新增段的默认值按"少而准"取向——
// 已确认信息与近期轮次是高价值低容量的内容，不是聊天记录回放。
func DefaultBudget() Budget {
	return Budget{
		ProfileRunes:      200,
		ProfileMaxEntries: 8,
		SummaryRunes:      300,
		RecentTurns:       3,
		TurnRunes:         400,
		RecentTurnsRunes:  1200,
		ToolResultRunes:   8000,
		EvidenceRunes:     20000,
		UserTextRunes:     2000,
		TotalRunes:        30000,
	}
}

// Normalize 补齐零值/非法值字段，保证预算永远可用且非零。
func (b Budget) Normalize() Budget {
	d := DefaultBudget()
	fixInt := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	b.ProfileRunes = fixInt(b.ProfileRunes, d.ProfileRunes)
	b.ProfileMaxEntries = fixInt(b.ProfileMaxEntries, d.ProfileMaxEntries)
	b.SummaryRunes = fixInt(b.SummaryRunes, d.SummaryRunes)
	b.RecentTurns = fixInt(b.RecentTurns, d.RecentTurns)
	b.TurnRunes = fixInt(b.TurnRunes, d.TurnRunes)
	b.RecentTurnsRunes = fixInt(b.RecentTurnsRunes, d.RecentTurnsRunes)
	b.ToolResultRunes = fixInt(b.ToolResultRunes, d.ToolResultRunes)
	b.EvidenceRunes = fixInt(b.EvidenceRunes, d.EvidenceRunes)
	b.UserTextRunes = fixInt(b.UserTextRunes, d.UserTextRunes)
	b.TotalRunes = fixInt(b.TotalRunes, d.TotalRunes)
	return b
}
