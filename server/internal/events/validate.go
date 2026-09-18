// Package events 负责事件校验与最小化约束。
//
// 校验策略是“白名单 + 明确拒绝”：
//   - 只接受 V0.1 声明的三种事件类型；
//   - 字段名必须在 allowlist 内，出现 clipboard、screen、source_code、diff、
//     terminal_output 等禁用字段直接拒绝，并记录安全事件；
//   - 不做“先存下来以后再说”，未知类型不落库。
package events

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 允许的事件类型。
const (
	TypeWindowActivity = "window.activity"
	TypeIdleState      = "idle.state"
	TypeGitActivity    = "git.activity"
)

// 隐私等级。
const (
	PrivacyP0 = "P0"
	PrivacyP1 = "P1"
)

// IdleSchemaVersionV2 是 idle.state 区间语义的版本号。
//
// 语义（采集端与服务端必须一致）：
//
//	timestamp             = 状态区间的开始时刻
//	data.duration_seconds = 区间时长；缺省表示区间仍在进行中
//
// 历史数据（没有 schema_version，或有 duration_seconds）按旧语义解释：
// timestamp 是区间**结束**时刻，区间为 [timestamp - duration, timestamp]。
// 这个差异必须靠判定函数集中处理，不能让每个消费方各自猜。
const IdleSchemaVersionV2 = 2

// 禁用字段：出现即拒绝，防止未来误加采集。
var forbiddenFields = map[string]string{
	"clipboard":       "V0.1 不采集剪贴板",
	"clipboard_text":  "V0.1 不采集剪贴板",
	"screen":          "V0.1 不采集屏幕",
	"screenshot":      "V0.1 不采集截图",
	"source_code":     "V0.1 不采集源代码",
	"code":            "V0.1 不采集源代码",
	"diff":            "V0.1 不采集 diff",
	"patch":           "V0.1 不采集 diff",
	"terminal_output": "V0.1 不采集终端输出",
	"file_content":    "V0.1 不采集文件正文",
	"absolute_path":   "不上传绝对路径",
	"path":            "不上传绝对路径",
	"remote_url":      "不上传 Git remote URL",
	"window_title":    "窗口标题默认不上传",
	"title":           "窗口标题默认不上传",
	"username":        "不上传用户名",
	"token":           "不上传任何凭证",
	"api_key":         "不上传任何凭证",
	"password":        "不上传任何凭证",
}

// context 允许的字段。
var allowedContext = map[string]bool{
	"app": true, "bundle_id": true, "project": true, "repo": true,
}

// data 允许的字段。
var allowedData = map[string]bool{
	"duration_seconds": true, "checkpoint": true, "state": true,
	"branch": true, "head_commit": true, "commit_message": true,
	"changed_files_count": true, "kind": true, "schema_version": true,
}

// 系统伪应用：锁屏、屏保与系统认证窗口。
// 出现在前台不代表用户在工作，任何统计都必须忽略它们。
//
// 清单与采集端 desktop/lumen_desktop/sensors/macos.py 保持一致：
// 采集端过滤是为了不产生新的脏数据，服务端过滤是为了历史脏数据在重算时被忽略。
var systemPseudoAppNames = map[string]bool{
	"loginwindow":       true,
	"screensaverengine": true,
	"securityagent":     true,
	"coreautha":         true,
	"coreauthui":        true,
	"loginstatus":       true,
}

var systemPseudoBundlePrefixes = []string{
	"com.apple.loginwindow",
	"com.apple.screensaver",
	"com.apple.securityagent",
	"com.apple.coreauthui",
}

// IsSystemPseudoApp 判断应用是否为系统伪应用。
//
// 判断基于应用名与 bundle id，而不是「时长」或「时段」这类间接信号：
// 真机上 loginwindow 就是被当成普通前台应用连续记录了整夜，
// 只有按身份过滤才能稳定地拦住它。
func IsSystemPseudoApp(app, bundleID string) bool {
	name := strings.ToLower(strings.TrimSpace(app))
	if name != "" && systemPseudoAppNames[name] {
		return true
	}
	bundle := strings.ToLower(strings.TrimSpace(bundleID))
	for _, prefix := range systemPseudoBundlePrefixes {
		if bundle != "" && strings.HasPrefix(bundle, prefix) {
			return true
		}
	}
	return false
}

