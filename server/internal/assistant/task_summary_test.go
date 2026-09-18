package assistant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/storage"
)

// seedTaskSummary 写入一条任务摘要，返回它的 task_id。
func seedTaskSummary(t *testing.T, store *storage.Store, taskID, title, status string,
	outcomes, openLoops []string, occurredAt time.Time) string {
	t.Helper()
	_, err := store.UpsertTaskSummary(context.Background(), storage.TaskSummary{
		ID: "evt-" + taskID, DeviceID: "desktop-mac-01", TaskID: taskID,
		Project: "lumen", App: "ZCode", Title: title, Status: status,
		Outcomes: outcomes, OpenLoops: openLoops,
		SourceAgent: "zcode-cli", SourceSessionID: "sess-hidden-001",
		OccurredAt: occurredAt, UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("写入任务摘要失败: %v", err)
	}
	return taskID
}

func newTaskAgent(t *testing.T, store *storage.Store, planner Planner) *Agent {
	t.Helper()
	return newTestAgent(t, store, DefaultProfile(), planner)
}

// TestPlannerCanSelectTaskSummaryCapability 覆盖"多种自然问法都能选中该能力"。
//
// 这不是在测模型会不会措辞，而是在测：只要模型做出检索决定，
// 链路就真的返回任务摘要并把它作为可追溯来源。问法本身不进 Go 代码，
// 因此这里穷举多少种说法都不需要改实现。
func TestPlannerCanSelectTaskSummaryCapability(t *testing.T) {
	today := time.Now().In(testLoc)
	store := newTestStore(t, today.Format("2006-01-02"), "lumen")
	taskID := seedTaskSummary(t, store, "zcode-2026-09-17-001",
		"接通任务摘要链路", "done",
		[]string{"新增 agent.task_summary 事件类型"}, []string{"尚未接入真实上报"},
		today)

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "用户想知道完成了什么", TimeRange: "today", Confidence: 0.9,
			},
			ToolCalls: []ToolCall{{Name: "get_task_summaries", Arguments: map[string]any{
				"date": today.Format("2006-01-02"),
			}}},
		},
		synth: SynthResult{
			Answer:       "ZCode 那边报告完成了一件：接通任务摘要链路。",
			SupportLevel: SupportSupported,
		},
	}
	agent := newTaskAgent(t, store, planner)

	// 同一意图的多种自然说法，全部走模型计划而不经过任何关键词匹配。
	phrasings := []string{
		"我完成了什么",
		"今天做完了哪些事",
		"看看今天的成果",
		"ZCode 那边报了啥结果",
		"有啥没做完的",
	}
	for _, text := range phrasings {
		reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: text})
		if err != nil {
			t.Fatalf("%q 处理失败: %v", text, err)
		}
		if len(reply.ToolCalls) != 1 || reply.ToolCalls[0] != "get_task_summaries" {
			t.Fatalf("%q 应选择 get_task_summaries，实际 %v", text, reply.ToolCalls)
		}
		// 来源必须可追溯：task_id 是来源 Agent 的稳定标识。
		if len(reply.SourceTaskIDs) != 1 || reply.SourceTaskIDs[0] != taskID {
			t.Fatalf("%q 应带回 task_id 来源，实际 %v", text, reply.SourceTaskIDs)
		}
	}

	// 能力返回的事实必须真的进了合成阶段的输入。
	last := planner.synthReqs[len(planner.synthReqs)-1]
	if !strings.Contains(last.UserPrompt, "接通任务摘要链路") {
		t.Fatalf("合成阶段应拿到任务摘要，实际: %s", last.UserPrompt)
	}
	if !strings.Contains(last.UserPrompt, "新增 agent.task_summary 事件类型") {
		t.Fatalf("合成阶段应拿到已产出结果，实际: %s", last.UserPrompt)
	}
}

