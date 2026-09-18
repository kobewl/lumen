package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// taskSummaryEvent 构造一条合法的 agent.task_summary 事件。
func taskSummaryEvent(data map[string]any) map[string]any {
	base := map[string]any{
		"id":        "01J9Z4QK7M3F8N2P5R7T9V1X3E",
		"device_id": "desktop-mac-01",
		"type":      "agent.task_summary",
		"timestamp": "2026-09-17T09:42:00Z",
		"privacy":   "P1",
		"context":   map[string]any{"app": "ZCode", "project": "lumen"},
		"data": map[string]any{
			"schema_version": 1,
			"task_id":        "zcode-2026-09-17-001",
			"title":          "接通任务摘要链路",
			"status":         "done",
			"outcomes":       []any{"新增事件类型"},
			"open_loops":     []any{},
			"source_agent":   "zcode-cli",
			"privacy_mode":   "metadata_only",
		},
	}
	for k, v := range data {
		if v == nil {
			delete(base["data"].(map[string]any), k)
			continue
		}
		base["data"].(map[string]any)[k] = v
	}
	return base
}

func checkTaskEvent(t *testing.T, ev map[string]any) *ValidationError {
	t.Helper()
	re, verr := UnmarshalEvent(mustJSON(t, ev), 64*1024)
	if verr != nil {
		return verr
	}
	_, _, verr = Validate(re, "desktop-mac-01", testNow, 5*time.Minute)
	return verr
}

// TestTaskSummaryAcceptsValidEvent 覆盖正常任务摘要。
func TestTaskSummaryAcceptsValidEvent(t *testing.T) {
	re, verr := UnmarshalEvent(mustJSON(t, taskSummaryEvent(nil)), 64*1024)
	if verr != nil {
		t.Fatalf("解析失败: %v", verr)
	}
	got, _, verr := Validate(re, "desktop-mac-01", testNow, 5*time.Minute)
	if verr != nil {
		t.Fatalf("合法任务摘要应通过校验: %v", verr)
	}
	if got.Data["task_id"] != "zcode-2026-09-17-001" {
		t.Fatalf("task_id 应被保留，实际 %v", got.Data["task_id"])
	}
	if got.Data["title"] != "接通任务摘要链路" {
		t.Fatalf("title 应被保留，实际 %v", got.Data["title"])
	}
}

// TestTaskSummaryTitleIsAllowedOnlyThere 是本次改动最关键的隐私边界。
//
// title 在 agent.task_summary 里是任务标题（合法），在任何传感器事件里
// 都只可能是窗口标题（必须拒绝）。允许任务标题不能顺带放行窗口标题。
func TestTaskSummaryTitleIsAllowedOnlyThere(t *testing.T) {
	// 1) 任务摘要的 data.title 合法。
	if verr := checkTaskEvent(t, taskSummaryEvent(nil)); verr != nil {
		t.Fatalf("任务摘要的 title 应被接受，实际: %v", verr)
	}

	// 2) 传感器事件的 data.title 必须被拒绝。
	for _, eventType := range []string{TypeWindowActivity, TypeIdleState, TypeGitActivity} {
		ev := taskSummaryEvent(nil)
		ev["type"] = eventType
		switch eventType {
		case TypeWindowActivity:
			ev["privacy"] = "P0"
			ev["context"] = map[string]any{"app": "VS Code"}
			ev["data"] = map[string]any{"duration_seconds": 60, "title": "银行登录页面"}
		case TypeIdleState:
			ev["privacy"] = "P0"
			ev["context"] = map[string]any{}
			ev["data"] = map[string]any{"state": "idle", "title": "银行登录页面"}
		case TypeGitActivity:
			ev["privacy"] = "P1"
			ev["context"] = map[string]any{"repo": "lumen"}
			ev["data"] = map[string]any{"kind": "commit", "title": "银行登录页面"}
		}
		verr := checkTaskEvent(t, ev)
		if verr == nil {
			t.Fatalf("%s 携带 data.title 必须被拒绝", eventType)
		}
		if verr.Code != "unknown_field" {
			t.Fatalf("%s 的 title 应以 unknown_field 拒绝，实际 %s(%s)",
				eventType, verr.Code, verr.Message)
		}
	}

	// 3) 任何事件类型的 context.title 都必须被拒绝（那永远是窗口标题）。
	for _, eventType := range []string{
		TypeWindowActivity, TypeIdleState, TypeGitActivity, TypeAgentTaskSummary,
	} {
		ev := taskSummaryEvent(nil)
		ev["type"] = eventType
		if eventType == TypeAgentTaskSummary {
			ev["context"] = map[string]any{"app": "ZCode", "title": "银行登录页面"}
		} else {
			ev["context"] = map[string]any{"app": "VS Code", "title": "银行登录页面"}
		}
		verr := checkTaskEvent(t, ev)
		if verr == nil {
			t.Fatalf("%s 的 context.title 必须被拒绝", eventType)
		}
		if verr.Code != "forbidden_field" {
			t.Fatalf("%s 的 context.title 应以 forbidden_field 拒绝，实际 %s", eventType, verr.Code)
		}
	}
}

