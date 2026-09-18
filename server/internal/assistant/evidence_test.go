package assistant

import (
	"context"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/storage"
)

// 本文件覆盖"依据"的两条硬约束：
//   A. 候选记忆的来源只能引用本轮能力真实返回过的 ID；
//   B. 证据区间必须在无序输入上算对，且不重复计数。
//
// 这两条都是"用户拿去核对时会不会发现对不上"的问题，
// 因此断言必须精确到具体数值与具体 ID，不能用"包含某段文字"糊过去。

// ---- A. 候选记忆来源必须可核实 ----

// TestMemoryCandidateSourceIDsMustComeFromThisTurn 覆盖来源核实的三种结果。
//
// 模型看不到数据库，它写出的 source_ids 只是字符串：
//   - 能在本轮结果里找到 → 保留；
//   - 找不到但还有别的真实来源 → 剔除假的；
//   - 一个都找不到 → 整条丢弃（不是"留着但来源为空"）。
func TestMemoryCandidateSourceIDsMustComeFromThisTurn(t *testing.T) {
	// 用今天：get_today_status 只查当天，种在别的日期上会导致"本轮没取到"。
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "用户提到了记录", TimeRange: "today", Confidence: 0.9,
			},
			ToolCalls: []ToolCall{{Name: "get_today_status"}},
			MemoryCandidates: []MemoryCandidate{
				{
					Kind: "fact", Content: "看着 ZCode 的时段记录在推进项目", Confidence: 0.8,
					SourceIDs: []string{"s_test_session_0001"},
				},
				{
					Kind: "fact", Content: "这一段其实是编造的出处", Confidence: 0.8,
					SourceIDs: []string{"s_model_invented_9999"},
				},
				{
					Kind: "fact", Content: "真假来源混在一起", Confidence: 0.8,
					SourceIDs: []string{"s_test_session_0001", "s_model_invented_8888"},
				},
				{
					Kind: "preference", Content: "用户偏好先看结论再看细节", Confidence: 0.7,
				},
			},
		},
		synth: SynthResult{Answer: "今天用了 ZCode 约 90 分钟。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, DefaultProfile(), planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "看看今天的记录"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	// 4 条里只有 3 条能落库：整条来源都编造的那条必须消失。
	if reply.MemoryCandidates != 3 {
		t.Fatalf("应写入 3 条候选（1 条因来源不可核实被丢弃），实际 %d", reply.MemoryCandidates)
	}

	candidates := loadCandidates(t, store)
	byContent := map[string][]string{}
	for _, c := range candidates {
		byContent[c.Content] = c.SourceIDs
	}

	if got := byContent["看着 ZCode 的时段记录在推进项目"]; len(got) != 1 || got[0] != "s_test_session_0001" {
		t.Fatalf("真实来源应原样保留，实际 %v", got)
	}
	if _, exists := byContent["这一段其实是编造的出处"]; exists {
		t.Fatal("来源完全无法核实的候选记忆不应落库")
	}
	mixed := byContent["真假来源混在一起"]
	if len(mixed) != 1 || mixed[0] != "s_test_session_0001" {
		t.Fatalf("混合来源应只保留可核实的那一个，实际 %v", mixed)
	}
	if got, exists := byContent["用户偏好先看结论再看细节"]; !exists {
		t.Fatal("用户自己说的偏好没有来源，应当照常落库")
	} else if len(got) != 0 {
		t.Fatalf("无来源的候选不应被补上来源，实际 %v", got)
	}

	// 被丢弃/剔除的 ID 绝不能出现在库里任何地方。
	for _, c := range candidates {
		for _, id := range c.SourceIDs {
			if strings.Contains(id, "invented") {
				t.Fatalf("不可核实的来源 ID 不应落库: %+v", c)
			}
		}
	}
}

// TestMemoryCandidateSourceIDsRejectForeignCapabilityIDs 覆盖"来源必须是本轮结果"。
//
// 关键区分：这两个 ID 都**真的在库里存在**，但本轮没查它们。
// 依据必须是"本轮实际取到的"，不是"库里存在的"——否则模型可以给任意结论
// 挂上一个真实但无关的出处，用户核对时会被误导。
func TestMemoryCandidateSourceIDsRejectForeignCapabilityIDs(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "查今天的记录", TimeRange: "today", Confidence: 0.9,
			},
			ToolCalls: []ToolCall{{Name: "get_today_status"}},
			MemoryCandidates: []MemoryCandidate{{
				Kind: "fact", Content: "引用了一个库里真实存在但本轮没查的时段", Confidence: 0.8,
				SourceIDs: []string{"s_other_turn_session", "s_test_session_0001"},
			}},
		},
		synth: SynthResult{Answer: "今天用了 ZCode 约 90 分钟。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, DefaultProfile(), planner)

	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "今天做了什么"}); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	candidates := loadCandidates(t, store)
	if len(candidates) != 1 {
		t.Fatalf("应有 1 条候选，实际 %d", len(candidates))
	}
	if len(candidates[0].SourceIDs) != 1 || candidates[0].SourceIDs[0] != "s_test_session_0001" {
		t.Fatalf("只应保留本轮取到的来源，实际 %v", candidates[0].SourceIDs)
	}
}