// TestTaskSummaryMarksSupportedButActivityIsInferred 是本阶段的核心验收：
// 回答必须区分"Agent 报告的结果"与"从活动记录推断"。
func TestTaskSummaryMarksSupportedButActivityIsInferred(t *testing.T) {
	today := time.Now().In(testLoc)
	store := newTestStore(t, today.Format("2006-01-02"), "lumen")
	seedTaskSummary(t, store, "zcode-001", "接通任务摘要链路", "done",
		[]string{"新增事件类型"}, nil, today)

	date := today.Format("2006-01-02")
	synth := SynthResult{Answer: "本次汇报如下。", SupportLevel: SupportSupported}

	t.Run("只有任务摘要时可以标 supported", func(t *testing.T) {
		planner := &fakePlanner{
			enabled: true,
			plan: Plan{
				Mode:          ModeRecall,
				Understanding: Understanding{Goal: "完成了什么", TimeRange: "today", Confidence: 0.9},
				ToolCalls:     []ToolCall{{Name: "get_task_summaries", Arguments: map[string]any{"date": date}}},
			},
			synth: synth,
		}
		reply, _ := newTaskAgent(t, store, planner).Handle(
			context.Background(), Turn{UserID: "u1", Text: "完成了什么"})
		if reply.SupportLevel != SupportSupported {
			t.Fatalf("有 Agent 报告时应保持 supported，实际 %s", reply.SupportLevel)
		}
		if !strings.Contains(reply.Text, "Agent 报告") {
			t.Fatalf("证据行应说明来源是 Agent 报告，实际: %s", reply.Text)
		}
	})

	t.Run("声称完成但只有活动记录时必须降为 inferred", func(t *testing.T) {
		// 模型自己标了 supported，并且在回答里宣称"完成了"——
		// 这在活动记录上一定是过度自信，因为"用了 ZCode 90 分钟"
		// 推不出"完成了什么"。
		planner := &fakePlanner{
			enabled: true,
			plan: Plan{
				Mode:          ModeRecall,
				Understanding: Understanding{Goal: "完成了什么", TimeRange: "today", Confidence: 0.9},
				ToolCalls:     []ToolCall{{Name: "get_today_status"}},
			},
			synth: SynthResult{Answer: "看起来完成了不少工作。", SupportLevel: SupportSupported},
		}
		reply, _ := newTaskAgent(t, store, planner).Handle(
			context.Background(), Turn{UserID: "u1", Text: "完成了什么"})
		if reply.SupportLevel != SupportInferred {
			t.Fatalf("声称完成但无 Agent 报告时应强制降为 inferred，实际 %s", reply.SupportLevel)
		}
		if len(reply.SourceTaskIDs) != 0 {
			t.Fatalf("没有任务摘要时不应有任务来源，实际 %v", reply.SourceTaskIDs)
		}
	})

	t.Run("单纯陈述活动事实时保留 supported", func(t *testing.T) {
		// 收紧必须精确：只说"用了某应用多久"确实直接来自事实，
		// 无差别降级会让用户对正确的标注也失去信任。
		planner := &fakePlanner{
			enabled: true,
			plan: Plan{
				Mode:          ModeRecall,
				Understanding: Understanding{Goal: "用了什么", TimeRange: "today", Confidence: 0.9},
				ToolCalls:     []ToolCall{{Name: "get_today_status"}},
			},
			synth: SynthResult{
				Answer:       "今天用了 ZCode 约 90 分钟。",
				SupportLevel: SupportSupported,
			},
		}
		reply, _ := newTaskAgent(t, store, planner).Handle(
			context.Background(), Turn{UserID: "u1", Text: "今天用了哪些应用"})
		if reply.SupportLevel != SupportSupported {
			t.Fatalf("单纯陈述活动事实时应保留 supported，实际 %s", reply.SupportLevel)
		}
	})

	t.Run("两者都有时来源分开记录", func(t *testing.T) {
		planner := &fakePlanner{
			enabled: true,
			plan: Plan{
				Mode:          ModeRecall,
				Understanding: Understanding{Goal: "完成了什么", TimeRange: "today", Confidence: 0.9},
				ToolCalls: []ToolCall{
					{Name: "get_task_summaries", Arguments: map[string]any{"date": date}},
					{Name: "get_today_status"},
				},
			},
			synth: synth,
		}
		reply, _ := newTaskAgent(t, store, planner).Handle(
			context.Background(), Turn{UserID: "u1", Text: "完成了什么"})
		if reply.SupportLevel != SupportSupported {
			t.Fatalf("有 Agent 报告时可标 supported，实际 %s", reply.SupportLevel)
		}
		if len(reply.SourceTaskIDs) == 0 {
			t.Fatal("应记录任务来源")
		}
		if len(reply.SourceSessionIDs) == 0 {
			t.Fatal("应同时记录时段来源")
		}
		// 证据行必须把两类来源分开写，用户才能自己判断可信度。
		if !strings.Contains(reply.Text, "Agent 报告") || !strings.Contains(reply.Text, "活动记录") {
			t.Fatalf("证据行应同时标注两类来源，实际: %s", reply.Text)
		}
	})
}

