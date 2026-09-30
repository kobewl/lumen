package assistant

import (
	"fmt"
	"strings"
	"time"

	"lumen/server/internal/tooling"
)

// 本文件是"依据"的唯一处理点：回答引用了哪些记录、证据行怎么写。
//
// 它现在只依赖 tooling.Result 这一个形状——工具细节（时段、任务摘要）
// 已经在各自的结果里声明过 Kind / Evidence / Count / Digest，
// 因此这里不需要 switch 每个具体工具，加新工具也不改这里。

// collectEvidence 汇总本轮全部工具返回的可核实记录 ID（去重、保持顺序）。
func collectEvidence(results []tooling.Result) []string {
	var ids []string
	seen := map[string]bool{}
	for _, r := range results {
		for _, id := range r.Evidence {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// hasReportedFacts 判断本轮是否取到了"Agent 报告的结论"。
//
// 这是区分 supported 与 inferred 的唯一依据：只有任务摘要这类结果是结论本身，
// 应用名与时长、git 提交信息都只是活动痕迹，从它们推出的"完成了什么"始终是推断。
func hasReportedFacts(results []tooling.Result) bool {
	for _, r := range results {
		if r.Kind == tooling.KindReportedTasks && r.Count > 0 {
			return true
		}
	}
	return false
}

// evidenceLine 汇总本轮依据，附在回答末尾。
//
// 刻意标注数据来源类型：用户看到的每一个结论都应该能自己判断可信度，
// 而不是只能相信模型的措辞。"Agent 报告"与"活动记录"在证据行里分开写，
// 因此即使模型把推断说成了结论，用户仍能看出依据是什么。
func evidenceLine(results []tooling.Result, loc *time.Location) string {
	reported := 0
	activity := 0
	var spans []tooling.Span
	for _, r := range results {
		switch r.Kind {
		case tooling.KindReportedTasks:
			reported += r.Count
		case tooling.KindActivity:
			activity += r.Count
			if r.Span.Valid() {
				spans = append(spans, r.Span)
			}
		}
	}

	var parts []string
	if reported > 0 {
		parts = append(parts, fmt.Sprintf("%d 条 Agent 报告", reported))
	}
	if activity > 0 {
		if span := spanRange(spans, loc); span != "" {
			parts = append(parts, fmt.Sprintf("%d 段活动记录（%s）", activity, span))
		} else {
			// 时间解析不出来时宁可不写区间，也不写一个可能错的区间。
			parts = append(parts, fmt.Sprintf("%d 段活动记录", activity))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "证据：" + strings.Join(parts, "；")
}

// spanRange 把区间渲染成"最早开始 ~ 最晚结束"。
//
// 区间在工具里已经算好（tooling.MergeSpans）：多个查询的返回各自成序，
// 取首尾元素会写出一个根本不存在的区间。
func spanRange(spans []tooling.Span, loc *time.Location) string {
	merged := tooling.MergeSpans(spans...)
	if !merged.Valid() {
		return ""
	}
	if loc == nil {
		return ""
	}
	start := merged.Start.In(loc)
	end := merged.End.In(loc)
	endText := end.Format("15:04")
	// 跨天时只写"时:分"会让区间看起来是倒着的（23:30 ~ 00:40），
	// 这时把日期补上。
	if end.Year() != start.Year() || end.YearDay() != start.YearDay() {
		endText = end.Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("%s ~ %s", start.Format("2006-01-02 15:04"), endText)
}

// renderFacts 把工具返回的确定性事实渲染成一段克制的陈述。
//
// 这是合成失败时的降级输出：只陈述已有的事实，不做任何推测。
// 事实行由工具自己提供（Result.Digest），因此新增工具不需要改这里；
// 两类事实的措辞差别（"Agent 报告" vs 活动记录）也在工具里定死。
func renderFacts(results []tooling.Result) string {
	var lines []string
	for _, r := range results {
		for _, line := range r.Digest {
			if text := strings.TrimRight(line, "\n"); text != "" {
				lines = append(lines, text)
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}
