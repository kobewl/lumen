package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/storage"
)

var testLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

func TestParseQueryIntents(t *testing.T) {
	cases := []struct {
		text   string
		intent string
		value  string
	}{
		{"今天做了什么", IntentToday, ""},
		{"today", IntentToday, ""},
		{"昨日总结", IntentYesterday, ""},
		{"yesterday", IntentYesterday, ""},
		{"项目 lumen 最近做了什么", IntentProject, "lumen"},
		{"lumen 项目的进展", IntentProject, "lumen"},
		// 闲聊类意图（各自有确定性回复，见 qa_conversation_test.go）。
		{"你好呀", IntentGreeting, ""},
		{"你叫什么", IntentIdentity, ""},
		{"你能做什么", IntentCapability, ""},
		{"我下班了", IntentSignoff, ""},
		// 真正无法识别的请求仍走帮助文本。
		{"帮我写一份周报", IntentUnsupported, ""},
	}
	for _, c := range cases {
		got := ParseQuery(c.text)
		if got.Intent != c.intent {
			t.Fatalf("%q 意图应为 %s，实际 %s", c.text, c.intent, got.Intent)
		}
		if c.value != "" && got.Project != c.value {
			t.Fatalf("%q 项目应为 %q，实际 %q", c.text, c.value, got.Project)
		}
	}
}

// newQAStore 创建带数据的测试库。
func newQAStore(t *testing.T, date string, project string) *storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "qa.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	loc := testLoc
	start := time.Date(2026, 9, 17, 9, 0, 0, 0, loc)
	if date != "" {
		parsed, err := time.ParseInLocation("2006-01-02", date, loc)
		if err != nil {
			t.Fatalf("日期解析失败: %v", err)
		}
		start = parsed.Add(9 * time.Hour)
	}

	apps, _ := json.Marshal([]map[string]any{{"app": "Visual Studio Code", "duration_minutes": 90}})
	// 提交信息用中性文案：它是用户自己的数据，会被原样展示，
	// 因此夹具里不该出现「session」这类会干扰文案断言的词。
	gits, _ := json.Marshal([]map[string]any{{"branch": "main", "commit_message": "feat: 修复同步重试"}})
	stats, _ := json.Marshal(map[string]any{"duration_minutes": 90, "app_count": 1, "git_event_count": 1})

	sess := storage.Session{
		ID: "s_test_session_0001", Date: start.Format("2006-01-02"), Project: project,
		StartAt: start, EndAt: start.Add(90 * time.Minute),
		AppsJSON: string(apps), GitJSON: string(gits), StatsJSON: string(stats),
		AlgorithmVersion: "rules-v1",
		SourceStartAt:    start, SourceEndAt: start.Add(24 * time.Hour),
		UpdatedAt: time.Now().UTC(),
	}
	from := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc).UTC()
	if err := store.ReplaceSessions(context.Background(), sess.Date, from, from.Add(24*time.Hour), []storage.Session{sess}); err != nil {
		t.Fatalf("写入 Session 失败: %v", err)
	}
	return store
}

func TestAnswerWithoutAIUsesDeterministicText(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newQAStore(t, today, "lumen")

	// 传入未启用的 AI 客户端，回答应降级为确定性摘要。
	client := ai.NewClient("https://api.deepseek.com", "", "", time.Second)
	svc := NewQAService(store, client, testLoc, 20, 22, 30)

	answer, err := svc.Handle(context.Background(), ParseQuery("今天做了什么"))
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if answer.Status != "ok_no_ai" {
		t.Fatalf("应走无 AI 路径，实际 %s", answer.Status)
	}
	if !contains(answer.Text, "lumen") {
		t.Fatalf("回答应包含项目名，实际: %s", answer.Text)
	}
	if !contains(answer.Text, "证据") {
		t.Fatalf("回答应包含证据说明，实际: %s", answer.Text)
	}
	if len(answer.SourceSessionIDs) == 0 {
		t.Fatal("回答应带来源 Session")
	}
}

func TestAnswerForEmptyDay(t *testing.T) {
	store := newQAStore(t, "2020-01-01", "lumen")
	client := ai.NewClient("https://api.deepseek.com", "", "", time.Second)
	svc := NewQAService(store, client, testLoc, 20, 22, 30)

	answer, err := svc.Handle(context.Background(), ParseQuery("今天做了什么"))
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if answer.Status != "no_data" {
		t.Fatalf("无数据时应返回 no_data，实际 %s", answer.Status)
	}
	if !contains(answer.Text, "没有记录") {
		t.Fatalf("应明确说明没有记录，实际: %s", answer.Text)
	}
}

func TestAnswerForUnknownProjectListsKnownProjects(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newQAStore(t, today, "lumen")
	client := ai.NewClient("https://api.deepseek.com", "", "", time.Second)
	svc := NewQAService(store, client, testLoc, 20, 22, 30)

	answer, err := svc.Handle(context.Background(), ParseQuery("项目 nonexistent 最近做了什么"))
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if answer.Status != "no_data" {
		t.Fatalf("未知项目应返回 no_data，实际 %s", answer.Status)
	}
	if !contains(answer.Text, "lumen") {
		t.Fatalf("回答应列出已知项目，实际: %s", answer.Text)
	}
}

func TestUnsupportedIntentReturnsHelp(t *testing.T) {
	store := newQAStore(t, "2026-09-17", "lumen")
	client := ai.NewClient("https://api.deepseek.com", "", "", time.Second)
	svc := NewQAService(store, client, testLoc, 20, 22, 30)

	answer, _ := svc.Handle(context.Background(), ParseQuery("帮我写一份周报"))
	if answer.Status != "help" {
		t.Fatalf("不支持的意图应返回帮助文本，实际 %s", answer.Status)
	}
}

