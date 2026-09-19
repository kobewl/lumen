package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/auth"
	"lumen/server/internal/config"
	"lumen/server/internal/events"
	"lumen/server/internal/feishu"
	"lumen/server/internal/initiative"
	"lumen/server/internal/notification"
	"lumen/server/internal/sessions"
	"lumen/server/internal/storage"
	"lumen/server/internal/summary"
	"lumen/server/internal/temporal"
	"lumen/server/internal/tooling"
	"lumen/server/internal/tools"
	"lumen/server/internal/ulid"
)

const (
	testEnrollmentToken = "test-enrollment-token"
	testAdminToken      = "test-admin-token"
	testDeviceName      = "desktop-mac-01"
)

var testLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

type testEnv struct {
	handler   http.Handler
	srv       *Server
	store     *storage.Store
	cfg       *config.Config
	deviceID  string
	devToken  string
	aiCalls   *int
	aiServer  *httptest.Server
	aiEnabled bool
	summary   *summary.Service
}

// newTestEnv 搭建一个完整的测试服务端。
// aiResponder 为 nil 时表示不启用 DeepSeek（此时 summarize 会跳过）。
func newTestEnv(t *testing.T, aiResponder func(w http.ResponseWriter, r *http.Request)) *testEnv {
	t.Helper()

	cfg := &config.Config{
		ListenAddr:         "127.0.0.1:0",
		DBPath:             filepath.Join(t.TempDir(), "test.db"),
		BackupDir:          t.TempDir(),
		Timezone:           "Asia/Shanghai",
		EnrollmentToken:    testEnrollmentToken,
		AdminToken:         testAdminToken,
		DeepSeekBaseURL:    "https://api.deepseek.com",
		DeepSeekModel:      "deepseek-chat",
		DeepSeekTimeout:    10 * time.Second,
		SummaryDailyLimit:  5,
		QueryDailyLimit:    20,
		DailyTokenLimit:    100000,
		SummaryHour:        22,
		SummaryMinute:      30,
		MaxBatchBytes:      512 * 1024,
		MaxEventBytes:      64 * 1024,
		MaxBatchEvents:     500,
		ClockSkewTolerance: 5 * time.Minute,
		LogLevel:           "error",
	}

	store, err := storage.Open(context.Background(), cfg.DBPath)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := sessions.NewEngine(store, testLoc)
	authService := auth.NewService(store, cfg.EnrollmentToken, 1)

	env := &testEnv{store: store, cfg: cfg}
	env.aiCalls = new(int)

	var aiClient *ai.Client
	if aiResponder != nil {
		env.aiEnabled = true
		env.aiServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*env.aiCalls++
			aiResponder(w, r)
		}))
		t.Cleanup(env.aiServer.Close)
		aiClient = ai.NewClient(env.aiServer.URL, "test-key", "deepseek-chat", 10*time.Second)
	} else {
		aiClient = ai.NewClient(cfg.DeepSeekBaseURL, "", "", 10*time.Second)
	}

	var sender notification.Sender
	summaryService := summary.NewService(store, engine, aiClient, sender, testLoc,
		cfg.SummaryDailyLimit, nil, logger)
	env.summary = summaryService
	env.srv = NewServer(cfg, store, authService, engine, summaryService, nil, logger)
	env.handler = env.srv.Handler()
	return env
}

// register 完成一次设备注册并保存凭证。
func (e *testEnv) register(t *testing.T) {
	t.Helper()
	body := map[string]string{"enrollment_token": testEnrollmentToken, "device_name": testDeviceName}
	rec := e.do(t, http.MethodPost, "/api/v1/devices/register", body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("注册失败: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析注册响应失败: %v", err)
	}
	if resp.DeviceID == "" || resp.DeviceToken == "" {
		t.Fatalf("注册响应缺少字段: %+v", resp)
	}
	e.deviceID, e.devToken = resp.DeviceID, resp.DeviceToken
}

func (e *testEnv) do(t *testing.T, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	switch v := body.(type) {
	case nil:
		reader = nil
	case []byte:
		reader = bytes.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化请求失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func sampleEvent(id string, at time.Time) map[string]any {
	return map[string]any{
		"id":        id,
		"device_id": testDeviceName,
		"type":      "window.activity",
		"timestamp": at.UTC().Format(time.RFC3339),
		"privacy":   "P0",
		"context":   map[string]any{"app": "Visual Studio Code", "project": "lumen"},
		"data":      map[string]any{"duration_seconds": 600},
	}
}

func batchBody(eventIDs []string, at time.Time) map[string]any {
	evs := make([]map[string]any, 0, len(eventIDs))
	for _, id := range eventIDs {
		evs = append(evs, sampleEvent(id, at))
	}
	return map[string]any{
		"device_id": testDeviceName,
		"batch_id":  ulid.New(),
		"sent_at":   time.Now().UTC().Format(time.RFC3339),
		"events":    evs,
	}
}

func parseBatchResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析批量响应失败: %v (%s)", err, rec.Body.String())
	}
	return resp
}

func statusCounts(t *testing.T, resp map[string]any) map[string]int {
	t.Helper()
	counts := map[string]int{}
	list, _ := resp["results"].([]any)
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		status, _ := m["status"].(string)
		counts[status]++
	}
	return counts
}

