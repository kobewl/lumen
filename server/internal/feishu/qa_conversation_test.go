package feishu

import (
	"context"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/storage"
)

func newQAService(t *testing.T, date, project string) *QAService {
	t.Helper()
	store := newQAStore(t, date, project)
	client := ai.NewClient("https://api.deepseek.com", "", "", time.Second)
	return NewQAService(store, client, testLoc, 20, 22, 30)
}

// TestSmallTalkIntentsAreDistinct 验证问候、身份、能力、下班四类消息各自得到
// 不同的确定性回复，而不是同一份功能菜单。
func TestSmallTalkIntentsAreDistinct(t *testing.T) {
	svc := newQAService(t, "2026-09-17", "lumen")
	cases := []struct {
		text   string
		intent string
	}{
		{"你好", IntentGreeting},
		{"在吗", IntentGreeting},
		{"你叫什么", IntentIdentity},
		{"你是谁", IntentIdentity},
		{"你能做什么", IntentCapability},
		{"你会什么", IntentCapability},
		{"我下班了", IntentSignoff},
		{"下班了", IntentSignoff},
	}
	for _, c := range cases {
		got := ParseQuery(c.text)
		if got.Intent != c.intent {
			t.Fatalf("%q 意图应为 %s，实际 %s", c.text, c.intent, got.Intent)
		}
	}

	// 四类意图的回复必须彼此不同（不能都返回同一份三项菜单）。
	representative := map[string]string{}
	texts := map[string]string{}
	for _, c := range cases {
		answer, err := svc.Handle(context.Background(), ParseQuery(c.text))
		if err != nil {
			t.Fatalf("处理 %q 失败: %v", c.text, err)
		}
		if answer.Text == "" {
			t.Fatalf("%q 应有回复", c.text)
		}
		if len(answer.SourceSessionIDs) != 0 {
			t.Fatalf("%q 是闲聊，不应检索 Session", c.text)
		}
		texts[c.text] = answer.Text
		if prev, ok := representative[c.intent]; ok {
			if prev != answer.Text {
				t.Fatalf("同类意图 %s 应有一致回复:\n%q => %s\n%q => %s",
					c.intent, prev, prev, c.text, answer.Text)
			}
			continue
		}
		representative[c.intent] = answer.Text
	}

	byText := map[string]string{}
	for intent, text := range representative {
		if prev, ok := byText[text]; ok {
			t.Fatalf("意图 %s 与 %s 的回复完全相同，仍是同一份菜单:\n%s", intent, prev, text)
		}
		byText[text] = intent
	}
	if len(byText) != 4 {
		t.Fatalf("问候/身份/能力/下班应有 4 种不同回复，实际 %d 种", len(byText))
	}
	_ = texts
}

// TestGreetingIsNotTheFunctionMenu 验证问候不会一次性倾倒功能菜单。
func TestGreetingIsNotTheFunctionMenu(t *testing.T) {
	svc := newQAService(t, "2026-09-17", "lumen")
	answer, _ := svc.Handle(context.Background(), ParseQuery("你好"))
	if strings.Contains(answer.Text, "目前支持三类查询") {
		t.Fatalf("问候不应直接返回功能菜单，实际: %s", answer.Text)
	}
	if !strings.Contains(answer.Text, "Lumen") {
		t.Fatalf("问候应自报身份，实际: %s", answer.Text)
	}
}

// TestCapabilityMentionsBoundaries 验证能力介绍说明了 V0.1 的边界（只能看应用与时长），
// 而不是承诺"帮你写周报"这类未实现的能力。
func TestCapabilityMentionsBoundaries(t *testing.T) {
	svc := newQAService(t, "2026-09-17", "lumen")
	answer, _ := svc.Handle(context.Background(), ParseQuery("你能做什么"))
	text := answer.Text
	for _, want := range []string{"今天", "昨天", "项目"} {
		if !strings.Contains(text, want) {
			t.Fatalf("能力说明应包含 %q，实际: %s", want, text)
		}
	}
	if !strings.Contains(text, "应用") {
		t.Fatalf("能力说明应说明只记录应用与时长，实际: %s", text)
	}
	if strings.Contains(text, "周报") {
		t.Fatalf("不应承诺未实现的能力，实际: %s", text)
	}
}

// TestSignoffConfirmsAndStatesSummaryTime 验证「我下班了」是结束当天工作的明确语义。
func TestSignoffConfirmsAndStatesSummaryTime(t *testing.T) {
	svc := newQAService(t, "2026-09-17", "lumen")
	answer, err := svc.Handle(context.Background(), ParseQuery("我下班了"))
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if answer.Intent != IntentSignoff {
		t.Fatalf("意图应为 %s，实际 %s", IntentSignoff, answer.Intent)
	}
	if !strings.Contains(answer.Text, "22:30") {
		t.Fatalf("应告知总结时间，实际: %s", answer.Text)
	}
	if !strings.Contains(answer.Text, "今天") {
		t.Fatalf("应确认今天的记录到此结束，实际: %s", answer.Text)
	}
}

// TestSignoffIsIdempotent 验证重复说「我下班了」不会产生任何副作用或重复推送。
//
// 实现上不即时生成总结（那会在 22:30 之前生成残缺版本，之后又推一次），
// 因此这里断言：回复稳定、不检索 Session、不写会话记录之外的任何状态。
func TestSignoffIsIdempotent(t *testing.T) {
	svc := newQAService(t, "2026-09-17", "lumen")
	first, _ := svc.Handle(context.Background(), ParseQuery("我下班了"))
	second, _ := svc.Handle(context.Background(), ParseQuery("我下班了"))
	if first.Text != second.Text {
		t.Fatalf("重复说下班应得到同样的确认，\n第一次: %s\n第二次: %s", first.Text, second.Text)
	}
	if len(first.SourceSessionIDs) != 0 {
		t.Fatal("下班确认不应检索 Session")
	}
}