// TestAnswerWithAIFallsBackOnError 验证模型失败时降级而不是报错。
func TestAnswerWithAIFallsBackOnError(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newQAStore(t, today, "lumen")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := ai.NewClient(server.URL, "test-key", "deepseek-chat", 5*time.Second)
	svc := NewQAService(store, client, testLoc, 20, 22, 30)

	answer, err := svc.Handle(context.Background(), ParseQuery("今天做了什么"))
	if err != nil {
		t.Fatalf("模型失败时不应返回错误: %v", err)
	}
	if answer.Status != "ai_failed_fallback" {
		t.Fatalf("应降级为确定性摘要，实际 %s", answer.Status)
	}
	if !contains(answer.Text, "证据") {
		t.Fatalf("降级回答仍应带证据，实际: %s", answer.Text)
	}
}

func TestBudgetExhaustionBlocksAI(t *testing.T) {
	today := time.Now().In(testLoc).Format("2006-01-02")
	store := newQAStore(t, today, "lumen")

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": `{"answer":"ok"}`}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer server.Close()

	client := ai.NewClient(server.URL, "test-key", "deepseek-chat", 5*time.Second)

	// 先把预算用完。
	if err := store.AddAIUsage(context.Background(), today, "query", 10, 10); err != nil {
		t.Fatalf("写入用量失败: %v", err)
	}
	svc := NewQAService(store, client, testLoc, 1, 22, 30)

	answer, _ := svc.Handle(context.Background(), ParseQuery("今天做了什么"))
	if answer.Status != "ok_no_ai" {
		t.Fatalf("预算用尽时应跳过模型，实际 %s", answer.Status)
	}
	if calls != 0 {
		t.Fatalf("预算用尽时不应调用模型，实际调用 %d 次", calls)
	}
}

func TestMatchProject(t *testing.T) {
	known := []string{"lumen", "clipmaster-pro"}
	cases := map[string]string{
		"lumen":       "lumen",
		"LUMEN":       "lumen",
		"clipmaster":  "clipmaster-pro",
		"nonexistent": "",
		"":            "",
	}
	for query, want := range cases {
		if got := matchProject(query, known); got != want {
			t.Fatalf("matchProject(%q) 应为 %q，实际 %q", query, want, got)
		}
	}
}

func TestEvidenceLineIncludesRange(t *testing.T) {
	start := time.Date(2026, 9, 17, 9, 0, 0, 0, testLoc)
	sessions := []storage.Session{
		{ID: "s1", StartAt: start, EndAt: start.Add(time.Hour)},
		{ID: "s2", StartAt: start.Add(2 * time.Hour), EndAt: start.Add(3 * time.Hour)},
	}
	line := evidenceLine(sessions, testLoc)
	if !contains(line, "2 段记录") {
		t.Fatalf("应说明记录段数，实际: %s", line)
	}
	if !contains(line, "09-17") {
		t.Fatalf("应包含日期，实际: %s", line)
	}
	// 回归测试：时间必须按传入时区显示。
	// Session 内部时间若是 UTC，直接格式化会让用户看到早 8 小时的时间段。
	if !contains(line, "09:00") {
		t.Fatalf("应按本地时区显示起始时间 09:00，实际: %s", line)
	}
	if !contains(line, "12:00") {
		t.Fatalf("应按本地时区显示结束时间 12:00，实际: %s", line)
	}
}

// TestEvidenceLineUsesProvidedTimezone 确认不同时区下显示的小时数不同。
func TestEvidenceLineUsesProvidedTimezone(t *testing.T) {
	start := time.Date(2026, 9, 17, 9, 0, 0, 0, testLoc) // 上海 09:00 = UTC 01:00
	sessions := []storage.Session{
		{ID: "s1", StartAt: start, EndAt: start.Add(time.Hour)},
	}

	shanghai := evidenceLine(sessions, testLoc)
	if !contains(shanghai, "09:00") {
		t.Fatalf("上海时区应显示 09:00，实际: %s", shanghai)
	}

	utc := evidenceLine(sessions, time.UTC)
	if !contains(utc, "01:00") {
		t.Fatalf("UTC 应显示 01:00，实际: %s", utc)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestEvidenceLineSameDayOmitsRepeatedDate 验证同一天的证据行不重复写日期，
// 跨天才写完整区间。
func TestEvidenceLineSameDayOmitsRepeatedDate(t *testing.T) {
	start := time.Date(2026, 9, 17, 20, 17, 0, 0, testLoc)
	sameDay := []storage.Session{{ID: "s1", StartAt: start, EndAt: start.Add(24 * time.Minute)}}
	line := evidenceLine(sameDay, testLoc)
	if !contains(line, "09-17 20:17 ~ 20:41") {
		t.Fatalf("同一天应写成 09-17 20:17 ~ 20:41，实际: %s", line)
	}

	crossDay := []storage.Session{
		{ID: "s1", StartAt: start, EndAt: start.Add(2 * time.Hour)},
		{ID: "s2", StartAt: start.AddDate(0, 0, 1), EndAt: start.AddDate(0, 0, 1).Add(time.Hour)},
	}
	crossLine := evidenceLine(crossDay, testLoc)
	if !contains(crossLine, "09-17 20:17 ~ 09-18 21:17") {
		t.Fatalf("跨天应写完整区间，实际: %s", crossLine)
	}
}
