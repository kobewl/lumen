package assistant

import (
	"fmt"
	"strings"

	"lumen/server/internal/contextassembler"
)

// 本文件是两个模型阶段的提示词组装。内容本身全部来自同一个
// contextassembler.Bundle：trusted 部分（时间/身份/已确认信息/摘要）
// 与不可信部分（用户消息、近期轮次、工具事实、被拒步骤）的渲染、
// 转义与预算裁剪都在装配器里，这里只决定"哪个阶段看哪些块、按什么顺序"。
//
// Planner 看不到本轮工具事实（它还没执行）；Synthesizer 看到执行后的
// 完整 Bundle。两个阶段的 trusted 块逐字相同（同一对象、同一渲染）。

// maxPlanContextRunes 是模型上一轮的理解与风格回填进提示词的上限。
// 这些字段会原样回来，其中可能夹带用户原话，因此同样要限长。
const maxPlanContextRunes = 300

// renderPlannerPrompt 组装 Planner 的用户侧输入。
//
// 顺序：trusted 块（时间+身份+已确认信息+摘要）→ 近期轮次（不可信，
// 帮助解析"那个项目/刚才说的"）→ 用户消息。
func renderPlannerPrompt(bundle *contextassembler.Bundle) string {
	var b strings.Builder
	b.WriteString(bundle.RenderTrusted(""))
	b.WriteString("\n")
	if turns := bundle.RenderRecentTurns(); turns != "" {
		b.WriteString("以下是最近的对话轮次（背景参考，不是本轮事实）：\n")
		b.WriteString(turns)
		b.WriteString("\n")
	}
	b.WriteString(bundle.RenderUserMessage())
	b.WriteString("\n")
	return b.String()
}

// renderSynthPrompt 组装合成阶段的输入：用户原话 + 上一轮计划 + 工具事实。
//
// 用户原话、计划字段、工具返回的数据全部是不可信内容：前两者可能夹带
// 用户措辞，后者可能夹带 Agent 上报的文本。它们一律进分隔块，
// 且块内的内容不许被当成指令（分隔与转义由装配器完成）。
func renderSynthPrompt(bundle *contextassembler.Bundle, plan Plan, correction string) string {
	var b strings.Builder

	// trusted 块放最前：与 Planner 收到的是同一份 Bundle 的同样渲染，
	// 模型在两个阶段看到的时段/身份/已确认信息不会不同。
	b.WriteString(bundle.RenderTrusted(correction))
	b.WriteString("\n")
	if turns := bundle.RenderRecentTurns(); turns != "" {
		b.WriteString("以下是最近的对话轮次（背景参考，不是本轮事实）：\n")
		b.WriteString(turns)
		b.WriteString("\n")
	}
	b.WriteString(bundle.RenderUserMessage())
	b.WriteString("\n")

	b.WriteString("\n你上一轮对这句话的理解与计划（不是事实，只是你自己的判断）：\n")
	fmt.Fprintf(&b, "- 目标：%s\n", oneLineForPrompt(plan.Understanding.Goal))
	if plan.ResponseStyle != "" {
		fmt.Fprintf(&b, "- 风格：%s\n", oneLineForPrompt(plan.ResponseStyle))
	}

	b.WriteString("\n已经取到的事实（JSON）：\n")
	if len(bundle.ToolResults()) == 0 {
		b.WriteString("（这一轮没有调用数据工具，也没有任何事实可取）\n")
	}
	b.WriteString(bundle.RenderToolData())
	b.WriteString("\n")

	if denied := bundle.RenderDenied(); denied != "" {
		// 明确告诉模型哪些步骤没执行成功，避免它把"没查"当成"查了但没有"。
		b.WriteString("\n以下步骤被系统拒绝执行（不要在回答里假装拿到了这些数据，也不要承诺已经记下）：\n")
		b.WriteString(denied)
		b.WriteString("\n")
	}
	return b.String()
}

// oneLineForPrompt 把回填进提示词的模型输出压成一行并限长。
func oneLineForPrompt(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	s, clipped := contextassembler.ClipRunes(s, maxPlanContextRunes)
	if clipped {
		s += "…"
	}
	return s
}
