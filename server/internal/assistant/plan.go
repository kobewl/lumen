package assistant

import (
	"encoding/json"
	"fmt"
	"strings"

	"lumen/server/internal/ai"
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

// 允许的时间范围取值。空串表示未指定。
var allowedTimeRanges = map[string]bool{
	"":             true,
	"today":        true,
	"yesterday":    true,
	"last_7_days":  true,
	"last_14_days": true,
	"custom":       true,
}

// Understanding 是模型对这句话的理解。它只是**计划的一部分**，
// 不是事实：证据仍然来自 Capability 的返回，不来自这里。
type Understanding struct {
	Goal      string   `json:"goal"`
	Entities  []string `json:"entities"`
	TimeRange string   `json:"time_range"`
	// Confidence 是模型对自己理解的置信度（0-1）。低置信度且需要数据时，
	// 计划应当选择 clarify 而不是硬猜。
	Confidence float64 `json:"confidence"`
}

// ToolCall 是模型请求调用的一次能力。
//
// 参数用 map[string]any 接收，但会被 Policy Gate 按能力自身的 schema 逐项校验，
// 未声明的参数一律拒绝——模型不能靠构造参数绕过限制。
type ToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// MemoryCandidate 是模型提议记录的一条候选记忆。
//
// 它**不会**直接成为记忆：只以 candidate 状态落库，等用户确认后才可能晋升。
type MemoryCandidate struct {
	Kind       string   `json:"kind"`
	Content    string   `json:"content"`
	SourceIDs  []string `json:"source_ids"`
	Confidence float64  `json:"confidence"`
}

// Plan 是模型产出的 AgentPlan，对应一段严格 JSON。
type Plan struct {
	Mode                  string            `json:"mode"`
	Understanding         Understanding     `json:"understanding"`
	ToolCalls             []ToolCall        `json:"tool_calls"`
	NeedsClarification    bool              `json:"needs_clarification"`
	ClarificationQuestion string            `json:"clarification_question"`
	MemoryCandidates      []MemoryCandidate `json:"memory_candidates"`
	ResponseStyle         string            `json:"response_style"`
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
//  2. 策略层（policy.go）：工具白名单、参数 schema、敏感内容、数量上限。
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

	var p Plan
	dec := json.NewDecoder(strings.NewReader(string(env.Plan)))
	// 拒绝未知字段：模型臆造的新字段必须显式失败，而不是被静默忽略后
	// 让代码以为计划里没有这一步。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Plan{}, planError("plan 字段结构不符: %v", err)
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
		return Plan{}, planError("recall 模式必须至少调用一个数据能力")
	}
	return p, nil
}

// ToolNames 返回计划里请求调用的能力名，用于日志与审计（不含参数）。
func (p Plan) ToolNames() []string {
	out := make([]string, 0, len(p.ToolCalls))
	for _, c := range p.ToolCalls {
		out = append(out, c.Name)
	}
	return out
}
