package assistant

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/identity"
	"lumen/server/internal/storage"
	"lumen/server/internal/tooling"
	"lumen/server/internal/tools"
)

// 本文件覆盖"上下文装配 + 记忆生命周期"在 Agent 链路上的集成行为：
//   - 已确认信息经 Bundle 注入，Planner 与 Synthesizer 看到同一份；
//   - 候选记忆正文在任何一轮的提示词里都不出现；
//   - 拒绝的候选永远不可见；
//   - 近期轮次作为不可信背景注入。

// newLifecycleAgent 组装带存储与完整工具集的 Agent（假模型）。
func newLifecycleAgent(t *testing.T, store *storage.Store, planner Planner) *Agent {
	t.Helper()
	registry, err := tooling.NewRegistry(tools.All(tools.Options{
		Store: store, Profile: identity.Default(), Location: testLoc,
	})...)
	if err != nil {
		t.Fatalf("构造注册表失败: %v", err)
	}
	executor, err := tooling.NewExecutor(tooling.ExecutorOptions{Registry: registry})
	if err != nil {
		t.Fatalf("构造执行器失败: %v", err)
	}
	return NewAgent(Options{
		Store: store, Planner: planner, Executor: executor,
		Profile: identity.Default(), Location: testLoc,
	})
}

func newLifecycleStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestConfirmedProfileVisibleNextTurn 覆盖验收 P1-5/P2-bundle一致：
// 确认偏好后，下一轮 Planner 与 Synthesizer 的 trusted 块逐字相同，
// 且都包含已确认条目（key+值+版本）。
func TestConfirmedProfileVisibleNextTurn(t *testing.T) {
	store := newLifecycleStore(t)
	// 模拟"管理端确认了一条偏好"后的读模型状态。
	if _, _, err := store.ConfirmProfileValue(context.Background(), storage.ProfileEntry{
		UserID: "u1", Key: "称呼", Kind: "preference", Value: "叫我梁哥",
	}, "c1", "admin"); err != nil {
		t.Fatalf("写入已确认信息失败: %v", err)
	}

	planner := &fakePlanner{
		enabled: true,
		plan:    Plan{Mode: ModeChat, Understanding: Understanding{Goal: "闲聊", Confidence: 0.9}},
		synth:   SynthResult{Answer: "好的。", SupportLevel: SupportSupported},
	}
	agent := newLifecycleAgent(t, store, planner)

	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "在吗"}); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if len(planner.planReqs) != 1 || len(planner.synthReqs) != 1 {
		t.Fatalf("应各调用一次规划与合成，实际 %d/%d", len(planner.planReqs), len(planner.synthReqs))
	}
	planBlock := extractTrusted(planner.planReqs[0].UserPrompt)
	synthBlock := extractTrusted(planner.synthReqs[0].UserPrompt)
	if planBlock == "" || planBlock != synthBlock {
		t.Fatalf("两个阶段的 trusted 块必须逐字相同:\nplan=%s\nsynth=%s", planBlock, synthBlock)
	}
	for _, want := range []string{"称呼", "叫我梁哥", "第 1 版", "已确认信息"} {
		if !strings.Contains(planBlock, want) {
			t.Fatalf("trusted 块应包含已确认信息 %q，实际:\n%s", want, planBlock)
		}
	}
}

// TestCandidateContentNeverInPrompts 覆盖验收 P0-3/P1-10：
// 保存候选的那一轮与之后的任何一轮，候选正文都不出现在提示词里；
// 确认之后才经由读模型可见。
func TestCandidateContentNeverInPrompts(t *testing.T) {
	store := newLifecycleStore(t)
	planner := &fakePlanner{enabled: true}
	planner.plan = Plan{
		Mode:          ModeRecall,
		Understanding: Understanding{Goal: "记住偏好", TimeRange: "today", Confidence: 0.9},
		ToolCalls: []tooling.Call{
			{Name: "get_today_status"},
			{Name: "save_memory_candidate", Arguments: map[string]any{
				"kind": "preference", "key": "回答风格",
				"content": "用户偏好先看结论再看细节", "confidence": 0.9,
			}},
		},
	}
	planner.synth = SynthResult{Answer: "记下了（候选，未生效）。", SupportLevel: SupportSupported}
	agent := newLifecycleAgent(t, store, planner)

	// 第一轮：写入候选。当轮提示词里就不得出现"已确认信息"段。
	seedSession(t, store, time.Now().In(testLoc).Format("2006-01-02"), "lumen")
	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "以后先说结论"}); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	for i, req := range planner.planReqs {
		if strings.Contains(extractTrusted(req.UserPrompt), "已确认信息") {
			t.Fatalf("第 %d 轮规划提示词不应有已确认信息段", i+1)
		}
	}

	// 第二轮：chat，检查全部提示词。候选正文绝不能出现。
	planner.planReqs = nil
	planner.synthReqs = nil
	planner.plan = Plan{Mode: ModeChat, Understanding: Understanding{Goal: "闲聊", Confidence: 0.9}}
	planner.synth = SynthResult{Answer: "好的。", SupportLevel: SupportSupported}
	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "在吗"}); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	for _, req := range planner.planReqs {
		if strings.Contains(req.UserPrompt, "先看结论再看细节") {
			t.Fatalf("候选正文泄漏进规划提示词:\n%s", req.UserPrompt)
		}
	}
	for _, req := range planner.synthReqs {
		if strings.Contains(req.UserPrompt, "先看结论再看细节") {
			t.Fatalf("候选正文泄漏进合成提示词:\n%s", req.UserPrompt)
		}
	}

	// 管理端确认后，下一轮经由读模型可见（且仍不出现候选行原文以外的内容）。
	if _, _, err := store.ConfirmProfileValue(context.Background(), storage.ProfileEntry{
		UserID: "u1", Key: "回答风格", Kind: "preference", Value: "先看结论再看细节",
	}, "c_confirm", "admin"); err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	planner.planReqs = nil
	planner.synthReqs = nil
	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "在吗"}); err != nil {
		t.Fatalf("第三轮失败: %v", err)
	}
	planBlock := extractTrusted(planner.planReqs[0].UserPrompt)
	if !strings.Contains(planBlock, "先看结论再看细节") {
		t.Fatalf("确认后下一轮应包含该条目，实际:\n%s", planBlock)
	}
	if extractTrusted(planner.synthReqs[0].UserPrompt) != planBlock {
		t.Fatal("确认后两个阶段的 trusted 块仍必须逐字相同")
	}
}

