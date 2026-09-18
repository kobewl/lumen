package assistant

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lumen/server/internal/storage"
)

// StateView 是给模型看的有限对话状态。
//
// 只包含解析代词真正需要的东西：当前项目与上一轮待澄清的问题。
// 完整聊天历史不进上下文——既省 token，也避免模型把旧内容当成新事实。
type StateView struct {
	CurrentProject  string
	PendingQuestion string
	LastTimeRange   string
}

// view 把存储状态转成模型可见视图。
func stateView(c storage.ConversationState) StateView {
	return StateView{
		CurrentProject:  c.CurrentProject,
		PendingQuestion: c.PendingQuestion,
		LastTimeRange:   c.LastTimeRange,
	}
}

// PromptBlock 渲染状态块；没有可提供的信息时返回空串。
func (v StateView) PromptBlock() string {
	var lines []string
	if v.CurrentProject != "" {
		lines = append(lines, fmt.Sprintf(
			"- 上一轮提到的项目：「%s」（用户说“那个项目/它的进展”时很可能指它）", v.CurrentProject))
	}
	if v.PendingQuestion != "" {
		lines = append(lines, fmt.Sprintf(
			"- 你上一轮问过用户：「%s」（本轮用户的话可能是对它的回答）", v.PendingQuestion))
	}
	if v.LastTimeRange != "" {
		lines = append(lines, fmt.Sprintf("- 上一轮的时间范围：%s", v.LastTimeRange))
	}
	if len(lines) == 0 {
		return ""
	}
	return "已知的对话上下文：\n" + strings.Join(lines, "\n") + "\n"
}

// MinimalFallback 是模型完全不可用时的最小安全兜底。
//
// 它的定位被严格限制：**不是**产品入口，也**不是**另一套关键词机器人。
// 它只做一件最保守的事——把用户最明确、最不可能误判的几种"要数据"的说法
// 映射到只读检索，其余一律诚实说明模型不可用。
//
// 之所以保留这层：模型不可用时如果完全答不了"今天做了什么"，
// 用户会以为记录丢了。宁可给一份确定性的事实摘要，也不要让人怀疑数据。
type MinimalFallback struct {
	Store *storage.Store
	Loc   *time.Location
}

// CanHandle 判断这句话是否属于"最明确的数据请求"。
func (f *MinimalFallback) CanHandle(text string) bool {
	if f == nil || f.Store == nil {
		return false
	}
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	// 只认最直白的说法，宁可不匹配也不要误判。
	for _, kw := range []string{
		"今天做了什么", "今天干了什么", "今天怎么样",
		"昨天做了什么", "昨天干了什么",
	} {
		if strings.Contains(t, kw) {
			return true
		}
	}
	return false
}

// Handle 返回确定性的事实摘要。
func (f *MinimalFallback) Handle(ctx context.Context) (string, error) {
	loc := f.Loc
	if loc == nil {
		loc = time.UTC
	}
	date := time.Now().In(loc).Format("2006-01-02")
	sessions, err := f.Store.SessionsByDate(ctx, date)
	if err != nil {
		return "", err
	}

	views := make([]SessionView, 0, len(sessions))
	var total float64
	for _, s := range sessions {
		v := sessionToView(s, loc)
		total += v.DurationMins
		views = append(views, v)
	}

	results := []CapabilityResult{{
		Name: "get_today_status",
		Value: TodayStatusResult{
			Date: date, Sessions: views, TotalMinutes: round1(total),
		},
	}}
	body := renderFacts(results, loc)
	if body == "" {
		body = fmt.Sprintf("（%s）我这边没有查到记录。", date)
	}
	return body + "\n\n" + evidenceLine(results), nil
}
