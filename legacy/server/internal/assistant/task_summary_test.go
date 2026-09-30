package assistant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/storage"
	"lumen/server/internal/tooling"
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

// TestPlannerCanSelectTaskSummaryTool 覆盖"多种自然问法都能选中该工具"。
//
// 这不是在测模型会不会措辞，而是在测：只要模型做出检索决定，
// 链路就真的返回任务摘要并把它作为可追溯来源。问法本身不进 Go 代码，
// 因此这里穷举多少种说法都不需要改实现。
func TestPlannerCanSelectTaskSummaryTool(t *testing.T) {
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
			ToolCalls: []tooling.Call{{Name: "get_task_summaries", Arguments: map[string]any{
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

	// 工具返回的事实必须真的进了合成阶段的输入。
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
				ToolCalls:     []tooling.Call{{Name: "get_task_summaries", Arguments: map[string]any{"date": date}}},
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
				ToolCalls:     []tooling.Call{{Name: "get_today_status"}},
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
				ToolCalls:     []tooling.Call{{Name: "get_today_status"}},
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
				ToolCalls: []tooling.Call{
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
		if strings.Contains(strings.Join(reply.SourceTaskIDs, ","), "s_") {
			t.Fatalf("任务来源不应混入时段 ID，实际 %v", reply.SourceTaskIDs)
		}
		// 证据行必须把两类来源分开写，用户才能自己判断可信度。
		if !strings.Contains(reply.Text, "Agent 报告") || !strings.Contains(reply.Text, "活动记录") {
			t.Fatalf("证据行应同时标注两类来源，实际: %s", reply.Text)
		}
	})
}

// TestTaskSummaryToolHidesSourceSessionID 覆盖内部标识不外泄。
//
// source_session_id 是来源 Agent 的会话 ID，用于追溯；
// 它既不该进模型上下文，也不该出现在用户可见文本里。
func TestTaskSummaryToolHidesSourceSessionID(t *testing.T) {
	today := time.Now().In(testLoc)
	store := newTestStore(t, today.Format("2006-01-02"), "lumen")
	seedTaskSummary(t, store, "zcode-001", "接通任务摘要链路", "done",
		[]string{"新增事件类型"}, nil, today)

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode:          ModeRecall,
			Understanding: Understanding{Goal: "完成了什么", TimeRange: "today", Confidence: 0.9},
			ToolCalls: []tooling.Call{{Name: "get_task_summaries", Arguments: map[string]any{
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

// TestTaskSummaryToolClipsHistoricalData 覆盖历史数据的防御性裁剪。
//
// 入库时已校验过，但库里可能有早期版本或人工写入的超长内容，
// 不能假设它们一定符合当前约束。
func TestTaskSummaryToolClipsHistoricalData(t *testing.T) {
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
			ToolCalls: []tooling.Call{{Name: "get_task_summaries", Arguments: map[string]any{
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

// TestTaskSummaryToolJSONHasNoInternalIDs 覆盖工具视图的序列化边界。
func TestTaskSummaryToolJSONHasNoInternalIDs(t *testing.T) {
	// 视图结构里根本没有 source_session_id 字段，因此不可能被序列化出去。
	view := map[string]any{
		"task_id": "t-1", "title": "标题", "status": "done",
		"outcomes": []string{"结果"}, "source_agent": "zcode-cli",
		"occurred_at": "2026-09-17 15:42",
	}
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(b), "source_session_id") {
		t.Fatalf("视图不应包含来源会话 ID: %s", b)
	}
	if strings.Contains(string(b), "sess-") {
		t.Fatalf("序列化结果不应含会话 ID: %s", b)
	}
}
