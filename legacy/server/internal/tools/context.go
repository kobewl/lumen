package tools

import (
	"context"
	"fmt"
	"time"

	"lumen/server/internal/identity"
	"lumen/server/internal/temporal"
	"lumen/server/internal/tooling"
)

// temporalOf 取出调用携带的可信时间快照。
//
// 执行器已经保证 RunInfo.Temporal 有效才执行工具；这里再挡一道，
// 是为了让"直接 new 一个工具来调用"的误用（绕过执行器）也大声失败，
// 而不是悄悄退回各自的 time.Now——那正是"中午说早呀"的根源。
func temporalOf(inv tooling.Invocation) (temporal.Context, error) {
	if !inv.Temporal.Valid() {
		return temporal.Context{}, fmt.Errorf("缺少可信时间上下文（必须经工具执行器调用）")
	}
	return inv.Temporal, nil
}

// currentTimeTool 读取当前时间与所处时段。
//
// 数据来自调用携带的可信时间快照（与 Planner/Synthesizer 同一份），
// 因此它给出的时段与 TemporalGuard 用的永远是同一个答案。
// 时间已在 <trusted_context> 提供后，它降级为"补充手段"。
type currentTimeTool struct{}

func (t *currentTimeTool) Spec() tooling.Spec {
	return tooling.Spec{
		Name: "get_current_time",
		Summary: "读取当前日期、时间与所处时段（早上/上午/中午/下午/晚上/深夜）。" +
			"当前时间通常已在系统的可信上下文里给出；只有需要更精确的时间信息时才调用它。",
		ResultSchema: `{"date":"2026-09-18","time":"13:19","weekday":"周五",` +
			`"day_part":"下午","timezone":"Asia/Shanghai","iso":"..."}`,
		Kind: tooling.KindSystemInfo,
		Risk: tooling.RiskRead,
	}
}

// CurrentTimeResult 是 get_current_time 的返回结果。
type CurrentTimeResult struct {
	Date     string `json:"date"`
	Time     string `json:"time"`
	Weekday  string `json:"weekday"`
	DayPart  string `json:"day_part"`
	Timezone string `json:"timezone"`
	ISO      string `json:"iso"`
}

func (t *currentTimeTool) Execute(_ context.Context, inv tooling.Invocation) (tooling.Result, error) {
	tc, err := temporalOf(inv)
	if err != nil {
		return tooling.Result{}, err
	}
	now := tc.Now
	out := CurrentTimeResult{
		Date:     now.Format("2006-01-02"),
		Time:     now.Format("15:04"),
		Weekday:  temporal.WeekdayCN(now),
		DayPart:  tc.DayPart,
		Timezone: tc.Loc.String(),
		ISO:      now.Format(time.RFC3339),
	}

	return tooling.Result{
		Model:  out,
		Digest: []string{fmt.Sprintf("当前时间：%s %s（%s，%s）", out.Date, out.Time, out.Weekday, out.DayPart)},
	}, nil
}

// assistantProfileTool 返回助手身份配置。
//
// 身份必须是可调用的数据源：模型据此组织回答，而不是靠代码或提示词里写死的
// 「你叫 Lumen」。换部署环境只改配置，回答里的名字、定位、语气就跟着变。
type assistantProfileTool struct {
	profile identity.Profile
}

func (t *assistantProfileTool) Spec() tooling.Spec {
	return tooling.Spec{
		Name: "get_assistant_profile",
		Summary: "读取助手自己的身份配置（名字、定位、称呼、语言、语气、主动性）。" +
			"回答“你是谁/你叫什么/你能做什么/你什么时候会主动说话”这类问题时先调用它，身份信息只能来自这里。无参数。",
		ResultSchema: `{"name":"小灯","role":"个人助手/伙伴","owner_display_name":"liang",` +
			`"language":"zh-CN","tone":"友好、简洁","proactivity":"..."}`,
		Kind: tooling.KindSystemInfo,
		Risk: tooling.RiskRead,
	}
}

