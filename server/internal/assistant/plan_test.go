package assistant

import (
	"strings"
	"testing"
)

// TestParsePlanAcceptsValidPlan 覆盖一份完整合法计划的解析。
func TestParsePlanAcceptsValidPlan(t *testing.T) {
	content := `{"plan":{
		"mode":"recall",
		"understanding":{"goal":"想知道今天做了什么","entities":["lumen"],"time_range":"today","confidence":0.9},
		"tool_calls":[{"name":"get_today_status","arguments":{}}],
		"needs_clarification":false,
		"clarification_question":"",
		"memory_candidates":[],
		"response_style":"简洁列出应用与时长"
	}}`

	plan, err := ParsePlan(content)
	if err != nil {
		t.Fatalf("合法计划不应报错: %v", err)
	}
	if plan.Mode != ModeRecall {
		t.Fatalf("模式应为 recall，实际 %s", plan.Mode)
	}
	if got := plan.ToolNames(); len(got) != 1 || got[0] != "get_today_status" {
		t.Fatalf("工具调用应为 get_today_status，实际 %v", got)
	}
}

// TestParsePlanRejectsUnknownField 覆盖"模型臆造字段必须显式失败"。
//
// 静默忽略未知字段会让代码以为计划里没有这一步，是危险的默认行为。
func TestParsePlanRejectsUnknownField(t *testing.T) {
	content := `{"plan":{"mode":"chat","understanding":{"goal":"x","entities":[],"time_range":"","confidence":0.5},
		"tool_calls":[],"needs_clarification":false,"clarification_question":"",
		"memory_candidates":[],"response_style":"","run_sql":"SELECT * FROM sessions"}}`

	if _, err := ParsePlan(content); err == nil {
		t.Fatal("含未知字段的计划必须被拒绝")
	}
}

// TestParsePlanRejectsUnknownMode 覆盖模式白名单。
func TestParsePlanRejectsUnknownMode(t *testing.T) {
	content := `{"plan":{"mode":"execute","understanding":{"goal":"x","entities":[],"time_range":"","confidence":0.5},
		"tool_calls":[],"needs_clarification":false,"clarification_question":"",
		"memory_candidates":[],"response_style":""}}`

	if _, err := ParsePlan(content); err == nil {
		t.Fatal("未知模式必须被拒绝")
	}
}

// TestParsePlanRejectsUnknownTimeRange 覆盖时间范围白名单。
func TestParsePlanRejectsUnknownTimeRange(t *testing.T) {
	content := `{"plan":{"mode":"chat","understanding":{"goal":"x","entities":[],"time_range":"last_90_days","confidence":0.5},
		"tool_calls":[],"needs_clarification":false,"clarification_question":"",
		"memory_candidates":[],"response_style":""}}`

	if _, err := ParsePlan(content); err == nil {
		t.Fatal("未知时间范围必须被拒绝")
	}
}

// TestParsePlanRejectsRecallWithoutTools 覆盖计划自洽性检查。
//
// recall 意味着"要读记录才能回答"，却没有工具调用，这份计划是残缺的；
// 代码不猜测补齐，直接拒绝并走安全降级。
func TestParsePlanRejectsRecallWithoutTools(t *testing.T) {
	content := `{"plan":{"mode":"recall","understanding":{"goal":"今天做了什么","entities":[],"time_range":"today","confidence":0.9},
		"tool_calls":[],"needs_clarification":false,"clarification_question":"",
		"memory_candidates":[],"response_style":""}}`

	if _, err := ParsePlan(content); err == nil {
		t.Fatal("recall 模式没有工具调用必须被拒绝")
	}
}

// TestParsePlanRejectsClarifyWithoutQuestion 覆盖澄清提问缺失。
func TestParsePlanRejectsClarifyWithoutQuestion(t *testing.T) {
	content := `{"plan":{"mode":"clarify","understanding":{"goal":"指代不清","entities":[],"time_range":"","confidence":0.3},
		"tool_calls":[],"needs_clarification":true,"clarification_question":"  ",
		"memory_candidates":[],"response_style":""}}`

	if _, err := ParsePlan(content); err == nil {
		t.Fatal("声明需要澄清却没有问题时必须被拒绝")
	}
}

// TestParsePlanRequiresEnvelope 覆盖 {"plan":{...}} 包装要求。
func TestParsePlanRequiresEnvelope(t *testing.T) {
	// 裸对象：早期版本接受这种形态，但模型偶尔会夹杂说明文字，
	// 解析失败的原因变得难以定位，因此统一要求包装。
	bare := `{"mode":"chat","understanding":{"goal":"x","entities":[],"time_range":"","confidence":0.5},
		"tool_calls":[],"needs_clarification":false,"clarification_question":"",
		"memory_candidates":[],"response_style":""}`

	if _, err := ParsePlan(bare); err == nil {
		t.Fatal("缺少 plan 包装的对象必须被拒绝")
	}

	// 夹杂寒暄时仍应能提取出 JSON 主体。
	withChatter := "好的，这是计划：\n" + `{"plan":{"mode":"chat","understanding":{"goal":"x","entities":[],"time_range":"","confidence":0.5},
		"tool_calls":[],"needs_clarification":false,"clarification_question":"",
		"memory_candidates":[],"response_style":""}}`
	if _, err := ParsePlan(withChatter); err != nil {
		t.Fatalf("夹杂说明文字时仍应解析成功: %v", err)
	}
}

// TestParsePlanTruncatesLongClarification 覆盖澄清问题长度上限。
func TestParsePlanTruncatesLongClarification(t *testing.T) {
	long := strings.Repeat("问", 400)
	content := `{"plan":{"mode":"clarify","understanding":{"goal":"x","entities":[],"time_range":"","confidence":0.3},
		"tool_calls":[],"needs_clarification":true,"clarification_question":"` + long + `",
		"memory_candidates":[],"response_style":""}}`

	plan, err := ParsePlan(content)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if n := len([]rune(plan.ClarificationQuestion)); n > 300 {
		t.Fatalf("澄清问题应被截断到 300 字以内，实际 %d", n)
	}
}
