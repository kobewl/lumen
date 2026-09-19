package assistant

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/identity"
	"lumen/server/internal/storage"
	"lumen/server/internal/tooling"
	"lumen/server/internal/tools"
)

// fakePlanner 是测试用的假模型。
//
// 为什么不用 httptest 打真实 HTTP：这里要验证的是"模型说了什么"导致的代码行为
// ——工具选择、权限拦截、来源校验、身份注入——而不是模型措辞的质量。
// 把固定字符串当 AI 质量断言，既测不出真实质量，又会在提示词微调时误报。
type fakePlanner struct {
	enabled  bool
	plan     Plan
	planErr  error
	synth    SynthResult
	synthErr error

	planReqs  []PlanRequest
	synthReqs []SynthesizeRequest
}

func (f *fakePlanner) Enabled() bool { return f.enabled }

func (f *fakePlanner) Plan(_ context.Context, req PlanRequest) (Plan, ai.Response, error) {
	f.planReqs = append(f.planReqs, req)
	if f.planErr != nil {
		return Plan{}, ai.Response{}, f.planErr
	}
	return f.plan, ai.Response{Usage: ai.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, nil
}

func (f *fakePlanner) Synthesize(_ context.Context, req SynthesizeRequest) (SynthResult, ai.Response, error) {
	f.synthReqs = append(f.synthReqs, req)
	if f.synthErr != nil {
		return SynthResult{}, ai.Response{}, f.synthErr
	}
	return f.synth, ai.Response{Usage: ai.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, nil
}

var testLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// newTestStore 建一个带一条工作时段的内存库。
func newTestStore(t *testing.T, date, project string) *storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedSession(t, store, date, project)
	return store
}

// seedSession 往库里写一条 09:00~10:30 的时段记录。
func seedSession(t *testing.T, store *storage.Store, date, project string) {
	t.Helper()
	start := time.Date(2026, 9, 17, 9, 0, 0, 0, testLoc)
	if date != "" {
		parsed, err := time.ParseInLocation("2006-01-02", date, testLoc)
		if err != nil {
			t.Fatalf("日期解析失败: %v", err)
		}
		start = parsed.Add(9 * time.Hour)
	}

	apps, _ := json.Marshal([]map[string]any{{"app": "ZCode", "duration_minutes": 90}})
	gits, _ := json.Marshal([]map[string]any{{"branch": "main", "commit_message": "feat: 接通 Agent 链路"}})
	stats, _ := json.Marshal(map[string]any{"duration_minutes": 90, "app_count": 1, "git_event_count": 1})

	sess := storage.Session{
		ID: "s_test_session_0001", Date: start.Format("2006-01-02"), Project: project,
		StartAt: start, EndAt: start.Add(90 * time.Minute),
		AppsJSON: string(apps), GitJSON: string(gits), StatsJSON: string(stats),
		AlgorithmVersion: "rules-v2",
		SourceStartAt:    start, SourceEndAt: start.Add(24 * time.Hour),
		UpdatedAt: time.Now().UTC(),
	}
	from := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, testLoc).UTC()
	if err := store.ReplaceSessions(context.Background(), sess.Date, from, from.Add(24*time.Hour),
		[]storage.Session{sess}); err != nil {
		t.Fatalf("写入 Session 失败: %v", err)
	}
}

// testHarness 是"注册表 + 执行器 + 编排器"的测试装配。
//
// 审计写入点用内存实现：测试可以断言"每一次调用都留了痕"，
// 而不需要真的建库。
type testHarness struct {
	agent    *Agent
	planner  *fakePlanner
	audit    *tooling.MemorySink
	registry *tooling.Registry
	store    *storage.Store
}

