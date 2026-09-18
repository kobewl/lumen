package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func validWindowEvent() map[string]any {
	return map[string]any{
		"id":        "01J9Z4QK7M3F8N2P5R7T9V1X3B",
		"device_id": "desktop-mac-01",
		"type":      "window.activity",
		"timestamp": "2026-09-17T08:00:00Z",
		"privacy":   "P0",
		"context":   map[string]any{"app": "Visual Studio Code", "project": "lumen"},
		"data":      map[string]any{"duration_seconds": 320},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return b
}

func TestValidateAcceptsGoldenEvent(t *testing.T) {
	raw := mustJSON(t, validWindowEvent())
	re, verr := UnmarshalEvent(raw, 64*1024)
	if verr != nil {
		t.Fatalf("解析失败: %v", verr)
	}
	got, skew, verr := Validate(re, "desktop-mac-01", testNow, 5*time.Minute)
	if verr != nil {
		t.Fatalf("校验失败: %v", verr)
	}
	if skew {
		t.Fatal("正常时间不应标记 clock_skew")
	}
	if got.Type != TypeWindowActivity || got.Context["app"] != "Visual Studio Code" {
		t.Fatalf("解析结果不符: %+v", got)
	}
}

func TestValidateRejectsForbiddenFields(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value any
	}{
		{"顶层剪贴板", "clipboard", "secret"},
		{"顶层截图", "screenshot", "base64"},
		{"data 内源代码", "source_code", "print(1)"},
		{"context 内绝对路径", "abs_path", "/Users/liang/code"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := validWindowEvent()
			// 分别插入到不同层级，验证每一层都会被检查。
			switch c.field {
			case "clipboard", "screenshot":
				ev[c.field] = c.value
			default:
				data := ev["data"].(map[string]any)
				data[c.field] = c.value
			}
			raw := mustJSON(t, ev)
			re, verr := UnmarshalEvent(raw, 64*1024)
			if verr != nil {
				t.Fatalf("解析失败: %v", verr)
			}
			_, _, verr = Validate(re, "desktop-mac-01", testNow, 5*time.Minute)
			if verr == nil {
				t.Fatal("禁用字段应当被拒绝")
			}
			if verr.Code != "forbidden_field" && verr.Code != "unknown_field" {
				t.Fatalf("错误码应为 forbidden_field 或 unknown_field，实际 %s(%s)", verr.Code, verr.Message)
			}
		})
	}
}

func TestValidateRejectsUnknownType(t *testing.T) {
	ev := validWindowEvent()
	ev["type"] = "clipboard.copy"
	re, _ := UnmarshalEvent(mustJSON(t, ev), 64*1024)
	_, _, verr := Validate(re, "desktop-mac-01", testNow, 5*time.Minute)
	if verr == nil || verr.Code != "unknown_type" {
		t.Fatalf("未知类型应被拒绝，实际: %v", verr)
	}
}

func TestValidateRejectsSensitiveValues(t *testing.T) {
	cases := map[string]string{
		"绝对路径": "/Users/liang/Documents/Project/lumen",
		"URL":  "https://github.com/liang/lumen.git",
		"home": "/home/liang/work",
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			ev := validWindowEvent()
			ev["context"] = map[string]any{"app": "VS Code", "project": val}
			re, _ := UnmarshalEvent(mustJSON(t, ev), 64*1024)
			_, _, verr := Validate(re, "desktop-mac-01", testNow, 5*time.Minute)
			if verr == nil {
				t.Fatalf("敏感值 %q 应被拒绝", val)
			}
			if verr.Code != "sensitive_value" {
				t.Fatalf("错误码应为 sensitive_value，实际 %s", verr.Code)
			}
		})
	}
}

func TestValidateRejectsDeviceMismatch(t *testing.T) {
	raw := mustJSON(t, validWindowEvent())
	re, _ := UnmarshalEvent(raw, 64*1024)
	_, _, verr := Validate(re, "other-device", testNow, 5*time.Minute)
	if verr == nil || verr.Code != "device_mismatch" {
		t.Fatalf("设备不一致应被拒绝，实际: %v", verr)
	}
}

