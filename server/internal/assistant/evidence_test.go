package assistant

import (
	"context"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/identity"
	"lumen/server/internal/storage"
	"lumen/server/internal/tooling"
)

// 本文件覆盖"依据"的两条硬约束：
//   A. 候选记忆的来源只能来自本轮工具真实返回过的 ID；
//   B. 证据区间必须在无序输入上算对，且不重复计数。
//
// 这两条都是"用户拿去核对时会不会发现对不上"的问题，
// 因此断言必须精确到具体数值与具体 ID，不能用"包含某段文字"糊过去。

// ---- A. 候选记忆来源必须可核实 ----

// TestMemoryWriteRequiresEvidenceFromThisTurn 覆盖写工具的证据注入。
//
// 关键区别在于**谁来提供来源**：模型给不出 source_ids
// （schema 里没有这个参数，给了会被策略拒绝），
// 来源只能由执行器从本轮成功读到的记录里注入。
// 因此"来源必须是本轮真实取到的"从一条校验规则变成了结构性保证。
func TestMemoryWriteRequiresEvidenceFromThisTurn(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "用户提到记录并表达了偏好", TimeRange: "today", Confidence: 0.9,
			},
			ToolCalls: []tooling.Call{
				{Name: "get_today_status"},
				{Name: "save_memory_candidate", Arguments: map[string]any{
					"kind": "fact", "content": "看着 ZCode 的时段记录在推进项目", "confidence": 0.8,
				}},
			},
		},
		synth: SynthResult{Answer: "今天用了 ZCode 约 90 分钟。", SupportLevel: SupportSupported},
	}
	agent := newTaskAgent(t, store, planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "看看今天的记录"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if reply.MemoryCandidates != 1 {
		t.Fatalf("应写入 1 条候选记忆，实际 %d", reply.MemoryCandidates)
	}

	candidates := loadCandidates(t, store)
	if len(candidates) != 1 {
		t.Fatalf("应有 1 条候选，实际 %d", len(candidates))
	}
	// 来源必须是本轮真实读到的时段 ID。
	got := candidates[0].SourceIDs
	if len(got) != 1 || got[0] != "s_test_session_0001" {
		t.Fatalf("来源应由本轮读取结果注入，实际 %v", got)
	}
	if candidates[0].Status != "candidate" {
		t.Fatalf("状态必须是 candidate，实际 %q", candidates[0].Status)
	}
}