// AssistantProfileResult 是 get_assistant_profile 的返回结果。
type AssistantProfileResult struct {
	Name             string `json:"name"`
	Role             string `json:"role"`
	OwnerDisplayName string `json:"owner_display_name,omitempty"`
	Language         string `json:"language"`
	Tone             string `json:"tone"`
	Proactivity      string `json:"proactivity"`
}

func (t *assistantProfileTool) Execute(_ context.Context, _ tooling.Invocation) (tooling.Result, error) {
	p := t.profile.Normalize()
	out := AssistantProfileResult{
		Name: p.Name, Role: p.Role, OwnerDisplayName: p.OwnerDisplayName,
		Language: p.Language, Tone: p.Tone, Proactivity: p.Proactivity,
	}
	digest := fmt.Sprintf("配置里的身份：名字 %s，定位 %s，语言 %s，语气 %s。",
		p.Name, p.Role, p.Language, p.Tone)
	return tooling.Result{Model: out, Digest: []string{digest}}, nil
}

// conversationStateTool 读取**有限的**跨轮对话状态。
//
// 与提示词里回填的状态是同一份数据，但它是可调用、可审计的：
// 用户问"我们刚才在说哪个项目"时，模型读到的不是自己的记忆，而是一条读出来的记录。
type conversationStateTool struct {
	store Store
}

func (t *conversationStateTool) Spec() tooling.Spec {
	return tooling.Spec{
		Name: "get_conversation_state",
		Summary: "读取与当前用户的对话状态：上一轮提到的项目、上一轮问过但还没得到回答的问题、" +
			"上一轮的时间范围。只有这三项，没有聊天记录原文。无参数（用户身份由系统注入）。",
		ResultSchema: `{"current_project":"lumen","pending_question":"","last_time_range":"today","updated_at":"..."}`,
		Kind:         tooling.KindSystemInfo,
		Risk:         tooling.RiskRead,
	}
}

// ConversationStateResult 是 get_conversation_state 的返回结果。
type ConversationStateResult struct {
	CurrentProject  string `json:"current_project,omitempty"`
	PendingQuestion string `json:"pending_question,omitempty"`
	LastTimeRange   string `json:"last_time_range,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
	// Note 说明"没有状态"是正常的，避免模型把空当成错误。
	Note string `json:"note,omitempty"`
}

func (t *conversationStateTool) Execute(ctx context.Context, inv tooling.Invocation) (tooling.Result, error) {
	if t.store == nil || inv.Actor == "" {
		return tooling.Result{Model: ConversationStateResult{Note: "没有可用的对话状态。"}}, nil
	}
	state, err := t.store.ConversationStateByUser(ctx, inv.Actor)
	if err != nil {
		return tooling.Result{}, err
	}
	out := ConversationStateResult{
		PendingQuestion: state.PendingQuestion,
		LastTimeRange:   state.LastTimeRange,
	}
	// 空状态就是"没有"，不能显示成"暂未识别项目"——那是"有记录但没归类"的意思。
	if state.CurrentProject != "" {
		out.CurrentProject = DisplayProject(state.CurrentProject)
	}
	if !state.UpdatedAt.IsZero() {
		out.UpdatedAt = state.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if out.CurrentProject == "" && out.PendingQuestion == "" && out.LastTimeRange == "" {
		out.Note = "这是第一次对话，还没有历史状态。"
	}
	return tooling.Result{
		Model:  out,
		Digest: []string{conversationDigest(out)},
	}, nil
}

func conversationDigest(out ConversationStateResult) string {
	if out.Note != "" {
		return "对话状态：这是第一次对话，没有历史状态。"
	}
	text := "对话状态："
	if out.CurrentProject != "" {
		text += "上一轮的项目「" + out.CurrentProject + "」；"
	}
	if out.PendingQuestion != "" {
		text += "上一轮问过「" + truncateRunes(out.PendingQuestion, 60) + "」；"
	}
	if out.LastTimeRange != "" {
		text += "上一轮的时间范围 " + out.LastTimeRange
	}
	return text
}

// 保留给展示层使用的时区兜底；工具内部一律以可信时间快照为准。
func safeLocation(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}
