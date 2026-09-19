package assistant

import (
	"encoding/json"
	"fmt"
	"strings"

	"lumen/server/internal/ai"
	"lumen/server/internal/tooling"
)

// 计划模式。代码只认这几种，其余一律拒绝。
const (
	// ModeChat：不需要检索数据的对话（问候、身份、能力、闲聊、无法归类的话）。
	ModeChat = "chat"
	// ModeRecall：需要检索记录才能回答（今天做了/项目进展/某个时间段）。
	ModeRecall = "recall"
	// ModeClarify：信息不足，需要先向用户澄清。
	ModeClarify = "clarify"
	// ModeSignoff：用户表示今天到此为止（下班、收工）。
	ModeSignoff = "signoff"
)

// allowedTimeRanges 是时间范围白名单。空串表示未指定。
var allowedTimeRanges = map[string]bool{
	"":             true,
	"today":        true,
	"yesterday":    true,
	"last_7_days":  true,
	"last_14_days": true,
	"custom":       true,
}

// Understanding 是模型对这句话的理解。它只是**计划的一部分**，
// 不是事实：证据仍然来自工具的返回，不来自这里。
type Understanding struct {
	Goal      string   `json:"goal"`
	Entities  []string `json:"entities"`
	TimeRange string   `json:"time_range"`
	// Confidence 是模型对自己理解的置信度（0-1）。低置信度且需要数据时，
	// 计划应当选择 clarify 而不是硬猜。
	Confidence float64 `json:"confidence"`
}

// 计划的 JSON 形状。
//
// 工具调用直接复用 tooling.Call：模型输出与策略输入是同一个形状，
// 中间不需要再转一层（转换层是"参数在某处被悄悄改写"的常见来源）。
// Plan 自身则用自定义 UnmarshalJSON 做严格解析，因此这里只作用于模型输出。
type planDoc struct {
	Mode                  string         `json:"mode"`
	Understanding         Understanding  `json:"understanding"`
	ToolCalls             []tooling.Call `json:"tool_calls"`
	NeedsClarification    bool           `json:"needs_clarification"`
	ClarificationQuestion string         `json:"clarification_question"`
	ResponseStyle         string         `json:"response_style"`
}

// Plan 是模型产出的 AgentPlan。
type Plan struct {
	Mode                  string
	Understanding         Understanding
	ToolCalls             []tooling.Call
	NeedsClarification    bool
	ClarificationQuestion string
	ResponseStyle         string
}

// PlanParseError 表示模型输出无法作为合法计划使用。
//
// 与普通错误的区别：这类错误一律走安全降级，绝不"尽力解释"——
// 一个格式不对的计划说明模型这轮不可靠，猜它的意图比拒绝更危险。
type PlanParseError struct {
	Reason string
}

func (e *PlanParseError) Error() string { return "计划不可用: " + e.Reason }

func planError(format string, args ...any) *PlanParseError {
	return &PlanParseError{Reason: fmt.Sprintf(format, args...)}
}

// planEnvelope 是模型输出的外层结构。
//
// 只接受 {"plan": {...}} 这一种形态：早期版本直接解析裸对象，
// 结果模型偶尔会夹杂寒暄或解释文字，解析失败却没有清晰原因。
type planEnvelope struct {
	Plan json.RawMessage `json:"plan"`
}

// ParsePlan 解析并校验模型输出的计划。
//
// 校验分两层：
//  1. 结构层（这里）：JSON 合法性、未知字段拒绝、模式与时间范围白名单；
//  2. 策略层（tooling.PolicyGate）：工具白名单、参数 schema、数量上限。
//
// 结构层只保证"这是一份能读懂的计划"，不判断"该不该执行"。
func ParsePlan(content string) (Plan, error) {
	raw, err := ai.ExtractJSON(content)
	if err != nil {
		return Plan{}, planError("输出不是合法 JSON: %v", err)
	}

	var env planEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Plan{}, planError("缺少 plan 字段或结构不符: %v", err)
	}
	if len(env.Plan) == 0 {
		return Plan{}, planError("plan 字段为空")
	}

	var doc planDoc
	dec := json.NewDecoder(strings.NewReader(string(env.Plan)))
	// 拒绝未知字段：模型臆造的新字段必须显式失败，而不是被静默忽略后
	// 让代码以为计划里没有这一步。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return Plan{}, planError("plan 字段结构不符: %v", err)
	}

	p := Plan{
		Mode:                  doc.Mode,
		Understanding:         doc.Understanding,
		ToolCalls:             doc.ToolCalls,
		NeedsClarification:    doc.NeedsClarification,
		ClarificationQuestion: doc.ClarificationQuestion,
		ResponseStyle:         doc.ResponseStyle,
	}

	switch p.Mode {
	case ModeChat, ModeRecall, ModeClarify, ModeSignoff:
	default:
		return Plan{}, planError("未知模式 %q", p.Mode)
	}

	if !allowedTimeRanges[p.Understanding.TimeRange] {
		return Plan{}, planError("未知时间范围 %q", p.Understanding.TimeRange)
	}
	if p.Understanding.Confidence < 0 || p.Understanding.Confidence > 1 {
		return Plan{}, planError("confidence 必须在 0-1 之间")
	}
	if p.NeedsClarification && strings.TrimSpace(p.ClarificationQuestion) == "" {
		return Plan{}, planError("声明需要澄清但没有给出问题")
	}
	if len([]rune(p.ClarificationQuestion)) > 300 {
		p.ClarificationQuestion = string([]rune(p.ClarificationQuestion)[:300])
	}

	// 计划自洽性：recall 模式没有工具调用就没有数据可依据，说明这份计划
	// 是残缺的。这种情况不猜测补齐，直接判为不可用并走安全降级。
	if p.Mode == ModeRecall && len(p.ToolCalls) == 0 {
		return Plan{}, planError("recall 模式必须至少调用一个工具")
	}
	return p, nil
}

// ToolNames 返回计划里请求调用的工具名，用于日志与审计（不含参数）。
func (p Plan) ToolNames() []string {
	out := make([]string, 0, len(p.ToolCalls))
	for _, c := range p.ToolCalls {
		out = append(out, c.Name)
	}
	return out
}