// newHarness 组装一个使用假模型、身份可定制、审计可断言的 Agent。
func newHarness(t *testing.T, store *storage.Store, profile identity.Profile,
	planner *fakePlanner, toolSet ...tooling.Tool) *testHarness {
	t.Helper()

	if len(toolSet) == 0 {
		toolSet = tools.All(tools.Options{
			Store: store, Profile: profile, Location: testLoc,
		})
	}
	registry, err := tooling.NewRegistry(toolSet...)
	if err != nil {
		t.Fatalf("构造工具注册表失败: %v", err)
	}
	audit := &tooling.MemorySink{}
	executor, err := tooling.NewExecutor(tooling.ExecutorOptions{
		Registry: registry, Audit: audit,
	})
	if err != nil {
		t.Fatalf("构造工具执行器失败: %v", err)
	}

	agent := NewAgent(Options{
		Store:    store,
		Planner:  planner,
		Executor: executor,
		Profile:  profile,
		Location: testLoc,
	})
	return &testHarness{agent: agent, planner: planner, audit: audit, registry: registry, store: store}
}

// newTestAgent 是大多数测试需要的简写。
func newTestAgent(t *testing.T, store *storage.Store, profile identity.Profile, planner Planner) *Agent {
	t.Helper()
	fake, ok := planner.(*fakePlanner)
	if !ok {
		t.Fatal("newTestAgent 需要 *fakePlanner")
	}
	return newHarness(t, store, profile, fake).agent
}

// newTaskAgent 使用系统默认身份。
func newTaskAgent(t *testing.T, store *storage.Store, planner Planner) *Agent {
	t.Helper()
	return newTestAgent(t, store, identity.Default(), planner)
}