var (
	ulidRe   = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	commitRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	// userPathRe 用于检测路径与用户名残留。
	userPathRe = regexp.MustCompile(`(/Users/|/home/|C:\\Users\\)`)
	urlRe      = regexp.MustCompile(`(?i)^(https?|ssh|git)://`)
)

// Event 是校验通过后的事件。
type Event struct {
	ID        string
	DeviceID  string
	Type      string
	Timestamp time.Time
	Privacy   string
	Context   map[string]any
	Data      map[string]any
}

// RawEvent 是请求中原样解析出来的一批事件，用于逐条报错。
type RawEvent struct {
	ID        string                     `json:"id"`
	DeviceID  string                     `json:"device_id"`
	Type      string                     `json:"type"`
	Timestamp string                     `json:"timestamp"`
	Privacy   string                     `json:"privacy"`
	Context   map[string]json.RawMessage `json:"context"`
	Data      map[string]json.RawMessage `json:"data"`
	// Extra 捕获所有未声明字段，便于给出精确错误并检测禁用字段。
	Extra map[string]json.RawMessage `json:"-"`
}

// ValidationError 描述一条事件的校验失败原因。
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Code + ": " + e.Message }

func invalid(code, format string, args ...any) *ValidationError {
	return &ValidationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// UnmarshalEvent 解析单条事件原始 JSON，同时保留未声明字段用于校验。
func UnmarshalEvent(raw []byte, maxBytes int64) (RawEvent, *ValidationError) {
	if int64(len(raw)) > maxBytes {
		return RawEvent{}, invalid("event_too_large", "单事件超过 %d 字节上限", maxBytes)
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return RawEvent{}, invalid("invalid_json", "事件不是合法 JSON")
	}

	var e RawEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return RawEvent{}, invalid("invalid_json", "事件字段类型不正确: %v", err)
	}

	known := map[string]bool{
		"id": true, "device_id": true, "type": true, "timestamp": true,
		"privacy": true, "context": true, "data": true,
	}
	extra := make(map[string]json.RawMessage)
	for k := range probe {
		if !known[k] {
			extra[k] = probe[k]
		}
	}
	e.Extra = extra
	return e, nil
}

