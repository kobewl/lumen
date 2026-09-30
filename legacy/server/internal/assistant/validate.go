package assistant

import (
	"encoding/json"
	"fmt"
	"strings"

	"lumen/server/internal/ai"
)

// answerOverclaimPhrases 是回答里不允许出现的越界承诺。
//
// 当前工具边界是硬的：看不到窗口标题与内容，也不能代劳开放域任务。
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