// TestAgentExecutesPlannedTool 覆盖主链路：模型出计划 → 执行工具 → 合成回答。
func TestAgentExecutesPlannedTool(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "用户想知道今天的记录", TimeRange: "today", Confidence: 0.9,
			},
			ToolCalls: []tooling.Call{{Name: "get_today_status"}},
		},
		synth: SynthResult{Answer: "今天主要是 ZCode，约 90 分钟。", SupportLevel: SupportSupported},
	}
	h := newHarness(t, store, identity.Default(), planner)

	reply, err := h.agent.Handle(context.Background(), Turn{UserID: "u1", Text: "今天做了什么"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if reply.Mode != ModeRecall {
		t.Fatalf("模式应为 recall，实际 %s", reply.Mode)
	}
	if len(reply.ToolCalls) != 1 || reply.ToolCalls[0] != "get_today_status" {
		t.Fatalf("应记录工具调用，实际 %v", reply.ToolCalls)
	}
	if !strings.Contains(reply.Text, "ZCode") {
		t.Fatalf("回答应包含模型合成的文本，实际: %s", reply.Text)
	}
	if !strings.Contains(reply.Text, "证据") {
		t.Fatalf("有事实依据时应附证据范围，实际: %s", reply.Text)
	}
	// 来源必须是库里真实存在的时段 ID，而不是模型编的。
	if len(reply.SourceSessionIDs) != 1 || reply.SourceSessionIDs[0] != "s_test_session_0001" {
		t.Fatalf("来源应为真实时段 ID，实际 %v", reply.SourceSessionIDs)
	}
	// 工具返回的事实必须真的交给了合成阶段。
	if len(planner.synthReqs) != 1 || !strings.Contains(planner.synthReqs[0].UserPrompt, "ZCode") {
		t.Fatalf("合成阶段应拿到工具返回的事实，实际: %+v", planner.synthReqs)
	}
	// 审计：这一次调用必须留痕，且决策是放行。
	records := h.audit.All()
	if len(records) != 1 {
		t.Fatalf("应记录 1 条工具审计，实际 %d", len(records))
	}
	if records[0].Tool != "get_today_status" || records[0].Decision != tooling.DecisionAllowed {
		t.Fatalf("审计内容不符: %+v", records[0])
	}
	if records[0].Actor != "u1" {
		t.Fatalf("审计应记录请求者，实际 %q", records[0].Actor)
	}
	if records[0].Risk != tooling.RiskRead {
		t.Fatalf("审计应记录风险级别，实际 %q", records[0].Risk)
	}
}

// TestAgentIdentityComesFromConfiguredProfile 覆盖身份配置注入。
//
// 关键断言：换个 assistant_name 就能换身份，且提示词里不再出现写死的 Lumen。
func TestAgentIdentityComesFromConfiguredProfile(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")

	profile := identity.Profile{
		Name: "小灯", Role: "学习助手", OwnerDisplayName: "liang",
		Language: "zh-CN", Tone: "温和", Proactivity: "低",
	}
	planner := &fakePlanner{
		enabled: true,
		plan:    Plan{Mode: ModeChat, Understanding: Understanding{Goal: "自我介绍", Confidence: 0.9}},
		synth:   SynthResult{Answer: "我是小灯，你的学习助手。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, profile, planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "你是谁"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if !strings.Contains(reply.Text, "小灯") {
		t.Fatalf("回答应使用配置的名字，实际: %s", reply.Text)
	}
	if len(planner.planReqs) != 1 {
		t.Fatal("应调用过一次规划")
	}
	// 身份现在由装配器注入 trusted_context（用户侧提示词），系统提示只留规则。
	userPrompt := planner.planReqs[0].UserPrompt
	if !strings.Contains(userPrompt, "小灯") {
		t.Fatalf("规划提示词的可信块应注入配置的名字，实际: %s", userPrompt)
	}
	if !strings.Contains(userPrompt, "学习助手") {
		t.Fatalf("规划提示词的可信块应注入配置的定位，实际: %s", userPrompt)
	}
	if strings.Contains(userPrompt, "你是 Lumen") {
		t.Fatalf("提示词不应写死 Lumen，实际: %s", userPrompt)
	}
	if !strings.Contains(planner.planReqs[0].SystemPrompt, "get_assistant_profile") {
		t.Fatalf("规划提示词应包含工具目录，实际: %s", planner.planReqs[0].SystemPrompt)
	}
}

// TestAgentHonorsPlanRegardlessOfPhrasing 覆盖"同一意图多种说法，无需改 Go 代码"。
//
// 其中「嗯，那个呢」在旧的正则入口里会被判成 unsupported，
// 而现在它照样触发模型计划的检索——语义判断不在代码里。
func TestAgentHonorsPlanRegardlessOfPhrasing(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode:          ModeRecall,
			Understanding: Understanding{Goal: "了解今天的记录", TimeRange: "today", Confidence: 0.8},
			ToolCalls:     []tooling.Call{{Name: "get_today_status"}},
		},
		synth: SynthResult{Answer: "今天用了 ZCode 约 90 分钟。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, identity.Default(), planner)

	for _, text := range []string{
		"今天做了什么",
		"我今天都忙了些什么呀",
		"看看今天的记录",
		"嗯，那个呢",
	} {
		reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: text})
		if err != nil {
			t.Fatalf("%q 处理失败: %v", text, err)
		}
		if len(reply.ToolCalls) != 1 {
			t.Fatalf("%q 应执行模型计划的工具调用，实际 %v", text, reply.ToolCalls)
		}
		if len(reply.SourceSessionIDs) == 0 {
			t.Fatalf("%q 应带回真实来源", text)
		}
	}

	// 用户原话必须原样交给模型：代码不做任何关键词改写或预判。
	for i, req := range planner.planReqs {
		if !strings.Contains(req.UserPrompt, []string{
			"今天做了什么", "我今天都忙了些什么呀", "看看今天的记录", "嗯，那个呢",
		}[i]) {
			t.Fatalf("第 %d 轮的用户原话应原样传给模型，实际: %s", i+1, req.UserPrompt)
		}
	}
}

