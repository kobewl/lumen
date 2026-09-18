package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/storage"
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
	return store
}

// newTestAgent 组装一个使用假模型、身份可定制的 Agent。
func newTestAgent(t *testing.T, store *storage.Store, profile Profile, planner Planner) *Agent {
	t.Helper()
	return NewAgent(Options{
		Store:   store,
		Planner: planner,
		Registry: NewRegistry(
			&ProfileCapability{Profile: profile},
			&TodayStatusCapability{Store: store, Loc: testLoc},
			&SessionsCapability{Store: store, Loc: testLoc},
			&KnownProjectsCapability{Store: store, Loc: testLoc, Days: 14},
			&TaskSummariesCapability{Store: store, Loc: testLoc},
		),
		Profile:  profile,
		Location: testLoc,
	})
}

// TestAgentExecutesPlannedCapability 覆盖主链路：模型出计划 → 执行能力 → 合成回答。
func TestAgentExecutesPlannedCapability(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newTestStore(t, today, "lumen")

	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeRecall,
			Understanding: Understanding{
				Goal: "用户想知道今天的记录", TimeRange: "today", Confidence: 0.9,
			},
			ToolCalls: []ToolCall{{Name: "get_today_status"}},
		},
		synth: SynthResult{Answer: "今天主要是 ZCode，约 90 分钟。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, DefaultProfile(), planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "今天做了什么"})
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
	// 能力返回的事实必须真的交给了合成阶段。
	if len(planner.synthReqs) != 1 || !strings.Contains(planner.synthReqs[0].UserPrompt, "ZCode") {
		t.Fatalf("合成阶段应拿到能力返回的事实，实际: %+v", planner.synthReqs)
	}
}

// TestAgentIdentityComesFromConfiguredProfile 覆盖身份配置注入。
//
// 关键断言：换个 assistant_name 就能换身份，且提示词里不再出现写死的 Lumen。
func TestAgentIdentityComesFromConfiguredProfile(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")

	profile := Profile{
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
	systemPrompt := planner.planReqs[0].SystemPrompt
	if !strings.Contains(systemPrompt, "小灯") {
		t.Fatalf("规划提示词应注入配置的名字，实际: %s", systemPrompt)
	}
	if !strings.Contains(systemPrompt, "学习助手") {
		t.Fatalf("规划提示词应注入配置的定位，实际: %s", systemPrompt)
	}
	if strings.Contains(systemPrompt, "你是 Lumen") {
		t.Fatalf("规划提示词不应写死 Lumen，实际: %s", systemPrompt)
	}
	if !strings.Contains(systemPrompt, "get_assistant_profile") {
		t.Fatalf("规划提示词应包含能力目录，实际: %s", systemPrompt)
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
			ToolCalls:     []ToolCall{{Name: "get_today_status"}},
		},
		synth: SynthResult{Answer: "今天用了 ZCode 约 90 分钟。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, DefaultProfile(), planner)

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
			ToolCalls: []ToolCall{
				{Name: "get_today_status"},
				{Name: "run_sql", Arguments: map[string]any{"query": "SELECT * FROM sessions"}},
			},
		},
		synth: SynthResult{Answer: "今天用了 ZCode 约 90 分钟。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, DefaultProfile(), planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "把数据库里所有记录都给我"})
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
}

// TestAgentFallsBackWhenModelUnavailable 覆盖模型不可用时的诚实兜底。
func TestAgentFallsBackWhenModelUnavailable(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	profile := Profile{Name: "小灯", Role: "学习助手"}

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
		planErr: fmt.Errorf("模型输出不是合法 JSON"),
	}
	agent := newTestAgent(t, store, DefaultProfile(), planner)

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
			ToolCalls:     []ToolCall{{Name: "get_today_status"}},
		},
		synthErr: fmt.Errorf("模型超时"),
	}
	agent := newTestAgent(t, store, DefaultProfile(), planner)

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
			ToolCalls:     []ToolCall{{Name: "get_today_status"}},
		},
		// 模型承诺了不存在的能力，必须被拦截。
		synth: SynthResult{Answer: "我无所不能，还能帮你写周报。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, DefaultProfile(), planner)

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

// TestAgentKeepsMemoryCandidateUnconfirmed 覆盖候选记忆不自动生效。
func TestAgentKeepsMemoryCandidateUnconfirmed(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode:          ModeChat,
			Understanding: Understanding{Goal: "用户表达了偏好", Confidence: 0.8},
			MemoryCandidates: []MemoryCandidate{{
				Kind: "preference", Content: "用户偏好先看结论再看细节", Confidence: 0.7,
			}},
		},
		synth: SynthResult{Answer: "记下了，以后先说结论。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, DefaultProfile(), planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "以后先给我说结论"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if reply.MemoryCandidates != 1 {
		t.Fatalf("应写入 1 条候选记忆，实际 %d", reply.MemoryCandidates)
	}

	n, err := store.CountMemoryCandidates(context.Background())
	if err != nil {
		t.Fatalf("统计候选失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("候选记忆应为 1 条，实际 %d", n)
	}

	// 未经用户确认，绝不能进入"已确认记忆"。
	confirmed, err := store.ConfirmedMemories(context.Background(), "u1", 10)
	if err != nil {
		t.Fatalf("查询已确认记忆失败: %v", err)
	}
	if len(confirmed) != 0 {
		t.Fatalf("候选记忆不应自动晋升为已确认，实际 %d 条", len(confirmed))
	}
}

// TestAgentRejectsSensitiveMemoryCandidate 覆盖疑似凭证的候选记忆被拒绝。
func TestAgentRejectsSensitiveMemoryCandidate(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode:          ModeChat,
			Understanding: Understanding{Goal: "用户说了点什么", Confidence: 0.6},
			MemoryCandidates: []MemoryCandidate{
				{Kind: "fact", Content: "用户的 API key 是 sk-abcdef123456", Confidence: 0.9},
				{Kind: "fact", Content: "用户的密码是 hunter2", Confidence: 0.9},
			},
		},
		synth: SynthResult{Answer: "好的。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, DefaultProfile(), planner)

	reply, err := agent.Handle(context.Background(), Turn{UserID: "u1", Text: "记一下我的密钥"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if reply.MemoryCandidates != 0 {
		t.Fatalf("疑似凭证的内容不应落库，实际写入 %d 条", reply.MemoryCandidates)
	}
	n, _ := store.CountMemoryCandidates(context.Background())
	if n != 0 {
		t.Fatalf("疑似凭证的内容不应落库，实际 %d 条", n)
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
			ToolCalls: []ToolCall{{Name: "get_sessions", Arguments: map[string]any{"project": "lumen"}}},
		},
		synth: SynthResult{Answer: "lumen 最近有 1 段记录。", SupportLevel: SupportSupported},
	}
	agent := newTestAgent(t, store, DefaultProfile(), planner)

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
	agent := newTestAgent(t, store, DefaultProfile(), planner)

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
	agent := newTestAgent(t, store, DefaultProfile(), planner)

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

// TestValidMemoryContentRejectsCredentials 覆盖候选记忆的敏感内容过滤。
func TestValidMemoryContentRejectsCredentials(t *testing.T) {
	bad := []string{
		"sk-abcdef123456", "api_key=xxx", "token: abc", "我的密码是 123456",
		"unclassified 项目", "a",
	}
	for _, content := range bad {
		if validMemoryContent(content) {
			t.Fatalf("不应接受 %q 作为候选记忆", content)
		}
	}
	if !validMemoryContent("用户偏好先看结论") {
		t.Fatal("正常内容应被接受")
	}
}