func TestRegisterRequiresValidEnrollmentToken(t *testing.T) {
	env := newTestEnv(t, nil)

	rec := env.do(t, http.MethodPost, "/api/v1/devices/register", map[string]string{
		"enrollment_token": "wrong-token", "device_name": testDeviceName,
	}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误令牌应返回 401，实际 %d", rec.Code)
	}

	env.register(t)

	// 单用户单设备：第二次注册应被拒绝。
	rec = env.do(t, http.MethodPost, "/api/v1/devices/register", map[string]string{
		"enrollment_token": testEnrollmentToken, "device_name": "another-mac",
	}, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("超过设备上限应返回 409，实际 %d", rec.Code)
	}
}

func TestBatchRequiresDeviceToken(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t)

	rec := env.do(t, http.MethodPost, "/api/v1/events/batch", batchBody([]string{ulid.New()}, time.Now()), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺少凭证应返回 401，实际 %d", rec.Code)
	}

	rec = env.do(t, http.MethodPost, "/api/v1/events/batch", batchBody([]string{ulid.New()}, time.Now()), "invalid-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无效凭证应返回 401，实际 %d", rec.Code)
	}
}

// TestBatchIsIdempotent 验证“同一批上报 3 次，服务端每个 event_id 仍只有一条”。
func TestBatchIsIdempotent(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t)

	at := time.Now().UTC().Add(-time.Hour)
	ids := []string{ulid.New(), ulid.New(), ulid.New()}

	first := env.do(t, http.MethodPost, "/api/v1/events/batch", batchBody(ids, at), env.devToken)
	if first.Code != http.StatusOK {
		t.Fatalf("首次上报失败: %d %s", first.Code, first.Body.String())
	}
	if c := statusCounts(t, parseBatchResponse(t, first)); c["accepted"] != 3 {
		t.Fatalf("首次上报应全部 accepted，实际 %+v", c)
	}

	for i := 2; i <= 3; i++ {
		rec := env.do(t, http.MethodPost, "/api/v1/events/batch", batchBody(ids, at), env.devToken)
		counts := statusCounts(t, parseBatchResponse(t, rec))
		if counts["duplicate"] != 3 || counts["accepted"] != 0 {
			t.Fatalf("第 %d 次上报应全部 duplicate，实际 %+v", i, counts)
		}
	}

	n, err := env.store.CountEvents(context.Background())
	if err != nil {
		t.Fatalf("统计事件失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("重复上报后事件数应为 3，实际 %d", n)
	}
}

// TestPartialRejection 验证“单条被拒绝时，其余事件仍可确认成功”。
func TestPartialRejection(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t)

	at := time.Now().UTC().Add(-time.Hour)
	goodID := ulid.New()
	badEvent := sampleEvent(ulid.New(), at)
	badEvent["clipboard"] = "copied secret"

	body := map[string]any{
		"device_id": testDeviceName,
		"batch_id":  ulid.New(),
		"sent_at":   time.Now().UTC().Format(time.RFC3339),
		"events":    []any{sampleEvent(goodID, at), badEvent},
	}
	rec := env.do(t, http.MethodPost, "/api/v1/events/batch", body, env.devToken)
	counts := statusCounts(t, parseBatchResponse(t, rec))
	if counts["accepted"] != 1 || counts["rejected"] != 1 {
		t.Fatalf("应 1 accepted + 1 rejected，实际 %+v", counts)
	}

	n, _ := env.store.CountEvents(context.Background())
	if n != 1 {
		t.Fatalf("被拒绝的事件不应入库，事件数应为 1，实际 %d", n)
	}
}

// TestForbiddenFieldsRejected 覆盖隐私验收：剪贴板、截图、源代码、绝对路径都必须被拒。
func TestForbiddenFieldsRejected(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t)

	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"clipboard", func(e map[string]any) { e["clipboard"] = "secret" }},
		{"screenshot", func(e map[string]any) { e["screenshot"] = "base64" }},
		{"source_code_in_data", func(e map[string]any) { e["data"].(map[string]any)["source_code"] = "print(1)" }},
		{"window_title_in_context", func(e map[string]any) { e["context"].(map[string]any)["window_title"] = "Secret Project" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at := time.Now().UTC().Add(-time.Hour)
			ev := sampleEvent(ulid.New(), at)
			c.mutate(ev)
			body := map[string]any{
				"device_id": testDeviceName, "batch_id": ulid.New(),
				"sent_at": time.Now().UTC().Format(time.RFC3339), "events": []any{ev},
			}
			rec := env.do(t, http.MethodPost, "/api/v1/events/batch", body, env.devToken)
			counts := statusCounts(t, parseBatchResponse(t, rec))
			if counts["rejected"] != 1 {
				t.Fatalf("禁用字段 %s 应被拒绝，实际 %+v", c.name, counts)
			}
		})
	}
}

func TestBatchRejectsDeviceMismatch(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t)

	body := batchBody([]string{ulid.New()}, time.Now())
	body["device_id"] = "other-device"
	rec := env.do(t, http.MethodPost, "/api/v1/events/batch", body, env.devToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("设备不一致应返回 403，实际 %d", rec.Code)
	}
}

