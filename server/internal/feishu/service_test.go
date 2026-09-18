package feishu

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/assistant"
	"lumen/server/internal/config"
	"lumen/server/internal/storage"
)

var testLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// fakePlanner 是测试用的假模型：只决定"这一轮说什么"，不产生措辞质量断言。
type fakePlanner struct {
	enabled  bool
	plan     assistant.Plan
	synthFn  func(req assistant.SynthesizeRequest) assistant.SynthResult
	planReqs []assistant.PlanRequest
}

func (f *fakePlanner) Enabled() bool { return f.enabled }

func (f *fakePlanner) Plan(_ context.Context, req assistant.PlanRequest) (assistant.Plan, ai.Response, error) {
	f.planReqs = append(f.planReqs, req)
	if !f.enabled {
		return assistant.Plan{}, ai.Response{}, &ai.APIError{Code: "ai_disabled"}
	}
	return f.plan, ai.Response{Usage: ai.Usage{TotalTokens: 15}}, nil
}

func (f *fakePlanner) Synthesize(_ context.Context, req assistant.SynthesizeRequest) (assistant.SynthResult, ai.Response, error) {
	if f.synthFn == nil {
		return assistant.SynthResult{Answer: "默认回答", SupportLevel: assistant.SupportSupported}, ai.Response{}, nil
	}
	return f.synthFn(req), ai.Response{}, nil
}

// newQAStore 创建带一条工作时段的测试库。
func newQAStore(t *testing.T, date, project string) *storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "qa.db"))
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

func newTestService(t *testing.T, store *storage.Store, planner assistant.Planner,
	profile assistant.Profile) *QAService {
	t.Helper()
	svc := NewQAService(QAServiceOptions{
		Store: store, Planner: planner, Loc: testLoc, Profile: profile, QueryDailyLimit: 20,
	})
	if err := svc.Validate(); err != nil {
		t.Fatalf("问答服务构造失败: %v", err)
	}
	return svc
}