func TestValidateDetectsClockSkew(t *testing.T) {
	ev := validWindowEvent()
	ev["timestamp"] = "2026-09-18T08:00:00Z" // 比测试时间超前 20 小时
	re, _ := UnmarshalEvent(mustJSON(t, ev), 64*1024)
	_, skew, verr := Validate(re, "desktop-mac-01", testNow, 5*time.Minute)
	if verr != nil {
		t.Fatalf("校验失败: %v", verr)
	}
	if !skew {
		t.Fatal("超前时间应标记 clock_skew")
	}
}

func TestValidateGitEvent(t *testing.T) {
	ev := map[string]any{
		"id":        "01J9Z4QK7M3F8N2P5R7T9V1X3D",
		"device_id": "desktop-mac-01",
		"type":      "git.activity",
		"timestamp": "2026-09-17T08:20:00Z",
		"privacy":   "P1",
		"context":   map[string]any{"repo": "lumen"},
		"data": map[string]any{
			"kind": "commit", "branch": "main",
			"head_commit":    "a1b2c3d4e5f6078899aabbccddeeff0011223344",
			"commit_message": "docs: freeze V0.1 scope", "changed_files_count": 4,
		},
	}
	re, _ := UnmarshalEvent(mustJSON(t, ev), 64*1024)
	got, _, verr := Validate(re, "desktop-mac-01", testNow, 5*time.Minute)
	if verr != nil {
		t.Fatalf("git 事件应通过校验: %v", verr)
	}
	if got.Data["head_commit"] == "" {
		t.Fatal("head_commit 应被保留")
	}
}

func TestValidateRejectsBadCommit(t *testing.T) {
	ev := map[string]any{
		"id":        "01J9Z4QK7M3F8N2P5R7T9V1X3D",
		"device_id": "desktop-mac-01",
		"type":      "git.activity",
		"timestamp": "2026-09-17T08:20:00Z",
		"privacy":   "P1",
		"context":   map[string]any{"repo": "lumen"},
		"data":      map[string]any{"kind": "commit", "head_commit": "NOT-A-COMMIT"},
	}
	re, _ := UnmarshalEvent(mustJSON(t, ev), 64*1024)
	_, _, verr := Validate(re, "desktop-mac-01", testNow, 5*time.Minute)
	if verr == nil || verr.Code != "invalid_commit" {
		t.Fatalf("非法 commit 应被拒绝，实际: %v", verr)
	}
}

func TestValidateRejectsOversizedEvent(t *testing.T) {
	big := strings.Repeat("a", 2048)
	ev := validWindowEvent()
	ev["context"] = map[string]any{"app": big, "project": "lumen"}
	raw := mustJSON(t, ev)
	_, verr := UnmarshalEvent(raw, 1024)
	if verr == nil || verr.Code != "event_too_large" {
		t.Fatalf("超大事件应被拒绝，实际: %v", verr)
	}
}

func TestGoldenFileMatchesValidator(t *testing.T) {
	// golden 样例必须能通过校验，保证两端契约一致。
	golden := []string{
		`{"id":"01J9Z4QK7M3F8N2P5R7T9V1X3B","device_id":"desktop-mac-01","type":"window.activity","timestamp":"2026-09-17T08:00:00Z","privacy":"P0","context":{"app":"Visual Studio Code","bundle_id":"com.microsoft.VSCode","project":"lumen"},"data":{"duration_seconds":320,"checkpoint":true}}`,
		`{"id":"01J9Z4QK7M3F8N2P5R7T9V1X3C","device_id":"desktop-mac-01","type":"idle.state","timestamp":"2026-09-17T08:05:20Z","privacy":"P0","context":{},"data":{"state":"idle","duration_seconds":512}}`,
		`{"id":"01J9Z4QK7M3F8N2P5R7T9V1X3D","device_id":"desktop-mac-01","type":"git.activity","timestamp":"2026-09-17T08:20:00Z","privacy":"P1","context":{"repo":"lumen","project":"lumen"},"data":{"kind":"commit","branch":"main","head_commit":"a1b2c3d4e5f6078899aabbccddeeff0011223344","commit_message":"docs: freeze V0.1 scope","changed_files_count":4}}`,
	}
	for i, raw := range golden {
		re, verr := UnmarshalEvent([]byte(raw), 64*1024)
		if verr != nil {
			t.Fatalf("golden[%d] 解析失败: %v", i, verr)
		}
		if _, _, verr := Validate(re, "desktop-mac-01", testNow, 5*time.Minute); verr != nil {
			t.Fatalf("golden[%d] 校验失败: %v", i, verr)
		}
	}
}