func TestSessionsEndpointRequiresAuthAndReturnsData(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t)

	// 用今天的时间戳写入两段同项目活动，间隔 3 分钟，应合并成一个 Session。
	now := time.Now().In(testLoc)
	base := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, testLoc)
	ids := []string{ulid.New(), ulid.New()}
	body := map[string]any{
		"device_id": testDeviceName, "batch_id": ulid.New(),
		"sent_at": time.Now().UTC().Format(time.RFC3339),
		"events": []any{
			sampleEvent(ids[0], base),
			sampleEvent(ids[1], base.Add(13*time.Minute)),
		},
	}
	rec := env.do(t, http.MethodPost, "/api/v1/events/batch", body, env.devToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("上报失败: %d %s", rec.Code, rec.Body.String())
	}

	// 未鉴权访问应被拒绝。
	rec = env.do(t, http.MethodGet, "/api/v1/sessions?date="+base.Format("2006-01-02"), nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权查询应返回 401，实际 %d", rec.Code)
	}

	rec = env.do(t, http.MethodGet, "/api/v1/sessions?date="+base.Format("2006-01-02"), nil, env.devToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询失败: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Date     string           `json:"date"`
		Count    int              `json:"count"`
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析查询结果失败: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("3 分钟间隔应合并为 1 个 Session，实际 %d", resp.Count)
	}
	if resp.Sessions[0]["project"] != "lumen" {
		t.Fatalf("项目归属错误: %v", resp.Sessions[0]["project"])
	}
}

func TestSessionsRejectsBadDate(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t)

	rec := env.do(t, http.MethodGet, "/api/v1/sessions?date=2026-13-99", nil, env.devToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法日期应返回 400，实际 %d", rec.Code)
	}
}

func TestHealthzDoesNotLeakSecrets(t *testing.T) {
	env := newTestEnv(t, nil)
	env.cfg.DeepSeekAPIKey = "sk-super-secret-value"
	env.cfg.FeishuAppSecret = "feishu-secret-value"

	rec := env.do(t, http.MethodGet, "/api/v1/healthz", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("健康检查失败: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"sk-super-secret-value", "feishu-secret-value", testEnrollmentToken} {
		if bytes.Contains([]byte(body), []byte(secret)) {
			t.Fatalf("健康检查泄露了敏感值: %s", secret)
		}
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析健康检查失败: %v", err)
	}
	if resp["version"] == nil || resp["db"] != "ok" {
		t.Fatalf("健康检查缺少必要字段: %+v", resp)
	}
}

func TestGenerateSummaryRequiresAdminToken(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t)

	rec := env.do(t, http.MethodPost, "/api/v1/summaries/daily/generate", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺少管理令牌应返回 401，实际 %d", rec.Code)
	}

	rec = env.do(t, http.MethodPost, "/api/v1/summaries/daily/generate", nil, env.devToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("设备令牌不能用于管理接口，实际 %d", rec.Code)
	}
}

