package ai

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/storage"
)

var testLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// TestBuildInputUsesDisplayNameForUnclassified 验证发给模型的项目名是可读文案。
//
// 直接把 unclassified 发过去，模型会把它当成项目名原样写进总结正文，
// 用户就会在飞书上看到 "▍unclassified"。分组仍按内部标识，只改展示名。
func TestBuildInputUsesDisplayNameForUnclassified(t *testing.T) {
	start := time.Date(2026, 9, 17, 9, 0, 0, 0, testLoc)
	sessions := []storage.Session{{
		ID: "s_test", Date: "2026-09-17", Project: SessionsUnclassified,
		StartAt: start, EndAt: start.Add(30 * time.Minute),
		AppsJSON:  `[{"app":"ZCode","duration_minutes":30}]`,
		GitJSON:   "[]",
		StatsJSON: `{"duration_minutes":30,"app_count":1,"git_event_count":0}`,
	}}

	in := BuildInput("2026-09-17", testLoc, sessions)
	if len(in.Projects) != 1 {
		t.Fatalf("应有 1 个项目，实际 %d", len(in.Projects))
	}
	if in.Projects[0].Name != "暂未识别项目" {
		t.Fatalf("项目名应为可读文案，实际 %q", in.Projects[0].Name)
	}
	raw, _ := json.Marshal(in)
	if strings.Contains(string(raw), SessionsUnclassified) {
		t.Fatalf("发给模型的内容不应含内部标记: %s", raw)
	}
}

// TestRenderOmitsInternalIDs 验证总结正文不含内部 Session ID。
func TestRenderOmitsInternalIDs(t *testing.T) {
	rendered := Render(StructuredSummary{
		Date:     "2026-09-17",
		Headline: "推进 Lumen 的时间记录",
		Projects: []SummaryProject{{
			Name:               "lumen",
			DurationMinutes:    45,
			Activities:         []string{"使用 ZCode 约 45 分钟"},
			EvidenceSessionIDs: []string{"s_9f3c1d8b7a6e5f4012345678"},
		}},
	})
	if strings.Contains(rendered, "s_9f3c") {
		t.Fatalf("用户可见总结不应包含内部 ID:\n%s", rendered)
	}
	if !strings.Contains(rendered, "lumen") || !strings.Contains(rendered, "45") {
		t.Fatalf("总结应保留项目与时长:\n%s", rendered)
	}
}

// TestRenderUsesDisplayName 验证渲染时内部标记被替换。
func TestRenderUsesDisplayName(t *testing.T) {
	rendered := Render(StructuredSummary{
		Date:     "2026-09-17",
		Headline: "记录",
		Projects: []SummaryProject{{
			Name:               SessionsUnclassified,
			DurationMinutes:    20,
			Activities:         []string{"使用 ZCode 约 20 分钟"},
			EvidenceSessionIDs: []string{"s_x"},
		}},
	})
	if strings.Contains(rendered, SessionsUnclassified) {
		t.Fatalf("渲染结果不应含 unclassified:\n%s", rendered)
	}
	if !strings.Contains(rendered, "暂未识别项目") {
		t.Fatalf("应显示为可读项目名:\n%s", rendered)
	}
}

// TestProjectDisplayName 覆盖展示名映射。
func TestProjectDisplayName(t *testing.T) {
	cases := map[string]string{
		SessionsUnclassified: "暂未识别项目",
		"":                   "暂未识别项目",
		"lumen":              "lumen",
	}
	for in, want := range cases {
		if got := ProjectDisplayName(in); got != want {
			t.Fatalf("ProjectDisplayName(%q) 应为 %q，实际 %q", in, want, got)
		}
	}
}

// TestSummaryPromptForbidsTaskInflation 是提示词的回归测试。
//
// 只有应用名和时长时，模型必须陈述「用了什么、多久」，
// 不能把它包装成「完成了某项功能」。
func TestSummaryPromptForbidsTaskInflation(t *testing.T) {
	for _, want := range []string{"不等于", "任务内容", "使用 XX 约 N 分钟"} {
		if !strings.Contains(SummarySystemPrompt, want) {
			t.Fatalf("总结提示词应包含约束 %q，否则模型会把应用时长包装成任务", want)
		}
	}
}