// TestAgentReportsDeniedToolsToSynthesizer 覆盖权限拦截的诚实传达。
//
// 被拒的工具不能让模型以为"查过了但没有数据"，必须显式告诉它这一步没做成。
func TestAgentReportsDeniedToolsToSynthesizer(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode:          ModeRecall,
			Understanding: Understanding{Goal: "查所有记录", TimeRange: "today", Confidence: 0.9},
			ToolCalls: []tooling.Call{
				{Name: "get_today_status"},
				{Name: "run_sql", Arguments: map[string]any{"query": "SELECT * FROM sessions"}},
			},
		},
		synth: SynthResult{Answer: "今天用了 ZCode 约 90 分钟。", SupportLevel: SupportSupported},
	}
	h := newHarness(t, store, identity.Default(), planner)

	reply, err := h.agent.Handle(context.Background(), Turn{UserID: "u1", Text: "把数据库里所有记录都给我"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if len(reply.DeniedTools) != 1 || reply.DeniedTools[0].Name != "run_sql" {
		t.Fatalf("应记录被拦截的 run_sql，实际 %+v", reply.DeniedTools)
	}
	if len(reply.ToolCalls) != 2 {
		t.Fatalf("审计里应记录模型请求过的全部工具，实际 %v", reply.ToolCalls)
	}

	synthPrompt := planner.synthReqs[0].UserPrompt
	if !strings.Contains(synthPrompt, "run_sql") {
		t.Fatalf("合成提示词应告知被拒步骤，实际: %s", synthPrompt)
	}
	if !strings.Contains(synthPrompt, "拒绝") {
		t.Fatalf("合成提示词应说明被拒绝，实际: %s", synthPrompt)
	}

	// 被拒的调用同样要留审计：这是"模型是否在尝试越权"的唯一线索。
	records := h.audit.All()
	var denied *tooling.AuditRecord
	for i := range records {
		if records[i].Tool == "run_sql" {
			denied = &records[i]
		}
	}
	if denied == nil {
		t.Fatalf("被拒的调用必须留审计，实际记录: %+v", records)
	}
	if denied.Decision != tooling.DecisionDenied || denied.Reason == "" {
		t.Fatalf("审计应记录拒绝与原因，实际: %+v", denied)
	}
}

// TestAgentFallsBackWhenModelUnavailable 覆盖模型不可用时的诚实兜底。
func TestAgentFallsBackWhenModelUnavailable(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	profile := identity.Profile{Name: "小灯", Role: "学习助手"}

	planner := &fakePlanner{enabled: false}
	agent := newTestAgent(t, store, profile, planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "你好"})
	if err != nil {
		t.Fatalf("模型不可用不应返回错误: %v", err)
	}
	if reply.Status != "fallback_no_model" {
		t.Fatalf("状态应为 fallback_no_model，实际 %s", reply.Status)
	}
	if !strings.Contains(reply.Text, "小灯") {
		t.Fatalf("兜底文案也应使用配置的名字，实际: %s", reply.Text)
	}
	if reply.UsedAI {
		t.Fatal("兜底不应标记为使用了 AI")
	}
	if len(planner.planReqs) != 0 {
		t.Fatal("模型不可用时不应发起调用")
	}
	if len(reply.SourceSessionIDs) != 0 {
		t.Fatal("兜底不应凭空捏造来源")
	}
}

// TestAgentFallsBackWhenPlanInvalid 覆盖计划非法时的安全降级。
//
// 格式不对的计划说明模型这轮不可靠，猜它的意图比拒绝更危险。
func TestAgentFallsBackWhenPlanInvalid(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{
		enabled: true,
		planErr: errFake("模型输出不是合法 JSON"),
	}
	agent := newTestAgent(t, store, identity.Default(), planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "今天做了什么"})
	if err != nil {
		t.Fatalf("计划非法不应返回错误: %v", err)
	}
	if reply.Status != "fallback_no_model" {
		t.Fatalf("应走兜底路径，实际状态 %s", reply.Status)
	}
	if len(planner.synthReqs) != 0 {
		t.Fatal("计划非法时不应继续合成")
	}
}

