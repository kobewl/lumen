package assistant

import (
	"fmt"
	"strings"

	"lumen/server/internal/temporal"
)

// 本文件是 TemporalGuard：修复"中午说早呀"的代码级守门。
//
// 它刻意**窄**：只识别回答里与可信时段明显冲突的问候语，不做任何别的判断。
// 语义（这句话想表达什么）永远是模型的事；代码只认"时段冲突"这一种硬冲突，
// 因为它有一个代码之外的权威答案（temporal.Context）可对照。
//
// 它**不是**闲聊关键词路由：
//   - 词表只用于"发现冲突"，不用于决定回答内容；
//   - 冲突后的处理是把纠正事实交回模型重写，而不是套模板。

// greetingDayParts 是封闭的问候语表：问候 → 允许的时段（白名单）。
//
// 只收多字问候短语，不收单字"早"——"早"会误伤"早点休息"这类正常句子。
// 晚安是告别语、语境依赖强（用户先说晚安再回晚安是正常的），刻意不收录。
// 长短语在前：保证"早上好呀"整体匹配，而不是先命中它的子串"早上好"。
var greetingDayParts = []struct {
	phrase  string
	allowed map[string]bool
}{
	{"早上好呀", dayPartSet("早上", "上午")},
	{"早上好啊", dayPartSet("早上", "上午")},
	{"早上好", dayPartSet("早上", "上午")},
	{"早安", dayPartSet("早上", "上午")},
	{"早呀", dayPartSet("早上", "上午")},
	{"早啊", dayPartSet("早上", "上午")},
	{"上午好", dayPartSet("上午")},
	{"中午好", dayPartSet("中午")},
	{"下午好", dayPartSet("下午")},
	{"晚上好", dayPartSet("晚上", "深夜")},
}

func dayPartSet(parts ...string) map[string]bool {
	set := make(map[string]bool, len(parts))
	for _, p := range parts {
		set[p] = true
	}
	return set
}

// detectGreetingConflict 检查回答里的问候是否与可信时段明显冲突。
//
// 返回空串表示没有冲突（或没有问候）；非空是给模型的纠正事实
// （由 trusted_context 的纠正要求段带给模型，重写一次）。
func detectGreetingConflict(answer string, tc temporal.Context) string {
	if !tc.Valid() || tc.DayPart == "" {
		// 没有可信时间就无法判断冲突——放行而不是误报。
		// 可信时间的存在性由执行器对工具保证；这里的放行只影响提示词，
		// 不影响数据访问。
		return ""
	}
	for _, g := range greetingDayParts {
		if strings.Contains(answer, g.phrase) && !g.allowed[tc.DayPart] {
			return fmt.Sprintf(
				"你上一版回答里的问候「%s」与可信时段明显冲突：现在是 %s（%s，%s）。"+
					"请重写整段回答：问候必须与可信时段一致，或者不用问候直接进入正文。",
				g.phrase, tc.ClockText(), tc.Weekday, tc.DayPart)
		}
	}
	return ""
}

// neutralizeGreeting 把文本降级为"中性且无时段"：剔除全部问候语素，
// 清理残句首尾。返回空串表示剔除后没有可用内容（调用方换确定性兜底）。
//
// 这是第二次重写失败后的降级路径，不是常规处理：正常情况下重写一次就够。
func neutralizeGreeting(answer string) string {
	text := answer
	for _, g := range greetingDayParts {
		text = strings.ReplaceAll(text, g.phrase, "…")
	}
	// 剔除处用省略号占位再修剪，避免"你好呀"这类残句直接拼在一起。
	text = strings.TrimSpace(text)
	text = strings.Trim(text, "…")
	// 去掉残句开头悬着的标点与空白。
	text = strings.TrimLeft(text, "！？。，,!. \n　")
	text = strings.TrimSpace(text)
	if len([]rune(text)) < 2 {
		return ""
	}
	return text
}