// TestTaskSummaryRejectsForbiddenPayloads 覆盖各类正文载体都被拒绝。
func TestTaskSummaryRejectsForbiddenPayloads(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value any
	}{
		{"完整对话", "conversation", []any{"user: 帮我改代码", "assistant: 好的"}},
		{"终端输出", "terminal_output", "npm ERR! ..."},
		{"代码", "code", "func main() {}"},
		{"diff", "diff", "--- a/x.go\n+++ b/x.go"},
		{"凭证", "token", "sk-abcdef123456"},
		{"源码字段", "source_code", "print(1)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := taskSummaryEvent(nil)
			ev["data"].(map[string]any)[c.key] = c.value
			verr := checkTaskEvent(t, ev)
			if verr == nil {
				t.Fatalf("%s 不应被接受", c.name)
			}
			if verr.Code != "forbidden_field" && verr.Code != "unknown_field" {
				t.Fatalf("错误码应为 forbidden_field 或 unknown_field，实际 %s", verr.Code)
			}
		})
	}
}

// TestTaskSummaryRejectsSensitiveValues 覆盖取值里夹带路径/URL 的情况。
func TestTaskSummaryRejectsSensitiveValues(t *testing.T) {
	cases := map[string]map[string]any{
		"标题含路径":    {"title": "/Users/liang/Documents/Project/lumen"},
		"结果含 URL":  {"outcomes": []any{"见 https://github.com/liang/lumen"}},
		"未完成项含路径":  {"open_loops": []any{"继续改 /home/liang/work/x.go"}},
		"标题含用户名目录": {"title": "在 /Users/liang 下的改动"},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if verr := checkTaskEvent(t, taskSummaryEvent(data)); verr == nil {
				t.Fatalf("%s 应被拒绝", name)
			}
		})
	}
}

// TestTaskSummaryRejectsOversizedFields 覆盖长度与数量上限。
//
// 这些上限是"元数据摘要"这条边界的执行手段：没有它们，标题与条目
// 会被逐步当成正文通道使用。
func TestTaskSummaryRejectsOversizedFields(t *testing.T) {
	long := strings.Repeat("字", TaskTitleMaxLength+1)
	longItem := strings.Repeat("字", TaskItemMaxLength+1)

	cases := map[string]map[string]any{
		"标题超长":       {"title": long},
		"条目超长":       {"outcomes": []any{longItem}},
		"条目过多":       {"outcomes": manyItems(TaskItemsMax + 1)},
		"task_id 超长": {"task_id": strings.Repeat("a", TaskIDMaxLength+1)},
		"空条目":        {"outcomes": []any{"  "}},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			verr := checkTaskEvent(t, taskSummaryEvent(data))
			if verr == nil {
				t.Fatalf("%s 应被拒绝", name)
			}
			if verr.Code != "field_too_long" && verr.Code != "invalid_format" {
				t.Fatalf("错误码应为 field_too_long 或 invalid_format，实际 %s(%s)", verr.Code, verr.Message)
			}
		})
	}
}

func manyItems(n int) []any {
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "条目内容")
	}
	return out
}