// TestUnsupportedIntentStillGivesHelp 验证无法识别的消息仍给出功能说明。
func TestUnsupportedIntentStillGivesHelp(t *testing.T) {
	svc := newQAService(t, "2026-09-17", "lumen")
	answer, _ := svc.Handle(context.Background(), ParseQuery("帮我写一份周报"))
	if answer.Status != "help" {
		t.Fatalf("不支持的意图应返回帮助文本，实际 %s", answer.Status)
	}
	if !strings.Contains(answer.Text, "今天做了什么") {
		t.Fatalf("帮助文本应说明支持的查询，实际: %s", answer.Text)
	}
}

// TestUserFacingTextHasNoEngineeringTerms 是文案回归测试。
//
// 用户可见文案里绝不能出现 unclassified、内部 Session ID 或「唯一 session」
// 这类工程术语。注意：commit message 是用户自己的数据，会原样展示，
// 不在本测试的约束范围内（找不到证据时反而应该如实展示）。
func TestUserFacingTextHasNoEngineeringTerms(t *testing.T) {
	ctx := context.Background()

	// 1) 闲聊类回复。
	svc := newQAService(t, "", "")
	for _, text := range []string{"你好", "你叫什么", "你能做什么", "我下班了", "帮我写周报"} {
		answer, _ := svc.Handle(ctx, ParseQuery(text))
		assertNoEngineeringTerms(t, answer.Text)
	}

	// 2) 未识别项目的会话：必须显示为「暂未识别项目」。
	unknown := newQAService(t, time.Now().In(testLoc).Format("2006-01-02"), UnclassifiedProject)
	answer, _ := unknown.Handle(ctx, ParseQuery("今天做了什么"))
	assertNoEngineeringTerms(t, answer.Text)
	if !strings.Contains(answer.Text, "暂未识别项目") {
		t.Fatalf("未识别项目应显示为可读文案，实际: %s", answer.Text)
	}

	// 3) 项目问答里也不能泄露内部标记。
	projectAnswer, _ := unknown.Handle(ctx, ParseQuery("项目 暂未识别项目 最近做了什么"))
	assertNoEngineeringTerms(t, projectAnswer.Text)
}

// assertNoEngineeringTerms 检查文案中不含内部术语与内部 ID。
func assertNoEngineeringTerms(t *testing.T, text string) {
	t.Helper()
	for _, bad := range []string{"unclassified", "Session", "session", "唯一 session", "s_"} {
		if strings.Contains(text, bad) {
			t.Fatalf("用户可见文案不应出现工程术语 %q，实际: %s", bad, text)
		}
	}
}

// TestEvidenceUsesTimeRangeNotSessionIDs 验证证据行只保留时间范围。
func TestEvidenceUsesTimeRangeNotSessionIDs(t *testing.T) {
	start := time.Date(2026, 9, 17, 9, 0, 0, 0, testLoc)
	sessions := []storage.Session{
		{ID: "s_9f3c1d8b7a6e5f4012345678", Project: "lumen", StartAt: start, EndAt: start.Add(time.Hour),
			StatsJSON: `{"duration_minutes":60}`, AppsJSON: `[{"app":"ZCode","duration_minutes":60}]`},
	}
	line := evidenceLine(sessions, testLoc)
	if strings.Contains(line, "s_9f3c1d8b") {
		t.Fatalf("证据行不应包含内部 ID，实际: %s", line)
	}
	if !strings.Contains(line, "09:00") || !strings.Contains(line, "10:00") {
		t.Fatalf("证据行应包含时间范围，实际: %s", line)
	}
}

// TestProjectAnswerRejectsEngineeringTermAsQuery 验证用户用工程术语查询时不会被当成项目名。
func TestProjectAnswerRejectsEngineeringTermAsQuery(t *testing.T) {
	svc := newQAService(t, time.Now().In(testLoc).Format("2006-01-02"), UnclassifiedProject)

	// 内部术语不该被解析成项目名，而应走「无法识别」的帮助路径。
	if got := ParseQuery("项目 unclassified 最近做了什么"); got.Intent != IntentUnsupported {
		t.Fatalf("内部术语不应被当成项目名，实际意图 %s", got.Intent)
	}

	answer, err := svc.Handle(context.Background(), ParseQuery("项目 unclassified 最近做了什么"))
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	assertNoEngineeringTerms(t, answer.Text)
}

// TestSignoffUsesConfiguredSummaryTime 验证「我下班了」的总结时间来自配置，
// 而不是写死的字符串——写死的时间会在配置被改动后骗人。
func TestSignoffUsesConfiguredSummaryTime(t *testing.T) {
	store := newQAStore(t, "2026-09-17", "lumen")
	client := ai.NewClient("https://api.deepseek.com", "", "", time.Second)

	svc := NewQAService(store, client, testLoc, 20, 21, 5)
	answer, _ := svc.Handle(context.Background(), ParseQuery("我下班了"))
	if !strings.Contains(answer.Text, "21:05") {
		t.Fatalf("应使用配置的总结时间 21:05，实际: %s", answer.Text)
	}
	if strings.Contains(answer.Text, "22:30") {
		t.Fatalf("不应出现写死的时间，实际: %s", answer.Text)
	}

	// 非法配置回退到默认值，不产生畸形时间。
	fallback := NewQAService(store, client, testLoc, 20, -1, 99)
	fb, _ := fallback.Handle(context.Background(), ParseQuery("我下班了"))
	if !strings.Contains(fb.Text, "22:30") {
		t.Fatalf("非法配置应回退到 22:30，实际: %s", fb.Text)
	}
}