// TestAgentFallsBackWhenSynthesisFails 覆盖合成失败时改用原始事实。
//
// 这里不能编造内容：只把已经取到的结构化事实原样陈述，并说明这轮没能组织好。
func TestAgentFallsBackWhenSynthesisFails(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode:          ModeRecall,
			Understanding: Understanding{Goal: "今天的记录", TimeRange: "today", Confidence: 0.9},
			ToolCalls:     []tooling.Call{{Name: "get_today_status"}},
		},
		synthErr: errFake("模型超时"),
	}
	agent := newTestAgent(t, store, identity.Default(), planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "今天做了什么"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if reply.Status != "synth_failed" {
		t.Fatalf("应标记合成失败，实际 %s", reply.Status)
	}
	if !strings.Contains(reply.Text, "ZCode") {
		t.Fatalf("降级应陈述已取到的事实，实际: %s", reply.Text)
	}
	if len(reply.SourceSessionIDs) != 1 {
		t.Fatalf("降级仍应带回真实来源，实际 %v", reply.SourceSessionIDs)
	}
}

// TestAgentRejectsOverclaimingAnswer 覆盖回答越界时的拦截。
func TestAgentRejectsOverclaimingAnswer(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode:          ModeRecall,
			Understanding: Understanding{Goal: "今天的记录", TimeRange: "today", Confidence: 0.9},
			ToolCalls:     []tooling.Call{{Name: "get_today_status"}},
		},
		// 模型承诺了不存在的能力，必须被拦截。
		synth: SynthResult{Answer: "我无所不能，还能帮你写周报。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, identity.Default(), planner)

	reply, _ := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "今天做了什么"})
	if reply.Status != "answer_invalid" {
		t.Fatalf("越界回答应被拦截，实际状态 %s", reply.Status)
	}
	if strings.Contains(reply.Text, "无所不能") {
		t.Fatalf("越界措辞不应发给用户，实际: %s", reply.Text)
	}
	if !strings.Contains(reply.Text, "ZCode") {
		t.Fatalf("降级应陈述真实事实，实际: %s", reply.Text)
	}
}

// TestAgentSavesLimitedConversationState 覆盖有限对话状态只保存必要的指代信息。
func TestAgentSavesLimitedConversationState(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "用户想知道 lumen 项目的进展", Entities: []string{"lumen"},
				TimeRange: "last_7_days", Confidence: 0.9,
			},
			ToolCalls: []tooling.Call{{Name: "get_sessions", Arguments: map[string]any{"project": "lumen"}}},
		},
		synth: SynthResult{Answer: "lumen 最近有 1 段记录。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, identity.Default(), planner)

	if _, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "lumen 项目最近怎么样"}); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	state, err := store.ConversationStateByUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("读取对话状态失败: %v", err)
	}
	if state.CurrentProject != "lumen" {
		t.Fatalf("应记住当前项目 lumen，实际 %q", state.CurrentProject)
	}
	if state.LastTimeRange != "last_7_days" {
		t.Fatalf("应记住上一轮时间范围，实际 %q", state.LastTimeRange)
	}
	// 状态里不该出现用户原话：完整聊天历史不进模型上下文。
	if strings.Contains(state.PendingQuestion+state.CurrentProject+state.LastMode+state.LastTimeRange, "怎么样") {
		t.Fatalf("对话状态不应保存用户原话，实际 %+v", state)
	}
}