// TestTaskSummaryCapabilityHidesSourceSessionID 覆盖内部标识不外泄。
//
// source_session_id 是来源 Agent 的会话 ID，用于追溯；
// 它既不该进模型上下文，也不该出现在用户可见文本里。
func TestTaskSummaryCapabilityHidesSourceSessionID(t *testing.T) {
	today := time.Now().In(testLoc)
	store := newTestStore(t, today.Format("2006-01-02"), "lumen")
	seedTaskSummary(t, store, "zcode-001", "接通任务摘要链路", "done",
		[]string{"新增事件类型"}, nil, today)

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode:          ModeRecall,
			Understanding: Understanding{Goal: "完成了什么", TimeRange: "today", Confidence: 0.9},
			ToolCalls: []ToolCall{{Name: "get_task_summaries", Arguments: map[string]any{
				"date": today.Format("2006-01-02"),
			}}},
		},
		synth: SynthResult{Answer: "本次汇报如下。", SupportLevel: SupportSupported},
	}
	reply, _ := newTaskAgent(t, store, planner).Handle(
		context.Background(), Turn{UserID: "u1", Text: "完成了什么"})

	prompt := planner.synthReqs[0].UserPrompt
	if strings.Contains(prompt, "sess-hidden-001") {
		t.Fatalf("来源会话 ID 不应进入模型上下文: %s", prompt)
	}
	if strings.Contains(reply.Text, "sess-hidden-001") {
		t.Fatalf("来源会话 ID 不应出现在用户可见文本: %s", reply.Text)
	}
	// 但 task_id 要保留：它是可追溯的稳定标识。
	if !strings.Contains(prompt, "zcode-001") {
		t.Fatalf("task_id 应进入模型上下文用于追溯: %s", prompt)
	}
}

// TestTaskSummaryCapabilityClipsHistoricalData 覆盖历史数据的防御性裁剪。
//
// 入库时已校验过，但库里可能有早期版本或人工写入的超长内容，
// 不能假设它们一定符合当前约束。
func TestTaskSummaryCapabilityClipsHistoricalData(t *testing.T) {
	today := time.Now().In(testLoc)
	store := newTestStore(t, today.Format("2006-01-02"), "lumen")

	longTitle := strings.Repeat("长", 500)
	manyOutcomes := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		manyOutcomes = append(manyOutcomes, strings.Repeat("果", 300))
	}
	seedTaskSummary(t, store, "zcode-long", longTitle, "done", manyOutcomes, nil, today)

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode:          ModeRecall,
			Understanding: Understanding{Goal: "完成了什么", TimeRange: "today", Confidence: 0.9},
			ToolCalls: []ToolCall{{Name: "get_task_summaries", Arguments: map[string]any{
				"date": today.Format("2006-01-02"),
			}}},
		},
		synth: SynthResult{Answer: "已裁剪。", SupportLevel: SupportSupported},
	}
	reply, _ := newTaskAgent(t, store, planner).Handle(
		context.Background(), Turn{UserID: "u1", Text: "完成了什么"})

	prompt := planner.synthReqs[0].UserPrompt
	if len([]rune(prompt)) > 4000 {
		t.Fatalf("超长历史数据不应整体进入上下文，实际 %d 字", len([]rune(prompt)))
	}
	if reply.Status != "ok" {
		t.Fatalf("处理应正常完成，实际状态 %s", reply.Status)
	}
}

// TestTaskSummaryCapabilityRequiresScope 覆盖"必须给出 date 或 project"。
//
// 不给范围就等于拉全表，这是 Policy Gate 之外的能力内约束。
func TestTaskSummaryCapabilityRequiresScope(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	capability := &TaskSummariesCapability{Store: store, Loc: testLoc}

	if _, err := capability.Run(context.Background(), map[string]any{}); err == nil {
		t.Fatal("缺少 date 与 project 时应报错")
	}
	if _, err := capability.Run(context.Background(),
		map[string]any{"date": "不是日期"}); err == nil {
		t.Fatal("非法日期格式应报错")
	}
}