// TestGenerateSummaryEndToEnd 用一个假的 DeepSeek 服务验证总结全链路。
func TestGenerateSummaryEndToEnd(t *testing.T) {
	day := time.Now().In(testLoc).Format("2006-01-02")

	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		// 检查请求体：不允许包含路径、token 等敏感内容。
		raw, _ := io.ReadAll(r.Body)
		for _, bad := range []string{"/Users/", "device_token", "sk-", "clipboard"} {
			if bytes.Contains(raw, []byte(bad)) {
				t.Errorf("发往模型的请求包含敏感内容 %q", bad)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-1", "model": "deepseek-chat",
			"choices": []map[string]any{{
				"message":       map[string]any{"content": `{"date":"` + day + `","headline":"推进 Lumen 服务端","projects":[{"name":"lumen","duration_minutes":20,"activities":["实现 Session 引擎"],"evidence_session_ids":["PLACEHOLDER"]}],"uncertainties":[]}`},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 120, "completion_tokens": 60, "total_tokens": 180},
		})
	})
	env.register(t)

	// 写入一段今天的活动，供总结使用。
	now := time.Now().In(testLoc)
	base := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, testLoc)
	rec := env.do(t, http.MethodPost, "/api/v1/events/batch",
		batchBody([]string{ulid.New()}, base), env.devToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("上报失败: %d %s", rec.Code, rec.Body.String())
	}

	// 取真实 session id，替换 mock 响应里的占位符。
	sessionList, err := env.store.SessionsByDate(context.Background(), day)
	if err != nil || len(sessionList) == 0 {
		t.Fatalf("未生成 Session: %v (%d 个)", err, len(sessionList))
	}

	// mock 服务需要返回真实 session id，这里重写响应内容。
	env.aiServer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*env.aiCalls++
		w.Header().Set("Content-Type", "application/json")
		body := fmt.Sprintf(`{"id":"resp-2","model":"deepseek-chat","choices":[{"message":{"content":"{\"date\":\"%s\",\"headline\":\"推进 Lumen 服务端\",\"projects\":[{\"name\":\"lumen\",\"duration_minutes\":10,\"activities\":[\"实现 Session 引擎\"],\"evidence_session_ids\":[\"%s\"]}],\"uncertainties\":[]}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":60,"total_tokens":180}}`,
			day, sessionList[0].ID)
		_, _ = w.Write([]byte(body))
	})

	rec = env.do(t, http.MethodPost, "/api/v1/summaries/daily/generate?date="+day, nil, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("生成总结失败: %d %s", rec.Code, rec.Body.String())
	}
	var gen struct {
		Status string `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gen); err != nil {
		t.Fatalf("解析生成结果失败: %v", err)
	}
	if gen.Status != storage.SummaryOK {
		t.Fatalf("总结状态应为 succeeded，实际 %s", gen.Status)
	}
	if gen.Text == "" {
		t.Fatal("总结文本不应为空")
	}

	// 再次生成应复用已有成功结果，不重复调用模型。
	before := *env.aiCalls
	rec = env.do(t, http.MethodPost, "/api/v1/summaries/daily/generate?date="+day, nil, testAdminToken)
	if *env.aiCalls != before {
		t.Fatalf("输入未变化时不应重复调用模型，调用次数从 %d 变为 %d", before, *env.aiCalls)
	}

	// 查询接口应返回同一份总结。
	rec = env.do(t, http.MethodGet, "/api/v1/summaries/daily?date="+day, nil, env.devToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询总结失败: %d", rec.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["found"] != true || got["status"] != "succeeded" {
		t.Fatalf("查询结果不符: %+v", got)
	}
}

// TestSummaryFailsOnInvalidModelOutput 验证模型输出非法时保存失败状态，不写入虚假成功结果。
func TestSummaryFailsOnInvalidModelOutput(t *testing.T) {
	day := time.Now().In(testLoc).Format("2006-01-02")

	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-3", "model": "deepseek-chat",
			"choices": []map[string]any{{
				"message":       map[string]any{"content": "这不是 JSON"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	})
	env.register(t)

	now := time.Now().In(testLoc)
	base := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, testLoc)
	_ = env.do(t, http.MethodPost, "/api/v1/events/batch", batchBody([]string{ulid.New()}, base), env.devToken)

	rec := env.do(t, http.MethodPost, "/api/v1/summaries/daily/generate?date="+day, nil, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("请求应被接受: %d %s", rec.Code, rec.Body.String())
	}
	var gen struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &gen)
	if gen.Status != storage.SummaryFailed {
		t.Fatalf("非法输出应标记为 failed，实际 %s", gen.Status)
	}

	// 失败记录不应带出渲染文本。
	record, ok, err := env.store.SummaryByDate(context.Background(), day)
	if err != nil || !ok {
		t.Fatalf("应存在失败记录: %v", err)
	}
	if record.RenderedText != "" {
		t.Fatal("失败记录不应有渲染文本")
	}
	if record.ErrorCode == "" {
		t.Fatal("失败记录应保存错误码")
	}
}

// TestExistingSummaryRedeliversWithoutSpendingBudget 是回归测试。
//
// 场景：总结已生成成功，但飞书推送失败（或当时飞书还没配好）。
// 用户再次触发时应重发通知，而**不是**因为"当日预算用完"直接跳过。
// 之前这里会返回 budget_exhausted，导致已生成的总结永远补发不出去。
func TestExistingSummaryRedeliversWithoutSpendingBudget(t *testing.T) {
	day := time.Now().In(testLoc).Format("2006-01-02")

	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp", "model": "deepseek-chat",
			"choices": []map[string]any{{
				"message":       map[string]any{"content": `{"date":"` + day + `","headline":"推进进度","projects":[],"uncertainties":[]}`},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		})
	})
	env.register(t)

	// 注入假通知渠道，记录投递次数。
	var delivered []string
	env.summary.SetSenderForTest(notification.SenderFunc{
		NameFunc:    func() string { return "fake" },
		EnabledFunc: func() bool { return true },
		SendFunc: func(_ context.Context, _, text string) error {
			delivered = append(delivered, text)
			return nil
		},
	}, []string{"ou_test_user"})

	now := time.Now().In(testLoc)
	base := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, testLoc)
	if rec := env.do(t, http.MethodPost, "/api/v1/events/batch",
		batchBody([]string{ulid.New()}, base), env.devToken); rec.Code != http.StatusOK {
		t.Fatalf("上报失败: %d", rec.Code)
	}

	// 首次生成：应调用模型并推送。
	if rec := env.do(t, http.MethodPost, "/api/v1/summaries/daily/generate?date="+day, nil, testAdminToken); rec.Code != http.StatusOK {
		t.Fatalf("首次生成失败: %d %s", rec.Code, rec.Body.String())
	}
	if len(delivered) != 1 {
		t.Fatalf("首次生成应推送 1 次，实际 %d 次", len(delivered))
	}
	callsAfterFirst := *env.aiCalls

	// 故意把当日预算耗尽，模拟"今天已经调用到上限"。
	for i := 0; i < env.cfg.SummaryDailyLimit+1; i++ {
		_ = env.store.AddAIUsage(context.Background(), day, "summary", 1, 1)
	}

	rec := env.do(t, http.MethodPost, "/api/v1/summaries/daily/generate?date="+day, nil, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("补发请求应成功: %d %s", rec.Code, rec.Body.String())
	}
	var gen struct {
		Status  string `json:"status"`
		Skipped string `json:"skipped"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &gen)

	if gen.Status == "skipped" && gen.Skipped == "budget_exhausted" {
		t.Fatal("已有成功总结时应可补发，而不是因预算跳过")
	}
	if gen.Status != "already_succeeded" {
		t.Fatalf("状态应为 already_succeeded，实际 %s (skipped=%s)", gen.Status, gen.Skipped)
	}
	// 已经成功投递过的 summary 不应重复推送（delivery 去重）。
	if len(delivered) != 1 {
		t.Fatalf("已成功推送的总结不应重复发送，实际投递 %d 次", len(delivered))
	}
	if *env.aiCalls != callsAfterFirst {
		t.Fatalf("不应再次调用模型，调用次数从 %d 变为 %d", callsAfterFirst, *env.aiCalls)
	}
}