// Validate 校验一条事件是否满足协议要求。
// deviceID 是鉴权得到的设备标识，必须与 payload 中的 device_id 一致。
func Validate(e RawEvent, deviceID string, now time.Time, skewTolerance time.Duration) (Event, bool, *ValidationError) {
	// 1. 禁用字段检测（顶层、context、data 都要查）。
	for field := range e.Extra {
		if reason, bad := forbiddenFields[strings.ToLower(field)]; bad {
			return Event{}, false, invalid("forbidden_field", "字段 %s 被禁止: %s", field, reason)
		}
		return Event{}, false, invalid("unknown_field", "未声明字段: %s", field)
	}
	for field := range e.Context {
		if reason, bad := forbiddenFields[strings.ToLower(field)]; bad {
			return Event{}, false, invalid("forbidden_field", "context.%s 被禁止: %s", field, reason)
		}
		if !allowedContext[field] {
			return Event{}, false, invalid("unknown_field", "context 未声明字段: %s", field)
		}
	}
	for field := range e.Data {
		if reason, bad := forbiddenFields[strings.ToLower(field)]; bad {
			return Event{}, false, invalid("forbidden_field", "data.%s 被禁止: %s", field, reason)
		}
		if !allowedData[field] {
			return Event{}, false, invalid("unknown_field", "data 未声明字段: %s", field)
		}
	}

	// 2. 必填与格式。
	if !ulidRe.MatchString(e.ID) {
		return Event{}, false, invalid("invalid_id", "id 必须是 26 位 ULID")
	}
	if e.DeviceID == "" {
		return Event{}, false, invalid("missing_field", "缺少 device_id")
	}
	if e.DeviceID != deviceID {
		return Event{}, false, invalid("device_mismatch", "payload device_id 与凭证不一致")
	}
	switch e.Type {
	case TypeWindowActivity, TypeIdleState, TypeGitActivity:
	default:
		return Event{}, false, invalid("unknown_type", "未知事件类型: %s", e.Type)
	}
	if e.Privacy != PrivacyP0 && e.Privacy != PrivacyP1 {
		return Event{}, false, invalid("invalid_privacy", "privacy 必须是 P0 或 P1")
	}

	ts, err := time.Parse(time.RFC3339, e.Timestamp)
	if err != nil {
		return Event{}, false, invalid("invalid_timestamp", "timestamp 必须是 RFC3339")
	}
	ts = ts.UTC()

	// 客户端时间超前超过容忍度时标记 clock_skew，由调用方使用 received_at 参与聚合。
	clockSkew := ts.After(now.Add(skewTolerance))

	// 3. 分类型校验，并检查敏感内容残留。
	ctxAny, dataAny, verr := buildContextAndData(e)
	if verr != nil {
		return Event{}, false, verr
	}

	switch e.Type {
	case TypeWindowActivity:
		if e.Privacy != PrivacyP0 {
			return Event{}, false, invalid("invalid_privacy", "window.activity 必须是 P0")
		}
		if _, ok := ctxAny["app"]; !ok {
			return Event{}, false, invalid("missing_field", "window.activity 缺少 context.app")
		}
		if _, ok := dataAny["duration_seconds"]; !ok {
			return Event{}, false, invalid("missing_field", "window.activity 缺少 data.duration_seconds")
		}
	case TypeIdleState:
		state, _ := dataAny["state"].(string)
		if state != "active" && state != "idle" && state != "locked" {
			return Event{}, false, invalid("invalid_state", "idle.state 的 state 必须是 active/idle/locked")
		}
		if dur, ok := dataAny["duration_seconds"]; ok {
			if f, ok := toNumber(dur); !ok || f < 0 {
				return Event{}, false, invalid("invalid_duration", "idle.state 的 duration_seconds 必须是非负数")
			}
		}
		if e.Privacy != PrivacyP0 {
			return Event{}, false, invalid("invalid_privacy", "idle.state 必须是 P0")
		}
	case TypeGitActivity:
		if _, ok := ctxAny["repo"]; !ok {
			return Event{}, false, invalid("missing_field", "git.activity 缺少 context.repo")
		}
		if e.Privacy != PrivacyP1 {
			return Event{}, false, invalid("invalid_privacy", "git.activity 必须是 P1")
		}
		if hc, ok := dataAny["head_commit"].(string); ok && hc != "" && !commitRe.MatchString(hc) {
			return Event{}, false, invalid("invalid_commit", "head_commit 必须是 7-40 位小写十六进制")
		}
		if kind, ok := dataAny["kind"].(string); !ok || (kind != "commit" && kind != "workspace") {
			return Event{}, false, invalid("invalid_kind", "git.activity 的 kind 必须是 commit 或 workspace")
		}
	}

	// 4. 敏感内容检测：路径、用户名、URL。命中即拒绝，不静默修改。
	for k, v := range ctxAny {
		s, _ := v.(string)
		if s == "" {
			continue
		}
		if userPathRe.MatchString(s) {
			return Event{}, false, invalid("sensitive_value", "context.%s 含绝对路径", k)
		}
		if urlRe.MatchString(s) {
			return Event{}, false, invalid("sensitive_value", "context.%s 含 URL", k)
		}
	}

	return Event{
		ID: e.ID, DeviceID: e.DeviceID, Type: e.Type, Timestamp: ts, Privacy: e.Privacy,
		Context: ctxAny, Data: dataAny,
	}, clockSkew, nil
}

// toNumber 把 JSON 解出的数值统一转成 float64。
func toNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func buildContextAndData(e RawEvent) (map[string]any, map[string]any, *ValidationError) {
	ctxAny, err := decodeMap(e.Context)
	if err != nil {
		return nil, nil, invalid("invalid_context", "context 解析失败: %v", err)
	}
	dataAny, err := decodeMap(e.Data)
	if err != nil {
		return nil, nil, invalid("invalid_data", "data 解析失败: %v", err)
	}
	return ctxAny, dataAny, nil
}

func decodeMap(raw map[string]json.RawMessage) (map[string]any, error) {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = val
	}
	return out, nil
}

// MarshalContext 与 MarshalData 生成存储用的 JSON 文本。
func MarshalContext(m map[string]any) string { return marshal(m) }
func MarshalData(m map[string]any) string    { return marshal(m) }

func marshal(m map[string]any) string {
	if m == nil {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}