// TestMemoryWriteWithoutReadIsDenied 覆盖"没有读取就没有写入"。
//
// 模型可以单独请求写工具（不做任何检索），这时没有可核实的来源，
// 执行器必须在调用之前拒绝它——而不是写进去再事后补救。
func TestMemoryWriteWithoutReadIsDenied(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeChat,
			Understanding: Understanding{
				Goal: "用户说了句想记住的话", Confidence: 0.8,
			},
			ToolCalls: []tooling.Call{{Name: "save_memory_candidate", Arguments: map[string]any{
				"kind": "preference", "key": "回答风格", "content": "用户偏好先看结论再看细节", "confidence": 0.9,
			}}},
		},
		synth: SynthResult{Answer: "好，我记下了。", SupportLevel: SupportSupported},
	}
	h := newHarness(t, store, identity.Default(), planner)

	reply, err := h.agent.Handle(context.Background(), Turn{UserID: "u1", Text: "以后先说结论"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if reply.MemoryCandidates != 0 {
		t.Fatalf("没有来源证据时不应写入候选，实际 %d", reply.MemoryCandidates)
	}
	if n, _ := store.CountMemoryCandidates(context.Background()); n != 0 {
		t.Fatalf("库里不应有候选记忆，实际 %d 条", n)
	}
	if len(reply.DeniedTools) != 1 || reply.DeniedTools[0].Name != "save_memory_candidate" {
		t.Fatalf("应记录一次拒绝，实际 %+v", reply.DeniedTools)
	}
	// 被拒原因要能说清"缺什么"，否则模型会反复尝试同一个调用。
	if !strings.Contains(reply.DeniedTools[0].Reason, "来源") {
		t.Fatalf("拒绝原因应说明缺少来源证据，实际 %q", reply.DeniedTools[0].Reason)
	}
	// 拒绝也要留审计。
	records := h.audit.All()
	if len(records) != 1 || records[0].Decision != tooling.DecisionDenied {
		t.Fatalf("被拒的写入应留审计，实际 %+v", records)
	}
}

// TestMemoryWriteRejectsModelSuppliedSourceIDs 覆盖"模型不能自己指定来源"。
//
// 给写工具塞一个 source_ids 参数必须被策略拒绝整条调用（未声明参数），
// 而不是被忽略后照常写入——后者会让模型以为自己成功指定了出处。
func TestMemoryWriteRejectsModelSuppliedSourceIDs(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "用户提到记录", TimeRange: "today", Confidence: 0.9,
			},
			ToolCalls: []tooling.Call{
				{Name: "get_today_status"},
				{Name: "save_memory_candidate", Arguments: map[string]any{
					"kind": "fact", "content": "伪造来源的候选记忆内容",
					"source_ids": []any{"s_model_invented_9999"},
				}},
			},
		},
		synth: SynthResult{Answer: "今天用了 ZCode 约 90 分钟。", SupportLevel: SupportSupported},
	}
	agent := newTaskAgent(t, store, planner)

	reply, _ := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "看看今天的记录"})
	if reply.MemoryCandidates != 0 {
		t.Fatalf("带未声明参数的写入不应成功，实际写入 %d 条", reply.MemoryCandidates)
	}
	if len(reply.DeniedTools) != 1 {
		t.Fatalf("应记录一次拒绝，实际 %+v", reply.DeniedTools)
	}
	if n, _ := store.CountMemoryCandidates(context.Background()); n != 0 {
		t.Fatalf("库里不应有候选记忆，实际 %d 条", n)
	}
}

// TestMemoryWriteAcceptsTaskSummaryIDs 覆盖任务摘要同样是合法来源。
//
// 任务摘要的 task_id 与时段 ID 是两类不同的标识，但都属于"本轮取到的事实"。
func TestMemoryWriteAcceptsTaskSummaryIDs(t *testing.T) {
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
			ToolCalls: []tooling.Call{
				{Name: "get_task_summaries", Arguments: map[string]any{"date": today.Format("2006-01-02")}},
				{Name: "save_memory_candidate", Arguments: map[string]any{
					"kind": "project", "content": "任务摘要链路已经接通", "confidence": 0.8,
				}},
			},
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

// TestMemoryCandidatesStayUnconfirmed 覆盖候选记忆的三条隔离保证。
//
// liang 明确要求：候选记忆只能写候选、不得自动晋升、不得进入后续模型上下文。
func TestMemoryCandidatesStayUnconfirmed(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "用户提到记录并表达偏好", TimeRange: "today", Confidence: 0.8,
			},
			ToolCalls: []tooling.Call{
				{Name: "get_today_status"},
				{Name: "save_memory_candidate", Arguments: map[string]any{
					"kind": "preference", "key": "回答风格", "content": "用户偏好先看结论再看细节", "confidence": 0.7,
				}},
			},
		},
		synth: SynthResult{Answer: "记下了，以后先说结论。", SupportLevel: SupportSupported},
	}
	agent := newTaskAgent(t, store, planner)

	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "以后先给我说结论"}); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if n, _ := store.CountMemoryCandidates(context.Background()); n != 1 {
		t.Fatalf("应写入 1 条候选，实际 %d", n)
	}

	// 未经用户确认，绝不能进入"已确认记忆"。
	confirmed, err := store.MemoryCandidatesByStatus(context.Background(), storage.MemoryStatusConfirmed, 10)
	if err != nil {
		t.Fatalf("查询已确认记忆失败: %v", err)
	}
	if len(confirmed) != 0 {
		t.Fatalf("候选记忆不应自动晋升为已确认，实际 %d 条", len(confirmed))
	}

	// 第二轮：候选内容不得出现在任何提示词里。
	planner.planReqs = nil
	planner.synthReqs = nil
	planner.plan = Plan{Mode: ModeChat, Understanding: Understanding{Goal: "闲聊", Confidence: 0.9}}

	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "你好"}); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	for _, req := range planner.planReqs {
		if strings.Contains(req.UserPrompt, "先看结论") {
			t.Fatalf("候选记忆不应进入 Planner 上下文: %s", req.UserPrompt)
		}
	}
	for _, req := range planner.synthReqs {
		if strings.Contains(req.UserPrompt, "先看结论") {
			t.Fatalf("候选记忆不应进入 Synthesizer 上下文: %s", req.UserPrompt)
		}
	}
}