// TestFailedDeliveryIsRetriedOnNextTrigger 验证推送失败后再次触发会真正重发。
func TestFailedDeliveryIsRetriedOnNextTrigger(t *testing.T) {
	day := time.Now().In(testLoc).Format("2006-01-02")

	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp", "model": "deepseek-chat",
			"choices": []map[string]any{{
				"message":       map[string]any{"content": `{"date":"` + day + `","headline":"进度","projects":[],"uncertainties":[]}`},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		})
	})
	env.register(t)

	var attempts int
	failing := true
	env.summary.SetSenderForTest(notification.SenderFunc{
		NameFunc:    func() string { return "fake" },
		EnabledFunc: func() bool { return true },
		SendFunc: func(_ context.Context, _, _ string) error {
			attempts++
			if failing {
				return errors.New("模拟飞书不可用")
			}
			return nil
		},
	}, []string{"ou_test_user"})

	now := time.Now().In(testLoc)
	base := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, testLoc)
	_ = env.do(t, http.MethodPost, "/api/v1/events/batch", batchBody([]string{ulid.New()}, base), env.devToken)

	// 第一次：生成成功，但推送失败。
	_ = env.do(t, http.MethodPost, "/api/v1/summaries/daily/generate?date="+day, nil, testAdminToken)
	if attempts != 1 {
		t.Fatalf("应尝试推送 1 次，实际 %d", attempts)
	}

	// 飞书恢复后再次触发：应当重发。
	failing = false
	_ = env.do(t, http.MethodPost, "/api/v1/summaries/daily/generate?date="+day, nil, testAdminToken)
	if attempts != 2 {
		t.Fatalf("推送失败后再次触发应重发，期望 2 次尝试，实际 %d", attempts)
	}

	// 已成功投递后，再触发不应重复发送。
	_ = env.do(t, http.MethodPost, "/api/v1/summaries/daily/generate?date="+day, nil, testAdminToken)
	if attempts != 2 {
		t.Fatalf("成功投递后不应重复发送，实际 %d 次", attempts)
	}
}

func TestSummarySkippedWithoutSessions(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("没有 Session 时不应调用模型")
		w.WriteHeader(http.StatusInternalServerError)
	})
	env.register(t)

	rec := env.do(t, http.MethodPost, "/api/v1/summaries/daily/generate?date=2020-01-01", nil, testAdminToken)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("无数据时应返回 202，实际 %d %s", rec.Code, rec.Body.String())
	}
	var gen struct {
		Skipped string `json:"skipped"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &gen)
	if gen.Skipped != "no_sessions" {
		t.Fatalf("应跳过并说明原因，实际 %q", gen.Skipped)
	}
	if *env.aiCalls != 0 {
		t.Fatalf("不应调用模型，实际调用 %d 次", *env.aiCalls)
	}
}

func TestOversizedBatchRejected(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t)

	// 构造一个超过单批上限的请求体。
	big := bytes.Repeat([]byte("a"), int(env.cfg.MaxBatchBytes)+1024)
	rec := env.do(t, http.MethodPost, "/api/v1/events/batch", big, env.devToken)
	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Fatalf("超大请求应被拒绝，实际 %d", rec.Code)
	}
}

func TestEventsSchemaMatchesProtocolTypes(t *testing.T) {
	// 事件类型常量必须与协议 schema 中的 enum 一致。
	want := []string{"window.activity", "idle.state", "git.activity"}
	got := []string{events.TypeWindowActivity, events.TypeIdleState, events.TypeGitActivity}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("类型不匹配: want %s got %s", want[i], got[i])
		}
	}
}

// TestRebuildSessionsEndpoint 覆盖历史脏数据的重算入口。
//
// 场景：数据是旧规则写下的——锁屏期间的 loginwindow 假活动被算成了工作。
// 重算后这些假活动必须被忽略，原始事件保持不变（不删数据）。
func TestRebuildSessionsEndpoint(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t)

	day := time.Now().In(testLoc).Format("2006-01-02")
	base := time.Date(time.Now().In(testLoc).Year(), time.Now().In(testLoc).Month(),
		time.Now().In(testLoc).Day(), 9, 0, 0, 0, testLoc)

	events := []any{
		sampleEvent(ulid.New(), base),
		// 锁屏期间由 loginwindow 产生的假活动（旧数据）。
		map[string]any{
			"id": ulid.New(), "device_id": testDeviceName, "type": "window.activity",
			"timestamp": base.Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"privacy":   "P0",
			"context":   map[string]any{"app": "loginwindow", "bundle_id": "com.apple.loginwindow"},
			"data":      map[string]any{"duration_seconds": 3600},
		},
	}
	body := map[string]any{
		"device_id": testDeviceName, "batch_id": ulid.New(),
		"sent_at": time.Now().UTC().Format(time.RFC3339), "events": events,
	}
	if rec := env.do(t, http.MethodPost, "/api/v1/events/batch", body, env.devToken); rec.Code != http.StatusOK {
		t.Fatalf("上报失败: %d %s", rec.Code, rec.Body.String())
	}

	before, _ := env.store.CountEvents(context.Background())

	// 缺管理令牌时必须拒绝。
	rec := env.do(t, http.MethodPost, "/api/v1/sessions/rebuild?date="+day, nil, env.devToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("重算接口应要求管理令牌，实际 %d", rec.Code)
	}

	rec = env.do(t, http.MethodPost, "/api/v1/sessions/rebuild?date="+day, nil, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("重算失败: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Algorithm string           `json:"algorithm"`
		Days      []map[string]any `json:"days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析重算响应失败: %v", err)
	}
	if resp.Algorithm != sessions.AlgorithmVersion {
		t.Fatalf("重算响应应带算法版本，实际 %q", resp.Algorithm)
	}
	if len(resp.Days) != 1 {
		t.Fatalf("应重算 1 天，实际 %d 天", len(resp.Days))
	}

	list, err := env.store.SessionsByDate(context.Background(), day)
	if err != nil {
		t.Fatalf("查询 Session 失败: %v", err)
	}
	for _, s := range list {
		if bytes.Contains([]byte(s.AppsJSON), []byte("loginwindow")) {
			t.Fatalf("重算后不应保留登录窗口的假活动: %s", s.AppsJSON)
		}
	}

	// 原始事件不能被删除。
	after, _ := env.store.CountEvents(context.Background())
	if after != before {
		t.Fatalf("重算不应删除原始事件：%d -> %d", before, after)
	}

	// 重算幂等：连跑两次结果一致。
	rec = env.do(t, http.MethodPost, "/api/v1/sessions/rebuild?date="+day, nil, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复重算失败: %d", rec.Code)
	}
	again, _ := env.store.SessionsByDate(context.Background(), day)
	if len(again) != len(list) {
		t.Fatalf("重复重算不应改变 Session 数量：%d -> %d", len(list), len(again))
	}

	// 非法参数。
	if rec := env.do(t, http.MethodPost, "/api/v1/sessions/rebuild?date=2026-13-99", nil, testAdminToken); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法日期应返回 400，实际 %d", rec.Code)
	}
	if rec := env.do(t, http.MethodPost, "/api/v1/sessions/rebuild", nil, testAdminToken); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少参数应返回 400，实际 %d", rec.Code)
	}
}

