// Package contextassembler 是"送进模型的上下文"的唯一装配点。
//
// 为什么需要它：此前每轮上下文是散着拼的——Agent 直接读 conversation_states，
// 提示词各阶段各自渲染，候选记忆与已确认信息之间没有生命周期边界。本包把
// 每轮上下文收敛成一个受预算、可解释的 ContextBundle：
//
//   - trusted_context：只由代码生成（可信时间、助手身份、已确认用户信息、
//     会话摘要），用户文本与工具数据无法覆盖或提前闭合它；
//   - untrusted_evidence：近期有限轮次与工具事实，带来源与限额，全部转义；
//   - Budget：截断顺序固定（Profile > 摘要 > 近期轮次 > 证据细节），
//     一切丢弃都记录在截断元信息里，绝不静默随机截断。
//
// 依赖规则：assistant.Agent 不允许为拼上下文直接读 storage，只消费这里的
// Bundle；本包只依赖 temporal / identity / storage / tooling。
package contextassembler

import "strings"

// 分隔块标签。集中定义，避免提示词说明与代码里的标签名不一致——
// 一旦不一致，模型看到的规则就与实际的块对不上了。
const (
	TagUserMessage    = "user_message"
	TagPlanContext    = "plan_context"
	TagToolData       = "tool_data"
	TagDeniedSteps    = "denied_steps"
	TagRecentTurns    = "recent_turns"
	TagTrustedContext = "trusted_context"
)

// 各类内容的默认长度上限（runes）。
const (
	// MaxUserTextRunes 是单条用户消息进入提示词的长度上限。
	// 2000 字足以容纳正常提问，也远小于任何模型的上下文窗口。
	MaxUserTextRunes = 2000

	// TruncationNotice 在内容被截断处显式标出。
	//
	// 不静默截断：模型如果不知道内容少了半截，会把残缺的 JSON 或半句话
	// 当成完整事实来用，这比直接告诉它"这里被截断了"危险得多。
	TruncationNotice = "\n…（内容过长，已截断）"
)

// ClipRunes 按字符数截断，并报告是否发生了截断。
func ClipRunes(s string, limit int) (string, bool) {
	r := []rune(s)
	if limit <= 0 || len(r) <= limit {
		return s, false
	}
	return string(r[:limit]), true
}

// XMLEscape 转义不可信内容里的 & < >，使其无法提前闭合分隔标签。
func XMLEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// UntrustedBlock 渲染一个不可信内容块：转义后放入指定的 XML 标签。
//
// attr 会原样拼进开标签（如 name="get_sessions"），但**不会**出现在闭标签里：
// 闭合标签必须与标签名完全相同，带属性就不是合法 XML，模型也无法可靠配对。
func UntrustedBlock(tag, attr, body string) string {
	open := "<" + tag
	if attr != "" {
		open += " " + attr
	}
	return open + ">\n" + XMLEscape(body) + "\n</" + tag + ">"
}

// SafeTagValue 清洗要写进 XML 属性的值。
//
// 属性值同样在标签内，因此除了转义还要去掉引号与空白，
// 使它不可能闭合属性或注入新属性。
func SafeTagValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '"' || r == '\'' || r == '<' || r == '>' || r == '&' || r == '=':
			b.WriteRune('_')
		case r < 0x20:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len([]rune(out)) > 64 {
		out = string([]rune(out)[:64])
	}
	return out
}

// CollapseLine 把多行文本压成一行（已确认信息/摘要值在写入侧限长，
// 但仍可能带换行——渲染进 trusted_context 时必须压平，避免撑破块结构）。
func CollapseLine(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}
