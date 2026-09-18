package assistant

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"lumen/server/internal/ai"
)

// answerOverclaimPhrases 是回答里不允许出现的越界承诺。
//
// V0.1 的能力边界是硬的：看不到窗口标题与内容，也不能代劳开放域任务。
// 模型一旦承诺这些，就是编造能力，直接降级。
var answerOverclaimPhrases = []string{
	"无所不能", "什么都能", "任何问题都能", "帮你写周报", "帮您写周报",
	"帮你写代码", "帮您写代码", "陪你聊天到天亮", "我什么都知道",
}

// answerForbiddenTerms 是绝不能出现在用户可见文本里的内部术语。
var answerForbiddenTerms = []string{
	"unclassified", "Session", "session", "```", "s_",
}

// validAnswerText 校验模型生成的最终回答是否在边界内。
//
// 只做可枚举的硬校验（非空、长度、越界措辞、内部术语），不追求语义完美：
// 语气与结构交给模型自由发挥，代码只守住"不编造能力、不泄露内部概念"这两条。
func validAnswerText(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	if len([]rune(trimmed)) > 600 {
		return false
	}
	for _, bad := range answerOverclaimPhrases {
		if strings.Contains(trimmed, bad) {
			return false
		}
	}
	for _, bad := range answerForbiddenTerms {
		if strings.Contains(trimmed, bad) {
			return false
		}
	}
	return true
}

// validMemoryContent 校验候选记忆内容是否可以落库。
//
// 拒绝短到没有信息量的内容，以及含内部术语或疑似凭证的内容——
// 记忆会被长期保存，写错东西的代价比回复说错一句话高得多。
func validMemoryContent(content string) bool {
	trimmed := strings.TrimSpace(content)
	if len([]rune(trimmed)) < 2 {
		return false
	}
	for _, bad := range answerForbiddenTerms {
		if strings.Contains(trimmed, bad) {
			return false
		}
	}
	lower := strings.ToLower(trimmed)
	for _, bad := range []string{"sk-", "api key", "api_key", "token", "password", "密码", "密钥"} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	return true
}

// parseSynthResult 解析合成阶段的输出。
func parseSynthResult(content string) (SynthResult, error) {
	raw, err := ai.ExtractJSON(content)
	if err != nil {
		return SynthResult{}, fmt.Errorf("输出不是合法 JSON: %w", err)
	}
	var parsed struct {
		Answer       string `json:"answer"`
		SupportLevel string `json:"support_level"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parsed); err != nil {
		return SynthResult{}, fmt.Errorf("结构不符: %w", err)
	}
	parsed.Answer = strings.TrimSpace(parsed.Answer)
	if parsed.Answer == "" {
		return SynthResult{}, fmt.Errorf("answer 为空")
	}
	return SynthResult{Answer: parsed.Answer, SupportLevel: parsed.SupportLevel}, nil
}

// renderFacts 把能力返回的结构化事实渲染成一段克制的陈述。
//
// 这是合成失败时的降级输出：只陈述已有的事实，不做任何推测。
//
// 两类事实分开呈现且措辞不同：任务摘要写"Agent 报告"，活动记录写"活动记录"。
// 降级路径同样不能让用户把两者混为一谈——尤其不能把"用了某应用多久"
// 说成"完成了什么"。
func renderFacts(results []CapabilityResult, _ *time.Location) string {
	var b strings.Builder
	wrote := false

	for _, r := range results {
		switch v := r.Value.(type) {
		case TaskSummariesResult:
			wrote = writeTaskFacts(&b, v) || wrote
		case SessionsResult:
			wrote = writeSessionFacts(&b, v.Sessions, v.TotalMinutes, fmt.Sprintf("（%s）", v.Query)) || wrote
		case TodayStatusResult:
			wrote = writeSessionFacts(&b, v.Sessions, v.TotalMinutes, fmt.Sprintf("（%s）", v.Date)) || wrote
		}
	}
	if !wrote {
		return ""
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeTaskFacts 渲染任务摘要事实。
//
// 明确写出"Agent 报告"而不是"你完成了"：结论来自 Agent 的自我汇报，
// 不是 Lumen 观察到的，用户有权知道这个区别。
func writeTaskFacts(b *strings.Builder, v TaskSummariesResult) bool {
	if len(v.TaskSummaries) == 0 {
		fmt.Fprintf(b, "（%s）没有查到 Agent 汇报的任务摘要。\n", v.Query)
		return true
	}

	fmt.Fprintf(b, "（%s）Agent 报告了 %d 个任务：\n", v.Query, len(v.TaskSummaries))
	for _, t := range v.TaskSummaries {
		label := taskStatusLabel(t.Status)
		fmt.Fprintf(b, "▍%s（%s）\n", t.Title, label)
		if t.Project != "" {
			fmt.Fprintf(b, "  · 项目：%s\n", t.Project)
		}
		for _, item := range t.Outcomes {
			fmt.Fprintf(b, "  · 完成：%s\n", item)
		}
		for _, item := range t.OpenLoops {
			fmt.Fprintf(b, "  · 未完成：%s\n", item)
		}
		if t.SourceAgent != "" {
			fmt.Fprintf(b, "  · 来源：%s 报告\n", t.SourceAgent)
		}
	}
	return true
}

// taskStatusLabel 把状态转成用户可读文案。
func taskStatusLabel(status string) string {
	switch status {
	case "done":
		return "已完成"
	case "partial":
		return "部分完成"
	case "blocked":
		return "受阻"
	case "abandoned":
		return "已放弃"
	default:
		return "状态未说明"
	}
}

func writeSessionFacts(b *strings.Builder, sessions []SessionView, total float64, label string) bool {
	if len(sessions) == 0 {
		fmt.Fprintf(b, "%s我这边没有查到记录。\n", label)
		return true
	}
	fmt.Fprintf(b, "%s共 %d 段记录，合计约 %.0f 分钟：\n", label, len(sessions), total)
	for _, s := range sessions {
		fmt.Fprintf(b, "▍%s · 约 %.0f 分钟（%s ~ %s）\n",
			s.Project, s.DurationMins, shortTime(s.Start), s.End)
		for _, app := range s.Apps {
			fmt.Fprintf(b, "  · %s\n", app)
		}
		for _, msg := range s.GitMessages {
			fmt.Fprintf(b, "  · 提交：%s\n", msg)
		}
		if len(s.GitMessages) == 0 {
			b.WriteString("  · 只有应用与时长记录，看不出具体做了什么\n")
		}
	}
	return true
}

// shortTime 把 "2006-01-02 15:04" 压成 "01-02 15:04"，让事实摘要更紧凑。
func shortTime(s string) string {
	if len(s) >= 16 {
		return s[5:]
	}
	return s
}