// ---- 工具层只读接口 ----

// newToolEnv 搭一个带工具问答服务的测试服务端（无外部模型）。
func newToolEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t, nil)
	cfg := env.cfg
	svc := feishu.NewQAService(feishu.QAServiceOptions{
		Store:           env.store,
		Loc:             testLoc,
		Profile:         cfg.Profile(),
		QueryDailyLimit: cfg.QueryDailyLimit,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit:           storage.ToolAuditSink{Store: env.store},
	})
	authService := auth.NewService(env.store, cfg.EnrollmentToken, 1)
	env.handler = NewServer(cfg, env.store, authService, sessions.NewEngine(env.store, testLoc),
		env.summary, svc, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
	return env
}

// TestToolsEndpointRequiresAdmin 覆盖工具目录接口的鉴权。
func TestToolsEndpointRequiresAdmin(t *testing.T) {
	env := newToolEnv(t)

	rec := env.do(t, http.MethodGet, "/api/v1/tools", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺少令牌应返回 401，实际 %d", rec.Code)
	}
	env.register(t)
	rec = env.do(t, http.MethodGet, "/api/v1/tools", nil, env.devToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("设备令牌不能看工具目录，实际 %d", rec.Code)
	}
}

// TestToolsEndpointReturnsGovernanceMetadata 覆盖工具目录的内容。
//
// 这条接口的意义是"不读代码就能确认能力边界"：每个工具必须带上
// 风险级别、参数 schema、结果 schema 与是否需要来源证据。
func TestToolsEndpointReturnsGovernanceMetadata(t *testing.T) {
	env := newToolEnv(t)

	rec := env.do(t, http.MethodGet, "/api/v1/tools", nil, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询失败: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count int `json:"count"`
		Tools []struct {
			Name             string           `json:"name"`
			Summary          string           `json:"summary"`
			Risk             string           `json:"risk"`
			RiskLabel        string           `json:"risk_label"`
			Kind             string           `json:"kind"`
			RequiresEvidence bool             `json:"requires_evidence"`
			Parameters       []map[string]any `json:"parameters"`
			ResultSchema     string           `json:"result_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Count != 8 {
		t.Fatalf("应返回 8 个工具，实际 %d", resp.Count)
	}

	byName := map[string]int{}
	for i, tool := range resp.Tools {
		byName[tool.Name] = i
		if tool.Summary == "" || tool.ResultSchema == "" || tool.RiskLabel == "" {
			t.Fatalf("工具 %s 缺少治理元信息: %+v", tool.Name, tool)
		}
	}

	// 唯一的写工具必须标成低风险写入并要求来源证据。
	memIdx, ok := byName["save_memory_candidate"]
	if !ok {
		t.Fatal("工具目录应包含 save_memory_candidate")
	}
	mem := resp.Tools[memIdx]
	if mem.Risk != "write_low" || !mem.RequiresEvidence {
		t.Fatalf("写工具应是低风险且要求来源，实际 %+v", mem)
	}
	// 它不能接受 source_ids 参数：来源只能由代码注入。
	for _, p := range mem.Parameters {
		if p["name"] == "source_ids" {
			t.Fatal("写工具不应接受 source_ids 参数：来源必须由代码注入")
		}
	}
	// 只读工具不应要求来源。
	if idx, ok := byName["get_today_status"]; ok {
		if resp.Tools[idx].RequiresEvidence {
			t.Fatal("只读工具不应要求来源证据")
		}
	}
}

// TestToolAuditsEndpointReturnsRecords 覆盖审计接口。
func TestToolAuditsEndpointReturnsRecords(t *testing.T) {
	env := newToolEnv(t)

	// 手工写两条审计：放行与拒绝各一条。
	sink := storage.ToolAuditSink{Store: env.store}
	ctx := context.Background()
	if err := sink.Record(ctx, tooling.AuditRecord{
		Actor: "u1", Tool: "get_today_status", Risk: tooling.RiskRead,
		Args: map[string]any{"date": "2026-09-18"}, Decision: tooling.DecisionAllowed,
		ResultKind: tooling.KindActivity, Count: 2, EvidenceN: 2, DurationMS: 3,
	}); err != nil {
		t.Fatalf("写入审计失败: %v", err)
	}
	if err := sink.Record(ctx, tooling.AuditRecord{
		Actor: "u1", Tool: "run_sql", Decision: tooling.DecisionDenied,
		Reason: "未知工具", DurationMS: 0,
	}); err != nil {
		t.Fatalf("写入审计失败: %v", err)
	}

	// 鉴权。
	if rec := env.do(t, http.MethodGet, "/api/v1/tool-audits", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺少令牌应返回 401，实际 %d", rec.Code)
	}

	rec := env.do(t, http.MethodGet, "/api/v1/tool-audits", nil, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询失败: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count  int `json:"count"`
		Audits []struct {
			Tool       string         `json:"tool"`
			Decision   string         `json:"decision"`
			Reason     string         `json:"reason"`
			Actor      string         `json:"actor"`
			Args       map[string]any `json:"args"`
			ResultKind string         `json:"result_kind"`
			ItemCount  int            `json:"item_count"`
			EvidenceN  int            `json:"evidence_n"`
		} `json:"audits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Count != 2 {
		t.Fatalf("应返回 2 条审计，实际 %d", resp.Count)
	}
	// 最新的在前：拒绝的那条应当是第一条。
	if resp.Audits[0].Tool != "run_sql" || resp.Audits[0].Decision != tooling.DecisionDenied {
		t.Fatalf("应按时间倒序返回，实际 %+v", resp.Audits[0])
	}
	if resp.Audits[0].Reason == "" {
		t.Fatal("拒绝记录必须带原因")
	}
	if resp.Audits[1].Actor != "u1" || resp.Audits[1].ItemCount != 2 || resp.Audits[1].EvidenceN != 2 {
		t.Fatalf("放行记录内容不符: %+v", resp.Audits[1])
	}
}

// TestToolAuditsEndpointCapsArgumentLength 覆盖"审计里的参数被裁短"这条端到端保证。
//
// 审计要长期保存，不能因为某个参数很长就把大段用户内容写进去。
// 断言链路是完整的：执行器裁剪 → 存储落库 → 接口返回。
func TestToolAuditsEndpointCapsArgumentLength(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	registry, err := tooling.NewRegistry(auditProbeTool{})
	if err != nil {
		t.Fatalf("构造注册表失败: %v", err)
	}
	exec, err := tooling.NewExecutor(tooling.ExecutorOptions{
		Registry: registry, Audit: storage.ToolAuditSink{Store: store},
	})
	if err != nil {
		t.Fatalf("构造执行器失败: %v", err)
	}

	long := strings.Repeat("项", 200)
	info := tooling.RunInfo{Actor: "u1",
		Temporal: temporal.Build(temporal.FixedClock(time.Date(2026, 9, 18, 13, 19, 0, 0, time.UTC)), testLoc)}
	if _, denied := exec.Run(context.Background(), []tooling.Call{{
		Name: "get_sessions", Arguments: map[string]any{"project": long},
	}}, info); len(denied) != 0 {
		t.Fatalf("调用应被放行，实际 %+v", denied)
	}

	records, err := store.RecentToolAudits(context.Background(), 10)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("应有 1 条审计，实际 %d", len(records))
	}
	got, _ := records[0].Args["project"].(string)
	if len([]rune(got)) > 70 {
		t.Fatalf("审计里的参数应被裁短到 64 字左右，实际 %d 字", len([]rune(got)))
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("裁短处应显式标注，实际 %q", got)
	}
}