// TestAgentClarifySkipsSecondModelCall 覆盖澄清路径不浪费第二次调用。
func TestAgentClarifySkipsSecondModelCall(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode:                  ModeClarify,
			Understanding:         Understanding{Goal: "指代不清", Confidence: 0.3},
			NeedsClarification:    true,
			ClarificationQuestion: "你指的是哪个项目？",
		},
	}
	agent := newTestAgent(t, store, identity.Default(), planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "那个项目怎么样了"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if reply.Mode != ModeClarify {
		t.Fatalf("模式应为 clarify，实际 %s", reply.Mode)
	}
	if reply.Text != "你指的是哪个项目？" {
		t.Fatalf("应直接返回澄清问题，实际: %s", reply.Text)
	}
	if len(planner.synthReqs) != 0 {
		t.Fatal("澄清阶段没有事实可依据，不应再调一次模型")
	}

	// 待澄清问题要存下来，下一轮用户的话可能是对它的回答。
	state, _ := store.ConversationStateByUser(context.Background(), "u1")
	if state.PendingQuestion != "你指的是哪个项目？" {
		t.Fatalf("应保存待澄清问题，实际 %q", state.PendingQuestion)
	}
}

// TestAgentEmptyInputDoesNotCallModel 覆盖空输入不消耗模型调用。
func TestAgentEmptyInputDoesNotCallModel(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{enabled: true}
	agent := newTestAgent(t, store, identity.Default(), planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "   "})
	if err != nil {
		t.Fatalf("空输入不应报错: %v", err)
	}
	if reply.Status != "empty_input" {
		t.Fatalf("状态应为 empty_input，实际 %s", reply.Status)
	}
	if len(planner.planReqs) != 0 {
		t.Fatal("空输入不应调用模型")
	}
}

// TestAgentWithMissingExecutorStillAnswers 覆盖执行器缺失时整条链路仍能作答。
//
// 端到端行为：不 panic、不执行任何工具、如实说明这一步没做成。
func TestAgentWithMissingExecutorStillAnswers(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "查今天的记录", TimeRange: "today", Confidence: 0.9,
			},
			ToolCalls: []tooling.Call{{Name: "get_today_status"}},
		},
		synth: SynthResult{Answer: "没查到记录。", SupportLevel: SupportInsufficient},
	}
	agent := NewAgent(Options{
		Store:    store,
		Planner:  planner,
		Executor: nil, // 装配漏了执行器
		Profile:  identity.Default(),
		Location: testLoc,
	})

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "今天做了什么"})
	if err != nil {
		t.Fatalf("执行器缺失不应返回错误: %v", err)
	}
	if len(reply.DeniedTools) == 0 {
		t.Fatal("应把被拒绝的调用如实告知用户与审计")
	}
	if len(reply.SourceSessionIDs) != 0 {
		t.Fatal("没有执行任何工具时不应凭空产生来源")
	}
	// 必须告诉模型这一步没做成，否则它会以为"查过了但没有数据"。
	synthPrompt := planner.synthReqs[0].UserPrompt
	if !strings.Contains(synthPrompt, "执行器未初始化") {
		t.Fatalf("合成提示词应说明被拒原因，实际: %s", synthPrompt)
	}
}

// TestAgentToolCatalogComesFromRegistry 覆盖工具目录确实来自注册表。
//
// 目录是"模型能自己发现新工具"的机制：注册表里有的工具必须出现在提示词里，
// 注册表里没有的不可能出现。
func TestAgentToolCatalogComesFromRegistry(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan:    Plan{Mode: ModeChat, Understanding: Understanding{Confidence: 0.9}},
		synth:   SynthResult{Answer: "好。", SupportLevel: SupportSupported},
	}
	h := newHarness(t, store, identity.Default(), planner)

	if _, err := h.agent.Handle(context.Background(), Turn{UserID: "u1", Text: "你好"}); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	system := planner.planReqs[0].SystemPrompt
	for _, name := range h.registry.Names() {
		if !strings.Contains(system, name) {
			t.Fatalf("工具目录应包含 %s，实际: %s", name, system)
		}
	}
	if strings.Contains(system, "run_sql") {
		t.Fatal("目录里不应出现未注册的工具名")
	}
}
