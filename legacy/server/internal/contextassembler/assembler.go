package contextassembler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"lumen/server/internal/identity"
	"lumen/server/internal/storage"
	"lumen/server/internal/temporal"
	"lumen/server/internal/ulid"
)

// Store 是装配所需的窄存储接口（*storage.Store 满足它）。
//
// 刻意只暴露三个读方法：候选记忆表（memory_candidates）**不在其中**——
// 候选内容在任何情况下都不允许进入模型上下文，接口层面就没有这条读取路径。
type Store interface {
	ProfileEntries(ctx context.Context, userID string) ([]storage.ProfileEntry, error)
	ConversationStateByUser(ctx context.Context, userID string) (storage.ConversationState, error)
	RecentConversations(ctx context.Context, userID string, limit int) ([]storage.RecentTurn, error)
}

// Request 是一轮装配的输入。只有这三个字段：装配所需的一切
// （时间快照、用户消息）必须由调用方显式给出，
// 装配器不自己取 time.Now、不猜用户身份。
type Request struct {
	// UserID 是数据归属者；为空时视为无归属（不读任何用户数据）。
	UserID string
	// Temporal 是本轮可信时间快照（由注入时钟生成，一轮一个）。
	Temporal temporal.Context
	// UserText 是本轮用户消息原文（装配器负责清洗）。
	UserText string
}

// Assembler 按预算装配每轮上下文。
type Assembler struct {
	store    Store
	identity identity.Profile
	budget   Budget
	logger   *slog.Logger
}

// NewAssembler 创建装配器。store 可为 nil（全部降级为空段）；profile 必须可用。
func NewAssembler(store Store, id identity.Profile, budget Budget, logger *slog.Logger) *Assembler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Assembler{
		store:    store,
		identity: id.Normalize(),
		budget:   budget.Normalize(),
		logger:   logger,
	}
}

// Budget 返回装配使用的预算（测试与观测用）。
func (a *Assembler) Budget() Budget { return a.budget }

// Assemble 装配一轮 ContextBundle。
//
// 填充顺序即优先级（Profile > 摘要 > 近期轮次），整包超限时从最低优先级
// 开始丢弃并记录元信息。存储读失败时对应段降级为空，轮次继续——
// 缺一段上下文不应该让整轮对话失败。
//
// Temporal 无效时仍然返回 Bundle（提示词会如实说明缺少时间快照），
// 工具执行器会 fail-closed 拒绝全部调用。
func (a *Assembler) Assemble(ctx context.Context, req Request) *Bundle {
	b := &Bundle{
		budget:     a.budget,
		Temporal:   req.Temporal,
		Identity:   a.identity,
		UserText:   sanitizeUserText(req.UserText),
		TurnSource: "u_" + ulid.New(),
	}
	// 预算账本从"trusted 基础（时间+身份）+ 用户消息"起步，
	// 之后 Profile/摘要/轮次/证据按优先级依次占用。
	b.used = b.baseRunes()

	if a.store == nil || strings.TrimSpace(req.UserID) == "" {
		b.addTruncation("存储或用户标识缺失：本轮不注入已确认信息、会话摘要与近期轮次")
		return b
	}

	// 1. 已确认信息（最高优先级，最先填）。
	entries, err := a.store.ProfileEntries(ctx, req.UserID)
	if err != nil {
		a.logger.Warn("读取已确认信息失败，本轮不注入", "error", err.Error())
		b.addTruncation("已确认信息读取失败，本轮不注入")
	} else {
		b.fillProfile(entries)
	}

	// 2. 会话摘要。
	state, err := a.store.ConversationStateByUser(ctx, req.UserID)
	if err != nil {
		a.logger.Warn("读取会话摘要失败，本轮不注入", "error", err.Error())
		b.addTruncation("会话摘要读取失败，本轮不注入")
	} else {
		b.fillSummary(state)
	}

	// 3. 近期轮次。
	if a.budget.RecentTurns > 0 {
		turns, err := a.store.RecentConversations(ctx, req.UserID, a.budget.RecentTurns)
		if err != nil {
			a.logger.Warn("读取近期轮次失败，本轮不注入", "error", err.Error())
			b.addTruncation("近期轮次读取失败，本轮不注入")
		} else {
			b.fillTurns(turns)
		}
	}
	return b
}