// auditProbeTool 是只用于审计测试的最小工具。
type auditProbeTool struct{}

func (auditProbeTool) Spec() tooling.Spec {
	return tooling.Spec{
		Name: "get_sessions", Summary: "测试用只读工具",
		ResultSchema: `{"sessions":[]}`, Kind: tooling.KindActivity, Risk: tooling.RiskRead,
		Parameters: []tooling.Param{
			{Name: "project", Type: tooling.ParamString, MaxLength: 200, Description: "项目名"},
		},
	}
}

func (auditProbeTool) Execute(context.Context, tooling.Invocation) (tooling.Result, error) {
	return tooling.Result{Count: 0}, nil
}

// ---- 主动关怀端点 ----

// fakeInitiativePlanner 是 API 集成测试用的假提案模型（initiative.Planner）。
type fakeInitiativePlanner struct {
	proposal initiative.Proposal
	err      error
}

func (f *fakeInitiativePlanner) Propose(context.Context, initiative.ProposalRequest) (initiative.Proposal, error) {
	if f.err != nil {
		return initiative.Proposal{}, f.err
	}
	return f.proposal, nil
}

// newInitiativeEnv 搭一个启用主动关怀（dry-run）的测试服务端，并灌入今天的记录。
func newInitiativeEnv(t *testing.T, planner initiative.Planner) *testEnv {
	t.Helper()
	env := newTestEnv(t, nil)

	// 种子：今天一段工作 + 一条 Agent 任务摘要（保证有可依据的事实）。
	now := time.Now().In(testLoc)
	date := now.Format("2006-01-02")
	apps, _ := json.Marshal([]map[string]any{{"app": "ZCode", "duration_minutes": 90}})
	stats, _ := json.Marshal(map[string]any{"duration_minutes": 90})
	start := now.Add(-2 * time.Hour)
	sess := storage.Session{
		ID: "s_api_init", Date: date, Project: "lumen", StartAt: start,
		EndAt: start.Add(90 * time.Minute), AppsJSON: string(apps), StatsJSON: string(stats),
		AlgorithmVersion: "rules-v2", SourceStartAt: start, SourceEndAt: start.Add(24 * time.Hour),
		UpdatedAt: time.Now().UTC(),
	}
	from := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, testLoc).UTC()
	if err := env.store.ReplaceSessions(context.Background(), date, from, from.Add(24*time.Hour),
		[]storage.Session{sess}); err != nil {
		t.Fatalf("写入 Session 失败: %v", err)
	}
	if _, err := env.store.UpsertTaskSummary(context.Background(), storage.TaskSummary{
		ID: "evt-1", DeviceID: "dev-1", TaskID: "t_api_init", Project: "lumen",
		Title: "接通主动关怀", Status: "done", SourceAgent: "zcode-cli",
		OccurredAt: now.Add(-time.Hour), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("写入任务摘要失败: %v", err)
	}

	registry, err := tooling.NewRegistry(tools.All(tools.Options{
		Store: env.store, Profile: env.cfg.Profile(), Location: testLoc,
	})...)
	if err != nil {
		t.Fatalf("构造注册表失败: %v", err)
	}
	executor, err := tooling.NewExecutor(tooling.ExecutorOptions{Registry: registry})
	if err != nil {
		t.Fatalf("构造执行器失败: %v", err)
	}
	policy := initiative.DefaultPolicy()
	policy.Enabled = true
	svc := initiative.New(initiative.Options{
		Store: env.store, Executor: executor, Planner: planner, Profile: env.cfg.Profile(),
		Loc: testLoc, Targets: []string{"ou_api_target"}, DryRun: true, Policy: policy,
	})
	env.srv.SetInitiative(svc)
	return env
}