// TestSystemPseudoApps 覆盖系统伪应用清单。
//
// 清单必须与采集端桌面端 sensors/macos.py 保持一致：采集端过滤是为了不再
// 产生脏数据，服务端过滤是为了让历史脏数据在重算时被忽略。
func TestSystemPseudoApps(t *testing.T) {
	pseudo := []struct{ app, bundle string }{
		{"loginwindow", "com.apple.loginwindow"},
		{"Loginwindow", ""},
		{"ScreenSaverEngine", "com.apple.ScreenSaver.Engine"},
		{"SecurityAgent", "com.apple.SecurityAgent"},
		{"CoreAuthUI", ""},
		{"", "com.apple.loginwindow"},
	}
	for _, c := range pseudo {
		if !IsSystemPseudoApp(c.app, c.bundle) {
			t.Fatalf("%q/%q 应被识别为系统伪应用", c.app, c.bundle)
		}
	}

	real := []struct{ app, bundle string }{
		{"ZCode", "dev.zcode.app"},
		{"Tabbit浏览器", "com.tab-browser.Tabbit"},
		{"Finder", "com.apple.finder"},
		{"", ""},
	}
	for _, c := range real {
		if IsSystemPseudoApp(c.app, c.bundle) {
			t.Fatalf("%q/%q 是真实应用，不应被过滤", c.app, c.bundle)
		}
	}
}

// TestIdleStateAcceptsSchemaVersion 验证 schema_version 字段被允许，
// 且非法的 duration_seconds 被拒绝。
func TestIdleStateAcceptsSchemaVersion(t *testing.T) {
	now := time.Date(2026, 9, 18, 2, 0, 0, 0, time.UTC)

	valid := idleRawEvent(t, map[string]any{"state": "locked", "schema_version": 2})
	if _, _, verr := Validate(valid, "dev-1", now, 5*time.Minute); verr != nil {
		t.Fatalf("schema_version 应被接受，实际 %v", verr)
	}

	// 开放区间：没有 duration_seconds 也合法。
	open := idleRawEvent(t, map[string]any{"state": "idle", "schema_version": 2})
	if _, _, verr := Validate(open, "dev-1", now, 5*time.Minute); verr != nil {
		t.Fatalf("未闭合的区间应合法，实际 %v", verr)
	}

	bad := idleRawEvent(t, map[string]any{"state": "idle", "duration_seconds": -5})
	if _, _, verr := Validate(bad, "dev-1", now, 5*time.Minute); verr == nil {
		t.Fatal("负数的 duration_seconds 应被拒绝")
	}
}

func idleRawEvent(t *testing.T, data map[string]any) RawEvent {
	t.Helper()
	dataJSON, _ := json.Marshal(data)
	raw, _ := json.Marshal(map[string]any{
		"id": "01J9Z4QK7M3F8N2P5R7T9V1X3C", "device_id": "dev-1", "type": "idle.state",
		"timestamp": "2026-09-17T12:42:30Z", "privacy": "P0",
		"context": map[string]any{}, "data": json.RawMessage(dataJSON),
	})
	ev, verr := UnmarshalEvent(raw, 64*1024)
	if verr != nil {
		t.Fatalf("解析测试事件失败: %v", verr)
	}
	return ev
}