// fillProfile 按预算填入已确认信息。整包超限时停填并记录剩余条数。
//
// 预算记账使用与渲染**同一**的编码输出（profileLine）：值转义后可能变长，
// 账本按最终进提示词的长度计，保证 TotalRunes 是对真实渲染的承诺。
func (b *Bundle) fillProfile(entries []storage.ProfileEntry) {
	if len(entries) == 0 {
		return
	}
	clipped := 0
	kept := 0
	for _, e := range entries {
		if kept >= b.budget.ProfileMaxEntries {
			b.addTruncation(fmt.Sprintf("已确认信息：超过条数上限 %d，剩余 %d 条未注入",
				b.budget.ProfileMaxEntries, len(entries)-kept))
			break
		}
		value, valueClipped := ClipRunes(e.Value, b.budget.ProfileRunes)
		if valueClipped {
			clipped++
		}
		entry := ProfileEntry{Key: e.Key, Value: value, Version: e.Version}
		line := profileLine(entry)
		if b.used+len([]rune(line)) > b.budget.TotalRunes {
			b.addTruncation(fmt.Sprintf("已确认信息：超出整包预算，剩余 %d 条未注入", len(entries)-kept))
			break
		}
		b.used += len([]rune(line))
		b.profile = append(b.profile, entry)
		kept++
	}
	if clipped > 0 {
		b.addTruncation(fmt.Sprintf("已确认信息：%d 条内容超长，已裁剪到每条 %d 字", clipped, b.budget.ProfileRunes))
	}
}

// fillSummary 填入会话摘要。
//
// 摘要字段是模型输出的回声，裁剪策略是**整字段丢弃**（按
// LastTimeRange → PendingQuestion → CurrentProject 的优先级从尾部丢），
// 绝不渲染半截字段；记账与渲染用同一份编码输出（summaryTrustedLines）。
func (b *Bundle) fillSummary(state storage.ConversationState) {
	summary := ConversationSummary{
		CurrentProject:  state.CurrentProject,
		PendingQuestion: state.PendingQuestion,
		LastTimeRange:   state.LastTimeRange,
	}
	if summaryTrustedLines(summary) == "" {
		return
	}
	startFields := summaryFieldCount(summary)
	dropped := 0
	for summaryTrustedLines(summary) != "" {
		lines := summaryTrustedLines(summary)
		n := len([]rune(lines))
		if n <= b.budget.SummaryRunes && b.used+n <= b.budget.TotalRunes {
			if dropped > 0 {
				b.addTruncation(fmt.Sprintf(
					"会话摘要：超出段上限/整包预算，丢弃 %d 个放不下的字段（剩 %d 个）",
					dropped, startFields-dropped))
			}
			b.used += n
			b.summary = summary
			return
		}
		summary = dropLastSummaryField(summary)
		dropped++
	}
	b.addTruncation(fmt.Sprintf("会话摘要：超出段上限/整包预算，%d 个字段全部未注入", startFields))
}

// dropLastSummaryField 丢弃摘要里优先级最低的仍有值字段。
func dropLastSummaryField(s ConversationSummary) ConversationSummary {
	switch {
	case s.LastTimeRange != "":
		s.LastTimeRange = ""
	case s.PendingQuestion != "":
		s.PendingQuestion = ""
	default:
		s.CurrentProject = ""
	}
	return s
}

// summaryFieldCount 统计摘要里有值的字段数。
func summaryFieldCount(s ConversationSummary) int {
	n := 0
	for _, v := range []string{s.CurrentProject, s.PendingQuestion, s.LastTimeRange} {
		if v != "" {
			n++
		}
	}
	return n
}

// fillTurns 填入近期轮次（存储返回最新在前，这里转成旧→新）。
func (b *Bundle) fillTurns(turns []storage.RecentTurn) {
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	turnsUsed := 0
	kept := 0
	for _, t := range turns {
		if kept >= b.budget.RecentTurns {
			b.addTruncation(fmt.Sprintf("近期轮次：超过轮数上限 %d，其余未注入", b.budget.RecentTurns))
			break
		}
		body, block, ok := b.turnBody(t, turnsUsed)
		if !ok {
			b.addTruncation(fmt.Sprintf("近期轮次：超出轮次/整包预算，剩余 %d 轮未注入", len(turns)-kept))
			break
		}
		if strings.TrimSpace(body) == "" {
			continue // 空轮次跳过，不算占用也不算进度
		}
		b.turns = append(b.turns, EvidenceItem{Source: "conversation", Kind: KindRecentTurn, Body: body})
		turnsUsed += len([]rune(block))
		kept++
	}
}

// turnBody 渲染单轮近期对话，并按单轮/轮次合计/整包三级预算裁决。
// ok=false 表示预算已尽，后续轮次全部放弃。
func (b *Bundle) turnBody(t storage.RecentTurn, turnsUsed int) (body, block string, ok bool) {
	user := strings.TrimSpace(t.UserText)
	answer := strings.TrimSpace(t.AnswerText)
	if user == "" && answer == "" {
		return "", "", true
	}
	user, _ = ClipRunes(user, b.budget.TurnRunes/2)
	answer, _ = ClipRunes(answer, b.budget.TurnRunes/2)
	body = "用户：" + user
	if answer != "" {
		body += "\n你（上一轮回答）：" + answer
	}
	block = UntrustedBlock(TagRecentTurns, `source="conversation"`, body)
	n := len([]rune(block))
	if turnsUsed+n > b.budget.RecentTurnsRunes || b.used+n > b.budget.TotalRunes {
		return "", "", false
	}
	b.used += n
	return body, block, true
}

// sanitizeUserText 清洗用户输入：去首尾空白。
func sanitizeUserText(text string) string {
	return strings.TrimSpace(text)
}