// TestTaskSummaryRejectsInvalidEnums 覆盖状态、隐私模式与 schema 版本。
func TestTaskSummaryRejectsInvalidEnums(t *testing.T) {
	cases := map[string]map[string]any{
		"未知状态":      {"status": "in_progress"},
		"完整正文模式":    {"privacy_mode": "full_text"},
		"空隐私模式":     {"privacy_mode": ""},
		"错误 schema": {"schema_version": 99},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if verr := checkTaskEvent(t, taskSummaryEvent(data)); verr == nil {
				t.Fatalf("%s 应被拒绝", name)
			}
		})
	}
}

// TestTaskSummaryRejectsIncompleteEvent 覆盖必填字段缺失。
func TestTaskSummaryRejectsIncompleteEvent(t *testing.T) {
	cases := map[string]map[string]any{
		"缺 task_id":      {"task_id": nil},
		"缺 title":        {"title": nil},
		"缺 status":       {"status": nil},
		"缺 source_agent": {"source_agent": nil},
		"缺 privacy_mode": {"privacy_mode": nil},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			verr := checkTaskEvent(t, taskSummaryEvent(data))
			if verr == nil {
				t.Fatalf("%s 应被拒绝", name)
			}
			if verr.Code != "missing_field" {
				t.Fatalf("错误码应为 missing_field，实际 %s(%s)", verr.Code, verr.Message)
			}
		})
	}
}

// TestTaskSummaryRejectsWrongSeverity 覆盖隐私等级：任务摘要必须是 P1。
func TestTaskSummaryRejectsWrongSeverity(t *testing.T) {
	ev := taskSummaryEvent(nil)
	ev["privacy"] = "P0"
	if verr := checkTaskEvent(t, ev); verr == nil || verr.Code != "invalid_privacy" {
		t.Fatalf("P0 的任务摘要应被拒绝，实际: %v", verr)
	}
}

// TestTaskSummaryRejectsBadTaskID 覆盖 task_id 字符集。
//
// ID 本身也可能成为载荷（空格、斜杠、引号），因此限制字符集而不是只看长度。
func TestTaskSummaryRejectsBadTaskID(t *testing.T) {
	for _, bad := range []string{
		"task id with spaces",
		"../../etc/passwd",
		"task/../../x",
		"task\"quote",
	} {
		t.Run(bad, func(t *testing.T) {
			verr := checkTaskEvent(t, taskSummaryEvent(map[string]any{"task_id": bad}))
			if verr == nil {
				t.Fatalf("%q 应被拒绝", bad)
			}
			if verr.Code != "invalid_task_id" {
				t.Fatalf("错误码应为 invalid_task_id，实际 %s", verr.Code)
			}
		})
	}
}

// TestTaskSummaryGoldenMatchesValidator 保证 golden 样例与校验器一致。
//
// 两端契约必须能被同一个样例证明：schema 说合法的，校验器就必须接受。
func TestTaskSummaryGoldenMatchesValidator(t *testing.T) {
	raw := `{"id":"01J9Z4QK7M3F8N2P5R7T9V1X3E","device_id":"desktop-mac-01","type":"agent.task_summary","timestamp":"2026-09-17T09:42:00Z","privacy":"P1","context":{"app":"ZCode","project":"lumen"},"data":{"schema_version":1,"task_id":"zcode-task-2026-09-17-001","title":"给事件协议加任务摘要类型","status":"done","outcomes":["新增 agent.task_summary 事件类型","补齐协议 schema 与 golden 样例"],"open_loops":["尚未接入真实 Agent 上报"],"source_agent":"zcode-cli","source_session_id":"sess-9f3c1d8b","privacy_mode":"metadata_only"}}`

	re, verr := UnmarshalEvent([]byte(raw), 64*1024)
	if verr != nil {
		t.Fatalf("golden 解析失败: %v", verr)
	}
	got, _, verr := Validate(re, "desktop-mac-01", testNow, 5*time.Minute)
	if verr != nil {
		t.Fatalf("golden 校验失败: %v", verr)
	}
	var outcomes []any
	if err := json.Unmarshal([]byte(mustJSONString(t, got.Data["outcomes"])), &outcomes); err != nil {
		t.Fatalf("outcomes 应可解析为数组: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcomes 应有 2 条，实际 %d", len(outcomes))
	}
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(b)
}