// TestMemoryCandidateSourceIDsAcceptTaskSummaryIDs 覆盖任务摘要同样是合法来源。
//
// 任务摘要的 task_id 与时段 ID 是两类不同的标识，但都属于"本轮取到的事实"。
func TestMemoryCandidateSourceIDsAcceptTaskSummaryIDs(t *testing.T) {
	today := time.Now().In(testLoc)
	store := newTestStore(t, today.Format("2006-01-02"), "lumen")
	seedTaskSummary(t, store, "zcode-t-1", "接通任务摘要链路", "done", nil, nil, today)

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "用户想知道完成了什么", TimeRange: "today", Confidence: 0.9,
			},
			ToolCalls: []ToolCall{{Name: "get_task_summaries",
				Arguments: map[string]any{"date": today.Format("2006-01-02")}}},
			MemoryCandidates: []MemoryCandidate{{
				Kind: "project", Content: "任务摘要链路已经接通", Confidence: 0.8,
				SourceIDs: []string{"zcode-t-1"},
			}},
		},
		synth: SynthResult{Answer: "ZCode 那边报告完成了一件。", SupportLevel: SupportSupported},
	}
	agent := newTaskAgent(t, store, planner)

	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "今天完成了什么"}); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	candidates := loadCandidates(t, store)
	if len(candidates) != 1 {
		t.Fatalf("应有 1 条候选，实际 %d", len(candidates))
	}
	if len(candidates[0].SourceIDs) != 1 || candidates[0].SourceIDs[0] != "zcode-t-1" {
		t.Fatalf("Agent 报告的任务 ID 应可作为来源，实际 %v", candidates[0].SourceIDs)
	}
}

// TestFilterSourceIDsEdgeCases 覆盖去重与空值处理。
func TestFilterSourceIDsEdgeCases(t *testing.T) {
	verified := map[string]bool{"s_1": true, "s_2": true}

	kept, dropped, ok := filterSourceIDs([]string{"s_1", "s_1", "  ", "s_2"}, verified)
	if !ok || len(kept) != 2 || dropped != 1 {
		t.Fatalf("应保留 2 个去重后的 ID 并记 1 个被剔除，实际 kept=%v dropped=%d ok=%v",
			kept, dropped, ok)
	}
	if kept[0] != "s_1" || kept[1] != "s_2" {
		t.Fatalf("应保持首次出现的顺序，实际 %v", kept)
	}

	if _, _, ok := filterSourceIDs([]string{"s_ghost"}, verified); ok {
		t.Fatal("来源全部无法核实时应判定为不可用")
	}
	if kept, dropped, ok := filterSourceIDs(nil, verified); !ok || kept != nil || dropped != 0 {
		t.Fatalf("没有声称来源时应放行，实际 kept=%v dropped=%d ok=%v", kept, dropped, ok)
	}
}

// loadCandidates 读出测试用户的全部候选记忆。
func loadCandidates(t *testing.T, store *storage.Store) []storage.MemoryCandidate {
	t.Helper()
	rows, err := store.MemoryCandidatesByUser(context.Background(), "u1", 50)
	if err != nil {
		t.Fatalf("读取候选记忆失败: %v", err)
	}
	return rows
}

// ---- B. 证据区间必须在无序输入上算对 ----

// TestEvidenceRangeUsesMinStartAndMaxEnd 覆盖无序结果的区间计算。
//
// 早期实现取 views[0].Start ~ views[last].End：多个能力的返回各自成序，
// 拼在一起整体并不有序，于是会写出一个根本不存在的区间。
// 这里刻意把最小的开始放在中间、最大的结束放在开头。
func TestEvidenceRangeUsesMinStartAndMaxEnd(t *testing.T) {
	results := []CapabilityResult{
		{
			Name: "get_sessions",
			Value: SessionsResult{
				Query: "project=lumen",
				Sessions: []SessionView{
					{ID: "s_c", Project: "lumen", Start: "2026-09-17 14:00", End: "15:30"},
					{ID: "s_a", Project: "lumen", Start: "2026-09-17 09:00", End: "10:15"},
				},
			},
		},
		{
			Name: "get_today_status",
			Value: TodayStatusResult{
				Date: "2026-09-17",
				Sessions: []SessionView{
					{ID: "s_d", Project: "lumen", Start: "2026-09-17 19:45", End: "22:10"},
					{ID: "s_b", Project: "lumen", Start: "2026-09-17 11:00", End: "12:00"},
				},
			},
		},
	}

	line := evidenceLine(results)
	if !strings.Contains(line, "2026-09-17 09:00 ~ 22:10") {
		t.Fatalf("区间应取最早开始 ~ 最晚结束，实际: %s", line)
	}
	if strings.Contains(line, "14:00 ~ 12:00") {
		t.Fatalf("区间不应取首尾元素，实际: %s", line)
	}
}

