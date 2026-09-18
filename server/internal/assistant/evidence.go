package assistant

import (
	"fmt"
	"strings"
	"time"
)

// 本文件是"依据"的唯一处理点：回答引用了哪些记录、候选记忆标了哪些来源、
// 证据行怎么写。集中在一处的原因很直接——这些字符串是从代码流向
// 用户与数据库的**事实声明**，它们一旦不准确，用户就失去了核对的可能。

// collectSessionIDs 从能力结果里提取真实存在的时段 ID，用于审计。
//
// 只在确实返回过时段的能力上提取，避免把模型编造的 ID 记进来。
// 去重：同一次检索可能被两个能力覆盖（get_today_status 与 get_sessions），
// 不去重会把 1 段记录记成 2 段。
func collectSessionIDs(results []CapabilityResult) []string {
	var ids []string
	seen := map[string]bool{}
	appendID := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}

	for _, r := range results {
		switch v := r.Value.(type) {
		case SessionsResult:
			for _, s := range v.Sessions {
				appendID(s.ID)
			}
		case TodayStatusResult:
			for _, s := range v.Sessions {
				appendID(s.ID)
			}
		}
	}
	return ids
}

// collectTaskIDs 从能力结果里提取任务摘要的可追溯 ID。
//
// 用 task_id 而不是数据库主键：task_id 是来源 Agent 的稳定标识，
// 拿它可以直接回到 ZCode 那边核对"这条结论到底是不是它报的"。
func collectTaskIDs(results []CapabilityResult) []string {
	var ids []string
	seen := map[string]bool{}
	for _, r := range results {
		v, ok := r.Value.(TaskSummariesResult)
		if !ok {
			continue
		}
		for _, t := range v.TaskSummaries {
			if t.TaskID == "" || seen[t.TaskID] {
				continue
			}
			seen[t.TaskID] = true
			ids = append(ids, t.TaskID)
		}
	}
	return ids
}

// evidenceIDSet 汇总本轮能力真实返回过的全部 ID。
//
// 这是候选记忆来源的**唯一合法取值集合**：模型看不到数据库，
// 它写出的任何 ID 都只是字符串，只有能在本轮结果里找到的才算出处。
func evidenceIDSet(results []CapabilityResult) map[string]bool {
	set := map[string]bool{}
	for _, id := range collectSessionIDs(results) {
		set[id] = true
	}
	for _, id := range collectTaskIDs(results) {
		set[id] = true
	}
	return set
}

// filterSourceIDs 只保留能在本轮能力返回里核实的来源 ID。
//
// 返回 dropped 是被剔除的 ID 数，ok=false 表示"声称了来源但一个都核实不上"。
// 区分这两种情况是有意义的：
//   - 部分核实不上 → 剔除假 ID，保留真实出处（记忆的出处仍然准确）；
//   - 全部核实不上 → 整条候选丢弃：它的出处是编的，留一条"看起来有依据"
//     的记忆比不留更危险，将来用户核对时会发现出处根本不存在。
//
// 没有声称来源时 ok=true：用户自己说的偏好（"以后先看结论"）
// 本来就不依赖任何记录，不能因为"没出处"就拒绝。
func filterSourceIDs(claimed []string, verified map[string]bool) (kept []string, dropped int, ok bool) {
	if len(claimed) == 0 {
		return nil, 0, true
	}
	seen := map[string]bool{}
	for _, id := range claimed {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !verified[id] || seen[id] {
			dropped++
			continue
		}
		seen[id] = true
		kept = append(kept, id)
	}
	if len(kept) == 0 {
		return nil, dropped, false
	}
	return kept, dropped, true
}

// dedupeViews 按真实 ID 去重，保持首次出现的顺序。
//
// ID 为空的视图（历史数据或人工构造）无法判定是否重复，原样保留：
// 宁可多算一段，也不要把两条不同的记录当成一条。
func dedupeViews(views []SessionView) []SessionView {
	out := make([]SessionView, 0, len(views))
	seen := map[string]bool{}
	for _, v := range views {
		if v.ID != "" {
			if seen[v.ID] {
				continue
			}
			seen[v.ID] = true
		}
		out = append(out, v)
	}
	return out
}

// sessionSpan 解析一个时段的起止时刻。
//
// End 只有"时:分"，要补上日期才能比较：跨夜的时段结束时间属于次日，
// 否则 23:30 ~ 00:40 会被算成"结束早于开始"，区间计算就错了。
func sessionSpan(v SessionView) (time.Time, time.Time, bool) {
	// 统一按 UTC 解析：所有视图都由同一个时区格式化而来，
	// 这里只需要相对先后关系，时区不参与判断。
	start, err := time.ParseInLocation("2006-01-02 15:04", v.Start, time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	clock, err := time.Parse("15:04", v.End)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	end := time.Date(start.Year(), start.Month(), start.Day(),
		clock.Hour(), clock.Minute(), 0, 0, time.UTC)
	if end.Before(start) {
		end = end.AddDate(0, 0, 1)
	}
	return start, end, true
}

// spanRange 计算一组时段的"最早开始 ~ 最晚结束"。
//
// 不能像早期实现那样取首尾元素：多个能力的返回各自成序，拼在一起整体并不
// 有序（按项目查与按日期查的顺序不同、两个能力的返回先后也不确定），
// 取首尾会写出一个根本不存在的区间，用户拿它去核对就会发现对不上。
func spanRange(views []SessionView) string {
	var minStart, maxEnd time.Time
	found := false
	for _, v := range views {
		start, end, ok := sessionSpan(v)
		if !ok {
			continue
		}
		if !found || start.Before(minStart) {
			minStart = start
		}
		if !found || end.After(maxEnd) {
			maxEnd = end
		}
		found = true
	}
	if !found {
		return ""
	}

	endText := maxEnd.Format("15:04")
	// 跨天时只写"时:分"会让区间看起来是倒着的（23:30 ~ 00:40），
	// 这时把日期补上。
	if maxEnd.Year() != minStart.Year() || maxEnd.YearDay() != minStart.YearDay() {
		endText = maxEnd.Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("%s ~ %s", minStart.Format("2006-01-02 15:04"), endText)
}

// evidenceLine 汇总本轮依据，附在回答末尾。
//
// 刻意标注数据来源类型：用户看到的每一个结论都应该能自己判断可信度，
// 而不是只能相信模型的措辞。"Agent 报告"与"活动记录"在证据行里分开写，
// 因此即使模型把推断说成了结论，用户仍能看出依据是什么。
func evidenceLine(results []CapabilityResult) string {
	var views []SessionView
	taskCount := 0
	for _, r := range results {
		switch v := r.Value.(type) {
		case SessionsResult:
			views = append(views, v.Sessions...)
		case TodayStatusResult:
			views = append(views, v.Sessions...)
		case TaskSummariesResult:
			taskCount += len(v.TaskSummaries)
		}
	}
	views = dedupeViews(views)

	var parts []string
	if taskCount > 0 {
		parts = append(parts, fmt.Sprintf("%d 条 Agent 报告", taskCount))
	}
	if len(views) > 0 {
		if span := spanRange(views); span != "" {
			parts = append(parts, fmt.Sprintf("%d 段活动记录（%s）", len(views), span))
		} else {
			// 时间解析不出来时宁可不写区间，也不写一个可能错的区间。
			parts = append(parts, fmt.Sprintf("%d 段活动记录", len(views)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "证据：" + strings.Join(parts, "；")
}