// TestMemoryWriteRejectsSensitiveContent 覆盖疑似凭证的候选记忆被拒绝。
func TestMemoryWriteRejectsSensitiveContent(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "用户说了点什么", TimeRange: "today", Confidence: 0.6,
			},
			ToolCalls: []tooling.Call{
				{Name: "get_today_status"},
				{Name: "save_memory_candidate", Arguments: map[string]any{
					"kind": "fact", "content": "用户的 API key 是 sk-abcdef123456", "confidence": 0.9,
				}},
			},
		},
		synth: SynthResult{Answer: "好的。", SupportLevel: SupportSupported},
	}
	agent := newTaskAgent(t, store, planner)

	reply, _ := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "记一下我的密钥"})
	if reply.MemoryCandidates != 0 {
		t.Fatalf("疑似凭证的内容不应落库，实际写入 %d 条", reply.MemoryCandidates)
	}
	if n, _ := store.CountMemoryCandidates(context.Background()); n != 0 {
		t.Fatalf("疑似凭证的内容不应落库，实际 %d 条", n)
	}
	// 工具主动拒绝也要作为"被拒步骤"告知模型，否则它会在回答里说"已经记下了"。
	if len(reply.DeniedTools) != 1 {
		t.Fatalf("应记录一次拒绝，实际 %+v", reply.DeniedTools)
	}
	prompt := planner.synthReqs[0].UserPrompt
	if !strings.Contains(prompt, "不要承诺已经记下") {
		t.Fatalf("合成提示词应提醒不要承诺已记下，实际: %s", prompt)
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
// 早期实现取 views[0].Start ~ views[last].End：多个工具的返回各自成序，
// 拼在一起整体并不有序，于是会写出一个根本不存在的区间。
// 这里刻意把最小的开始放在中间、最大的结束放在开头。
func TestEvidenceRangeUsesMinStartAndMaxEnd(t *testing.T) {
	mk := func(start string, dur int) tooling.Span {
		s := parseLocal(t, start)
		return tooling.Span{Start: s, End: s.Add(time.Duration(dur) * time.Minute)}
	}
	results := []tooling.Result{
		{Tool: "get_sessions", Kind: tooling.KindActivity, Count: 2,
			Span: tooling.MergeSpans(mk("2026-09-17 14:00", 90), mk("2026-09-17 09:00", 75))},
		{Tool: "get_today_status", Kind: tooling.KindActivity, Count: 2,
			Span: tooling.MergeSpans(mk("2026-09-17 19:45", 145), mk("2026-09-17 11:00", 60))},
	}

	line := evidenceLine(results, testLoc)
	if !strings.Contains(line, "2026-09-17 09:00 ~ 22:10") {
		t.Fatalf("区间应取最早开始 ~ 最晚结束，实际: %s", line)
	}
	if strings.Contains(line, "14:00 ~ 12:00") {
		t.Fatalf("区间不应取首尾元素，实际: %s", line)
	}
}

// TestEvidenceCountsComeFromToolResults 覆盖计数直接来自工具结果。
//
// 去重发生在工具内部（同一段记录被两个工具返回时只算一次），
// 因此上层不需要再猜"这两条是不是同一条"。
func TestEvidenceCountsComeFromToolResults(t *testing.T) {
	results := []tooling.Result{
		{Tool: "get_today_status", Kind: tooling.KindActivity, Count: 1,
			Evidence: []string{"s_dup"}},
		{Tool: "get_sessions", Kind: tooling.KindActivity, Count: 1,
			Evidence: []string{"s_dup"}},
	}

	line := evidenceLine(results, testLoc)
	if !strings.Contains(line, "2 段活动记录") {
		t.Fatalf("段数应来自工具报告的次数，实际: %s", line)
	}
	// 但可核实的来源 ID 必须去重：同一个记录不能算两份依据。
	ids := collectEvidence(results)
	if len(ids) != 1 || ids[0] != "s_dup" {
		t.Fatalf("来源 ID 应去重，实际 %v", ids)
	}
}

// TestEvidenceRangeHandlesOvernightSpan 覆盖跨夜时段的区间。
func TestEvidenceRangeHandlesOvernightSpan(t *testing.T) {
	start := parseLocal(t, "2026-09-17 23:30")
	results := []tooling.Result{
		{Tool: "get_sessions", Kind: tooling.KindActivity, Count: 2,
			Span: tooling.Span{Start: parseLocal(t, "2026-09-17 09:00"),
				End: parseLocal(t, "2026-09-18 00:40")}},
		// 这条的结束时间更早，不应影响结果。
		{Tool: "get_sessions", Kind: tooling.KindActivity, Count: 1,
			Span: tooling.Span{Start: start, End: start.Add(30 * time.Minute)}},
	}

	line := evidenceLine(results, testLoc)
	if !strings.Contains(line, "2026-09-18 00:40") {
		t.Fatalf("跨夜时段的最晚结束应带上日期，实际: %s", line)
	}
	if !strings.Contains(line, "2026-09-17 09:00") {
		t.Fatalf("最早开始应为 09:00，实际: %s", line)
	}
}

// TestEvidenceLineKeepsTaskAndActivitySeparate 覆盖两类来源分开呈现。
func TestEvidenceLineKeepsTaskAndActivitySeparate(t *testing.T) {
	results := []tooling.Result{
		{Tool: "get_task_summaries", Kind: tooling.KindReportedTasks, Count: 2,
			Evidence: []string{"t-1", "t-2"}},
		{Tool: "get_sessions", Kind: tooling.KindActivity, Count: 1,
			Evidence: []string{"s_1"}},
	}

	line := evidenceLine(results, testLoc)
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

// TestEvidenceLineIgnoresNonEvidenceKinds 覆盖系统信息的"不构成证据"。
//
// 当前时间、身份配置、对话状态都是系统信息：它们不能被算成"活动记录"，
// 否则用户会看到"1 段活动记录"这种根本没有事实支撑的证据行。
func TestEvidenceLineIgnoresNonEvidenceKinds(t *testing.T) {
	results := []tooling.Result{
		{Tool: "get_current_time", Kind: tooling.KindSystemInfo, Count: 0},
		{Tool: "get_assistant_profile", Kind: tooling.KindSystemInfo, Count: 0},
		{Tool: "get_known_projects", Kind: tooling.KindReference, Count: 3},
	}
	if line := evidenceLine(results, testLoc); line != "" {
		t.Fatalf("无证据类结果不应产生证据行，实际: %s", line)
	}
}

// parseLocal 解析本地时间字符串（测试辅助）。
func parseLocal(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, testLoc)
	if err != nil {
		t.Fatalf("解析时间 %q 失败: %v", value, err)
	}
	return parsed
}