// TestTaskSummariesPipelineThroughAPI 覆盖从事件校验到能力返回的完整链路。
//
// 用手工构造的 payload 走真实校验器，验证"协议说的"与"能力给的"一致。
func TestTaskSummariesPipelineThroughAPI(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")

	// 模拟服务端投影：upsert 两次同一 task_id，验证幂等覆盖。
	occurred := time.Date(2026, 9, 17, 15, 42, 0, 0, testLoc)
	if _, err := store.UpsertTaskSummary(context.Background(), storage.TaskSummary{
		ID: "evt-1", DeviceID: "dev-1", TaskID: "task-A", Project: "lumen", App: "ZCode",
		Title: "第一版标题", Status: "partial", Outcomes: []string{"做了点"},
		SourceAgent: "zcode-cli", OccurredAt: occurred, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	created, err := store.UpsertTaskSummary(context.Background(), storage.TaskSummary{
		ID: "evt-2", DeviceID: "dev-1", TaskID: "task-A", Project: "lumen", App: "ZCode",
		Title: "最终标题", Status: "done", Outcomes: []string{"做完了", "还加了测试"},
		SourceAgent: "zcode-cli", OccurredAt: occurred, UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}
	if created {
		t.Fatal("同一 task_id 的重复汇报应被识别为更新，而不是新建")
	}

	n, err := store.CountTaskSummaries(context.Background())
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("同一 task_id 应只保留一条，实际 %d 条", n)
	}

	// 能力读出来的是最新内容。
	capability := &TaskSummariesCapability{Store: store, Loc: testLoc}
	raw, err := capability.Run(context.Background(), map[string]any{"date": "2026-09-17"})
	if err != nil {
		t.Fatalf("能力执行失败: %v", err)
	}
	result, ok := raw.(TaskSummariesResult)
	if !ok {
		t.Fatalf("返回类型不符: %T", raw)
	}
	if result.Count != 1 {
		t.Fatalf("应有 1 条任务摘要，实际 %d", result.Count)
	}
	got := result.TaskSummaries[0]
	if got.Title != "最终标题" || got.Status != "done" {
		t.Fatalf("应返回最新汇报，实际 %+v", got)
	}
	if len(got.Outcomes) != 2 {
		t.Fatalf("应返回最新结果列表，实际 %v", got.Outcomes)
	}
	if result.StatusCounts["done"] != 1 {
		t.Fatalf("状态汇总应统计 done，实际 %v", result.StatusCounts)
	}
}

// TestTaskSummaryCatalogIsVisibleToPlanner 覆盖能力目录进入 Planner 提示词。
//
// 模型不知道有这个能力就不可能选它 —— 目录是"模型能自己发现新能力"的机制。
func TestTaskSummaryCatalogIsVisibleToPlanner(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan:    Plan{Mode: ModeChat, Understanding: Understanding{Confidence: 0.9}},
		synth:   SynthResult{Answer: "好。", SupportLevel: SupportSupported},
	}
	registry := NewRegistry(
		&ProfileCapability{Profile: DefaultProfile()},
		&TaskSummariesCapability{Store: store, Loc: testLoc},
	)
	agent := NewAgent(Options{
		Store: store, Planner: planner, Registry: registry,
		Profile: DefaultProfile(), Location: testLoc,
	})

	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "你好"}); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	system := planner.planReqs[0].SystemPrompt
	if !strings.Contains(system, "get_task_summaries") {
		t.Fatalf("能力目录应包含 get_task_summaries，实际: %s", system)
	}
	// 提示词还要说明它与活动记录的区别，否则模型可能用错能力。
	if !strings.Contains(system, "结论") {
		t.Fatalf("提示词应说明任务摘要是带结论的数据源，实际: %s", system)
	}
}

// TestTaskSummaryStatusCountsAreReported 覆盖状态汇总，让模型能直接说"几个完成几个受阻"。
func TestTaskSummaryStatusCountsAreReported(t *testing.T) {
	today := time.Now().In(testLoc)
	store := newTestStore(t, today.Format("2006-01-02"), "lumen")
	occurred := today
	seedTaskSummary(t, store, "t-done", "做完了 A", "done", nil, nil, occurred)
	seedTaskSummary(t, store, "t-blocked", "卡在 B", "blocked", nil,
		[]string{"等上游接口"}, occurred)
	seedTaskSummary(t, store, "t-partial", "做了一半 C", "partial", nil, nil, occurred)

	capability := &TaskSummariesCapability{Store: store, Loc: testLoc}
	raw, err := capability.Run(context.Background(),
		map[string]any{"date": today.Format("2006-01-02")})
	if err != nil {
		t.Fatalf("能力执行失败: %v", err)
	}
	result := raw.(TaskSummariesResult)
	if result.Count != 3 {
		t.Fatalf("应有 3 条，实际 %d", result.Count)
	}
	if result.StatusCounts["done"] != 1 || result.StatusCounts["blocked"] != 1 ||
		result.StatusCounts["partial"] != 1 {
		t.Fatalf("状态汇总不正确: %v", result.StatusCounts)
	}

	// 受阻任务的 open_loops 必须保留，那是用户最需要看到的部分。
	var blocked TaskSummaryView
	for _, v := range result.TaskSummaries {
		if v.Status == "blocked" {
			blocked = v
		}
	}
	if len(blocked.OpenLoops) != 1 || blocked.OpenLoops[0] != "等上游接口" {
		t.Fatalf("受阻任务应保留未完成事项，实际 %+v", blocked)
	}
}