// TestRejectedCandidateInvisible 覆盖验收 P1-6：拒绝的候选不进读模型、不进提示词。
func TestRejectedCandidateInvisible(t *testing.T) {
	store := newLifecycleStore(t)
	if err := store.SaveMemoryCandidate(context.Background(), storage.MemoryCandidate{
		ID: "c_rej", UserID: "u1", Kind: "preference", Content: "叫我隔壁老王",
		Key: "称呼", SourceIDs: []string{"u_t1"},
	}); err != nil {
		t.Fatalf("写候选失败: %v", err)
	}
	if _, err := store.RejectMemoryCandidate(context.Background(), "c_rej", "admin", "不是这么叫的"); err != nil {
		t.Fatalf("写入拒绝决策失败: %v", err)
	}

	planner := &fakePlanner{
		enabled: true,
		plan:    Plan{Mode: ModeChat, Understanding: Understanding{Goal: "闲聊", Confidence: 0.9}},
		synth:   SynthResult{Answer: "好的。", SupportLevel: SupportSupported},
	}
	agent := newLifecycleAgent(t, store, planner)
	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "在吗"}); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	prompt := planner.planReqs[0].UserPrompt + planner.synthReqs[0].UserPrompt
	if strings.Contains(prompt, "隔壁老王") || strings.Contains(prompt, "已确认信息") {
		t.Fatalf("拒绝的候选不得出现在提示词里:\n%s", prompt)
	}
}

// TestRecentTurnsInjectedAsUntrusted 覆盖：近期轮次进入下一轮提示词
// （不可信块），帮助解析"刚才说的"。
func TestRecentTurnsInjectedAsUntrusted(t *testing.T) {
	store := newLifecycleStore(t)
	if err := store.RecordConversation(context.Background(), "m1", "msg1", "u1", "chat",
		`{"raw":"lumen 项目最近怎么样"}`, "今天投入 90 分钟。", "[]", "ok"); err != nil {
		t.Fatalf("写对话记录失败: %v", err)
	}

	planner := &fakePlanner{
		enabled: true,
		plan:    Plan{Mode: ModeChat, Understanding: Understanding{Goal: "闲聊", Confidence: 0.9}},
		synth:   SynthResult{Answer: "好的。", SupportLevel: SupportSupported},
	}
	agent := newLifecycleAgent(t, store, planner)
	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "在吗"}); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	planPrompt := planner.planReqs[0].UserPrompt
	if !strings.Contains(planPrompt, "<recent_turns") || !strings.Contains(planPrompt, "lumen 项目最近怎么样") {
		t.Fatalf("近期轮次应注入规划提示词，实际:\n%s", planPrompt)
	}
	if !strings.Contains(planner.synthReqs[0].UserPrompt, "lumen 项目最近怎么样") {
		t.Fatal("近期轮次也应注入合成提示词（同一 Bundle）")
	}
	// trusted 块里不出现（用户文本不可信）。
	if strings.Contains(extractTrusted(planPrompt), "lumen 项目最近怎么样") {
		t.Fatal("近期轮次原文不得进入 trusted 块")
	}
}

// TestTruncationMetadataSurfacesInReply 覆盖：截断元信息透出到 Reply，
// 供调试接口解释"这轮模型看到的上下文缺了什么"。
func TestTruncationMetadataSurfacesInReply(t *testing.T) {
	// 有存储但没有用户 ID：装配器降级并留下元信息。
	store := newLifecycleStore(t)
	planner := &fakePlanner{
		enabled: true,
		plan:    Plan{Mode: ModeChat, Understanding: Understanding{Goal: "闲聊", Confidence: 0.9}},
		synth:   SynthResult{Answer: "好的。", SupportLevel: SupportSupported},
	}
	agent := newLifecycleAgent(t, store, planner)
	reply, err := agent.Handle(context.Background(), Turn{UserID: "", Text: "在吗"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if len(reply.Truncations) == 0 {
		t.Fatalf("降级场景应留下截断/降级元信息，实际 %+v", reply.Truncations)
	}
}