// extractFactValue 从合成阶段的用户提示里取出能力返回的事实字段。
//
// 用它做断言而不是比对固定文案：验证的是"事实确实流到了模型手上"，
// 而不是模型最终怎么措辞。
func nameFromPrompt(req assistant.SynthesizeRequest) string {
	const marker = `"name":"`
	i := strings.Index(req.UserPrompt, marker)
	if i < 0 {
		return ""
	}
	rest := req.UserPrompt[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestIdentityComesFromConfigNotCode 覆盖身份端到端由配置决定。
//
// 断言链条：改 LUMEN_ASSISTANT_NAME → Config.Profile() → 能力目录 →
// 模型提示词 → 回答。任何一处写死 Lumen 都会让这个测试失败。
func TestIdentityComesFromConfigNotCode(t *testing.T) {
	store := newQAStore(t, "2026-09-17", "lumen")

	for _, name := range []string{"Lumen", "小灯", "Aria"} {
		cfg := &config.Config{
			AssistantName: name, AssistantRole: "个人助手/伙伴",
			AssistantLanguage: "zh-CN", AssistantTone: "友好、简洁",
			AssistantProactive: "低", SummaryHour: 22, SummaryMinute: 30,
		}
		profile := cfg.Profile()

		planner := &fakePlanner{
			enabled: true,
			plan: assistant.Plan{
				Mode:          assistant.ModeChat,
				Understanding: assistant.Understanding{Goal: "问助手是谁", Confidence: 0.9},
				// 身份必须来自 get_assistant_profile，而不是模型凭提示词编造。
				ToolCalls: []assistant.ToolCall{{Name: "get_assistant_profile"}},
			},
			synthFn: func(req assistant.SynthesizeRequest) assistant.SynthResult {
				got := nameFromPrompt(req)
				if got == "" {
					got = "（没拿到身份）"
				}
				return assistant.SynthResult{
					Answer: "我是" + got + "，你的个人助手。", SupportLevel: assistant.SupportSupported,
				}
			},
		}
		svc := newTestService(t, store, planner, profile)

		reply, err := svc.Handle(context.Background(), "u1", "你叫什么")
		if err != nil {
			t.Fatalf("%s: 处理失败: %v", name, err)
		}
		if !strings.Contains(reply.Text, name) {
			t.Fatalf("改配置后回答应体现名字 %q，实际: %s", name, reply.Text)
		}
		if name != "Lumen" && strings.Contains(reply.Text, "Lumen") {
			t.Fatalf("换了名字后回答里不应再出现 Lumen，实际: %s", reply.Text)
		}
		if len(planner.planReqs) != 1 {
			t.Fatalf("%s: 应调用一次规划", name)
		}
		if !strings.Contains(planner.planReqs[0].SystemPrompt, name) {
			t.Fatalf("%s: 规划提示词应含配置的名字", name)
		}
	}
}

// TestHandlePassesRawTextToModel 覆盖默认入口把原话交给模型。
//
// 这些说法在旧的正则入口里分别落到 greeting / unsupported，
// 现在它们都必须走到模型计划——语义判断不在 Go 代码里。
func TestHandlePassesRawTextToModel(t *testing.T) {
	store := newQAStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan: assistant.Plan{
			Mode:          assistant.ModeChat,
			Understanding: assistant.Understanding{Goal: "打招呼", Confidence: 0.9},
		},
		synthFn: func(assistant.SynthesizeRequest) assistant.SynthResult {
			return assistant.SynthResult{Answer: "嗨，我在。", SupportLevel: assistant.SupportSupported}
		},
	}
	svc := newTestService(t, store, planner, assistant.DefaultProfile())

	texts := []string{"你好", "在吗", "早上好呀", "帮我写一份周报", "嗯……那个呢"}
	for _, text := range texts {
		if _, err := svc.Handle(context.Background(), "u1", text); err != nil {
			t.Fatalf("%q 处理失败: %v", text, err)
		}
	}
	if len(planner.planReqs) != len(texts) {
		t.Fatalf("每条消息都应触发一次规划，实际 %d 次", len(planner.planReqs))
	}
	for i, req := range planner.planReqs {
		if !strings.Contains(req.UserPrompt, texts[i]) {
			t.Fatalf("第 %d 条消息应原样传给模型，实际提示词: %s", i+1, req.UserPrompt)
		}
	}
}

// TestRecallPathReturnsRealSources 覆盖检索链路的来源校验。
func TestRecallPathReturnsRealSources(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newQAStore(t, today, "lumen")

	planner := &fakePlanner{
		enabled: true,
		plan: assistant.Plan{
			Mode:          assistant.ModeRecall,
			Understanding: assistant.Understanding{Goal: "今天的记录", TimeRange: "today", Confidence: 0.9},
			ToolCalls:     []assistant.ToolCall{{Name: "get_today_status"}},
		},
		synthFn: func(assistant.SynthesizeRequest) assistant.SynthResult {
			return assistant.SynthResult{Answer: "今天用 ZCode 约 90 分钟。", SupportLevel: assistant.SupportSupported}
		},
	}
	svc := newTestService(t, store, planner, assistant.DefaultProfile())

	reply, err := svc.Handle(context.Background(), "u1", "今天做了什么")
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if reply.Mode != assistant.ModeRecall {
		t.Fatalf("模式应为 recall，实际 %s", reply.Mode)
	}
	if len(reply.SourceSessionIDs) != 1 {
		t.Fatalf("应带回真实来源，实际 %v", reply.SourceSessionIDs)
	}
	if !strings.Contains(reply.Text, "证据") {
		t.Fatalf("有事实依据的回答应附证据，实际: %s", reply.Text)
	}
}

// TestFallbackKeepsDataQueryAnswerable 覆盖模型不可用时的数据兜底。
//
// 模型不可用时如果连"今天做了什么"都答不了，用户会以为记录丢了。
func TestFallbackKeepsDataQueryAnswerable(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newQAStore(t, today, "lumen")
	planner := &fakePlanner{enabled: false}
	svc := newTestService(t, store, planner, assistant.DefaultProfile())

	reply, err := svc.Handle(context.Background(), "u1", "今天做了什么")
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if reply.Status != "fallback_no_model_with_facts" {
		t.Fatalf("应走确定性事实兜底，实际状态 %s", reply.Status)
	}
	if !strings.Contains(reply.Text, "ZCode") {
		t.Fatalf("兜底应给出真实事实，实际: %s", reply.Text)
	}
}

// TestFallbackIsHonestForOtherMessages 覆盖兜底不冒充能力。
//
// 非数据请求在模型不可用时必须诚实说明，而不是退回另一套关键词机器人。
func TestFallbackIsHonestForOtherMessages(t *testing.T) {
	store := newQAStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{enabled: false}
	profile := assistant.Profile{Name: "小灯", Role: "学习助手"}
	svc := newTestService(t, store, planner, profile)

	reply, err := svc.Handle(context.Background(), "u1", "你好")
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if reply.Status != "fallback_no_model" {
		t.Fatalf("应标记模型不可用，实际状态 %s", reply.Status)
	}
	if !strings.Contains(reply.Text, "小灯") {
		t.Fatalf("兜底文案应使用配置的名字，实际: %s", reply.Text)
	}
	if strings.Contains(reply.Text, "今天做了什么") {
		t.Fatalf("兜底不应再倾倒功能菜单，实际: %s", reply.Text)
	}
}

// TestRecordConversationKeepsModeAndSources 覆盖问答记录只落必要字段。
func TestRecordConversationKeepsModeAndSources(t *testing.T) {
	store := newQAStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{enabled: false}
	svc := newTestService(t, store, planner, assistant.DefaultProfile())

	ctx := context.Background()
	reply, err := svc.Handle(ctx, "u1", "今天做了什么")
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if err := svc.RecordConversation(ctx, "msg-1", "u1", "今天做了什么", reply); err != nil {
		t.Fatalf("写入问答记录失败: %v", err)
	}

	handled, err := svc.AlreadyHandled(ctx, "msg-1")
	if err != nil {
		t.Fatalf("查询幂等失败: %v", err)
	}
	if !handled {
		t.Fatal("写入后应判定为已处理")
	}
	// 没写过的消息必须判定为未处理，否则会漏掉用户的消息。
	other, _ := svc.AlreadyHandled(ctx, "msg-2")
	if other {
		t.Fatal("未写入的消息不应判定为已处理")
	}
}

// TestBudgetExhaustionSkipsModel 覆盖预算用尽时不调用模型。
func TestBudgetExhaustionSkipsModel(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newQAStore(t, today, "lumen")
	ctx := context.Background()

	if err := store.AddAIUsage(ctx, today, aiUsageKindQuery, 10, 10); err != nil {
		t.Fatalf("写入用量失败: %v", err)
	}

	planner := &fakePlanner{
		enabled: true,
		plan: assistant.Plan{
			Mode: assistant.ModeChat, Understanding: assistant.Understanding{Confidence: 0.9},
		},
	}
	svc := NewQAService(QAServiceOptions{
		Store: store, Planner: planner, Loc: testLoc,
		Profile: assistant.DefaultProfile(), QueryDailyLimit: 1,
	})

	if _, err := svc.Handle(ctx, "u1", "你好"); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if len(planner.planReqs) != 0 {
		t.Fatalf("预算用尽时不应调用模型，实际 %d 次", len(planner.planReqs))
	}
}

// TestProjectDisplayName 覆盖内部标记不外泄。
func TestProjectDisplayName(t *testing.T) {
	cases := map[string]string{
		"unclassified": "暂未识别项目",
		"":             "暂未识别项目",
		"lumen":        "lumen",
	}
	for in, want := range cases {
		if got := ProjectDisplayName(in); got != want {
			t.Fatalf("ProjectDisplayName(%q) 应为 %q，实际 %q", in, want, got)
		}
	}
}