// TestTaskSummaryFactsReachRenderFacts 覆盖降级路径也能呈现任务摘要。
//
// 合成失败时用原始事实拼回答，这条路径同样要能说出"Agent 报告了什么"。
func TestTaskSummaryFactsReachRenderFacts(t *testing.T) {
	results := []CapabilityResult{{
		Name: "get_task_summaries",
		Value: TaskSummariesResult{
			Query: "date=2026-09-17",
			TaskSummaries: []TaskSummaryView{{
				TaskID: "t-1", Title: "接通任务摘要链路", Status: "done",
				Outcomes: []string{"新增事件类型"}, SourceAgent: "zcode-cli",
			}},
			Count:        1,
			StatusCounts: map[string]int{"done": 1},
		},
	}}

	body := renderFacts(results, testLoc)
	if !strings.Contains(body, "接通任务摘要链路") {
		t.Fatalf("降级摘要应包含任务标题，实际: %s", body)
	}
	if !strings.Contains(body, "新增事件类型") {
		t.Fatalf("降级摘要应包含已产出结果，实际: %s", body)
	}

	line := evidenceLine(results)
	if !strings.Contains(line, "Agent 报告") {
		t.Fatalf("证据行应标注 Agent 报告来源，实际: %s", line)
	}
}

// TestTaskSummaryJSONHasNoInternalIDs 覆盖能力返回结构的序列化边界。
func TestTaskSummaryJSONHasNoInternalIDs(t *testing.T) {
	view := TaskSummaryView{
		TaskID: "t-1", Title: "标题", Status: "done",
		Outcomes: []string{"结果"}, SourceAgent: "zcode-cli",
		OccurredAt: "2026-09-17 15:42",
	}
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	// 视图结构里根本没有 source_session_id 字段，因此不可能被序列化出去。
	if strings.Contains(string(b), "source_session_id") {
		t.Fatalf("视图不应包含来源会话 ID: %s", b)
	}
	if strings.Contains(string(b), "sess-") {
		t.Fatalf("序列化结果不应含会话 ID: %s", b)
	}
}

// TestMemoryCandidatesStayOutOfContext 覆盖候选记忆的隔离。
//
// liang 明确要求：候选记忆只有落库没有确认入口，必须保持候选隔离，
// 不能让它进入回答上下文。这条测试把这个约束钉住：写进去的候选内容
// 不得出现在后续任何一轮的 Planner 或 Synthesizer 输入里。
func TestMemoryCandidatesStayOutOfContext(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")

	// 第一轮：模型提议一条候选记忆并落库。
	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode:          ModeChat,
			Understanding: Understanding{Goal: "用户表达了偏好", Confidence: 0.8},
			MemoryCandidates: []MemoryCandidate{{
				Kind: "preference", Content: "用户偏好先看结论再看细节", Confidence: 0.7,
			}},
		},
		synth: SynthResult{Answer: "记下了。", SupportLevel: SupportSupported},
	}
	agent := newTaskAgent(t, store, planner)

	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "以后先说结论"}); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if n, _ := store.CountMemoryCandidates(context.Background()); n != 1 {
		t.Fatalf("应写入 1 条候选，实际 %d", n)
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

	// 已确认记忆同样还没有读取路径：V0.1 没有确认入口，
	// 因此 ConfirmedMemories 恒为空，也不会被 Agent 使用。
	confirmed, err := store.ConfirmedMemories(context.Background(), "u1", 10)
	if err != nil {
		t.Fatalf("查询已确认记忆失败: %v", err)
	}
	if len(confirmed) != 0 {
		t.Fatalf("候选不应自动晋升为已确认，实际 %d 条", len(confirmed))
	}
}