// TestEvidenceDeduplicatesSessionsAcrossCapabilities 覆盖跨能力去重。
//
// get_today_status 与 get_sessions 可能返回同一段记录：
// 不去重会把 1 段记成 2 段，用户核对时数量对不上。
func TestEvidenceDeduplicatesSessionsAcrossCapabilities(t *testing.T) {
	shared := SessionView{ID: "s_dup", Project: "lumen",
		Start: "2026-09-17 09:00", End: "10:30"}

	results := []CapabilityResult{
		{Name: "get_today_status", Value: TodayStatusResult{
			Date: "2026-09-17", Sessions: []SessionView{shared},
		}},
		{Name: "get_sessions", Value: SessionsResult{
			Query: "date=2026-09-17", Sessions: []SessionView{shared},
		}},
	}

	line := evidenceLine(results)
	if !strings.Contains(line, "1 段活动记录") {
		t.Fatalf("同一段记录重复返回时应只计 1 段，实际: %s", line)
	}
	if ids := collectSessionIDs(results); len(ids) != 1 || ids[0] != "s_dup" {
		t.Fatalf("来源 ID 应去重，实际 %v", ids)
	}
}

// TestEvidenceRangeHandlesOvernightSpan 覆盖跨夜时段的区间。
//
// End 只有"时:分"，不补日期就会算成"结束早于开始"，
// 从而把最晚结束判错。
func TestEvidenceRangeHandlesOvernightSpan(t *testing.T) {
	results := []CapabilityResult{{
		Name: "get_sessions",
		Value: SessionsResult{
			Query: "date=2026-09-17",
			Sessions: []SessionView{
				{ID: "s_day", Project: "lumen", Start: "2026-09-17 09:00", End: "12:00"},
				{ID: "s_night", Project: "lumen", Start: "2026-09-17 23:30", End: "00:40"},
			},
		},
	}}

	line := evidenceLine(results)
	// 最晚结束是次日 00:40，必须带上日期才看得出是跨天。
	if !strings.Contains(line, "2026-09-18 00:40") {
		t.Fatalf("跨夜时段的最晚结束应带上日期，实际: %s", line)
	}
	if !strings.Contains(line, "2026-09-17 09:00") {
		t.Fatalf("最早开始应为 09:00，实际: %s", line)
	}
}

// TestEvidenceRangeSkipsUnparsableTimes 覆盖脏数据下的降级。
//
// 时间解析不出来时宁可不写区间，也不写一个可能错的区间。
func TestEvidenceRangeSkipsUnparsableTimes(t *testing.T) {
	results := []CapabilityResult{{
		Name: "get_sessions",
		Value: SessionsResult{
			Query: "date=2026-09-17",
			Sessions: []SessionView{
				{ID: "s_bad", Project: "lumen", Start: "不是时间", End: "?"},
			},
		},
	}}

	line := evidenceLine(results)
	if strings.Contains(line, "~") {
		t.Fatalf("时间不可解析时不应写区间，实际: %s", line)
	}
	if !strings.Contains(line, "1 段活动记录") {
		t.Fatalf("仍应报告段数，实际: %s", line)
	}
}

// TestEvidenceLineKeepsTaskAndActivitySeparate 覆盖两类来源分开呈现。
func TestEvidenceLineKeepsTaskAndActivitySeparate(t *testing.T) {
	results := []CapabilityResult{
		{Name: "get_task_summaries", Value: TaskSummariesResult{
			Query: "date=2026-09-17",
			TaskSummaries: []TaskSummaryView{
				{TaskID: "t-1", Title: "A", Status: "done", SourceAgent: "zcode-cli"},
				{TaskID: "t-2", Title: "B", Status: "partial", SourceAgent: "zcode-cli"},
			},
			Count: 2,
		}},
		{Name: "get_sessions", Value: SessionsResult{
			Query:    "date=2026-09-17",
			Sessions: []SessionView{{ID: "s_1", Start: "2026-09-17 09:00", End: "10:00"}},
		}},
	}

	line := evidenceLine(results)
	if !strings.Contains(line, "2 条 Agent 报告") {
		t.Fatalf("应报告 Agent 报告条数，实际: %s", line)
	}
	if !strings.Contains(line, "1 段活动记录") {
		t.Fatalf("应报告活动记录段数，实际: %s", line)
	}
	// 两类来源必须在同一行里各自成段，用户才能分辨可信度。
	if !strings.Contains(line, "；") {
		t.Fatalf("两类来源应分开呈现，实际: %s", line)
	}
}