// TestInitiativeRunEndpointDryRun 覆盖验收 P1-5 的端点层：
// dry-run 走完整链路，写 outbox，但不真实发送。
func TestInitiativeRunEndpointDryRun(t *testing.T) {
	planner := &fakeInitiativePlanner{proposal: initiative.Proposal{
		Question: "上午在 lumen 上花了不少时间，进展还顺利吗？",
		Basis:    []string{"t_api_init", "get_today_status"},
	}}
	env := newInitiativeEnv(t, planner)

	// 鉴权。
	if rec := env.do(t, http.MethodPost, "/api/v1/initiative/run", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺少令牌应返回 401，实际 %d", rec.Code)
	}

	rec := env.do(t, http.MethodPost, "/api/v1/initiative/run", nil, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("触发失败: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Action    string   `json:"action"`
		Reason    string   `json:"reason"`
		Text      string   `json:"text"`
		OutboxIDs []string `json:"outbox_ids"`
		Forced    bool     `json:"forced"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Action != "dry_run" {
		t.Fatalf("默认应为 dry_run（默认不真实发送），实际 %s (%s)", resp.Action, resp.Reason)
	}
	if len(resp.OutboxIDs) != 1 || resp.Text == "" {
		t.Fatalf("应写 outbox 并带问题全文，实际 %+v", resp)
	}

	// outbox 查询端点可见，状态 dry_run，依据可追溯。
	rec = env.do(t, http.MethodGet, "/api/v1/initiative/outbox", nil, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询失败: %d", rec.Code)
	}
	var list struct {
		Count  int `json:"count"`
		Outbox []struct {
			Status   string   `json:"status"`
			Text     string   `json:"text"`
			Basis    []string `json:"basis"`
			Evidence []string `json:"evidence"`
		} `json:"outbox"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if list.Count != 1 || list.Outbox[0].Status != "dry_run" {
		t.Fatalf("应返回 1 条 dry_run 记录，实际 %+v", list)
	}
	if len(list.Outbox[0].Basis) != 2 {
		t.Fatalf("依据应落库，实际 %v", list.Outbox[0].Basis)
	}

	// 频率：第二次不带 force 应被间隔拦住（dry_run 也计入）。
	rec = env.do(t, http.MethodPost, "/api/v1/initiative/run", nil, testAdminToken)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Action != "skipped" || !strings.Contains(resp.Reason, "间隔") {
		t.Fatalf("第二次应因间隔跳过，实际 %s (%s)", resp.Action, resp.Reason)
	}

	// force 跳过频率限制（本地验收用），仍写审计。
	rec = env.do(t, http.MethodPost, "/api/v1/initiative/run?force=true", nil, testAdminToken)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Action != "dry_run" || !resp.Forced {
		t.Fatalf("force 应放行，实际 %s (%s)", resp.Action, resp.Reason)
	}
}

// TestInitiativeDisabledEndpoint 覆盖开关关闭时端点诚实返回。
func TestInitiativeDisabledEndpoint(t *testing.T) {
	env := newTestEnv(t, nil)
	// 未调用 SetInitiative：服务未启用。
	rec := env.do(t, http.MethodPost, "/api/v1/initiative/run", nil, testAdminToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未启用应返回 503，实际 %d", rec.Code)
	}
	rec = env.do(t, http.MethodGet, "/api/v1/initiative/outbox", nil, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("账本查询仍应可用（空列表），实际 %d", rec.Code)
	}
}
