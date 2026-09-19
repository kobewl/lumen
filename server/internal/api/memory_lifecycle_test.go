package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lumen/server/internal/profile"
	"lumen/server/internal/storage"
)

// 本文件覆盖记忆生命周期的管理 API：
//   - 全部端点要求管理令牌；
//   - 确认需要明确候选 ID、幂等、写读模型与历史；
//   - 拒绝不可见（不产生任何 Profile 投影）；
//   - 纠正确认替换当前值并保留旧值历史；
//   - 敏感内容拒绝确认并给出原因；
//   - 候选在确认前不出现在 /api/v1/profile（候选不泄漏）。

// newMemoryEnv 建一个启用了生命周期服务的环境。
func newMemoryEnv(t *testing.T) (*testEnv, *profile.Service) {
	t.Helper()
	env := newTestEnv(t, nil)
	svc := profile.NewService(env.store, nil)
	env.srv.SetProfileService(svc)
	return env, svc
}

// seedCandidate 绕过模型直接落一条候选（生命周期测试只需要候选行）。
func seedCandidate(t *testing.T, env *testEnv, id, kind, key, content string) {
	t.Helper()
	if err := env.store.SaveMemoryCandidate(context.Background(), storage.MemoryCandidate{
		ID: id, UserID: "ou_test_user", Kind: kind, Content: content, Key: key,
		SourceIDs: []string{"u_test_turn"}, Confidence: 0.9,
	}); err != nil {
		t.Fatalf("写候选失败: %v", err)
	}
}

// TestMemoryLifecycleEndpoints 覆盖主链路：列表 → 确认 → 视图 → 纠正 → 幂等。
func TestMemoryLifecycleEndpoints(t *testing.T) {
	env, _ := newMemoryEnv(t)
	seedCandidate(t, env, "c_life_1", "preference", "称呼", "叫我小梁")
	seedCandidate(t, env, "c_life_2", "preference", "称呼", "叫我梁哥")

	// 鉴权：无令牌 401。
	if rec := env.do(t, http.MethodGet, "/api/v1/memory/candidates", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，实际 %d", rec.Code)
	}
	if rec := env.do(t, http.MethodPost, "/api/v1/memory/confirm",
		map[string]string{"candidate_id": "c_life_1"}, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌确认应 401，实际 %d", rec.Code)
	}

	// 候选列表可见（状态 candidate）。
	rec := env.do(t, http.MethodGet, "/api/v1/memory/candidates", nil, testAdminToken)
	var list struct {
		Count      int `json:"count"`
		Candidates []struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Key    string `json:"key"`
			Status string `json:"status"`
		} `json:"candidates"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if list.Count != 2 || list.Candidates[0].Status != storage.MemoryStatusCandidate {
		t.Fatalf("应列出 2 条候选，实际 %+v", list)
	}

	// 确认前 /api/v1/profile 必须为空：候选不泄漏。
	rec = env.do(t, http.MethodGet, "/api/v1/profile?user_id=ou_test_user", nil, testAdminToken)
	var before struct {
		Count   int `json:"count"`
		Entries []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"entries"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &before)
	if before.Count != 0 {
		t.Fatalf("确认前读模型必须为空（候选不泄漏），实际 %+v", before)
	}

	// 确认第一条 → 读模型出现 v1。
	rec = env.do(t, http.MethodPost, "/api/v1/memory/confirm",
		map[string]string{"candidate_id": "c_life_1"}, testAdminToken)
	var confirmed struct {
		Status  string `json:"status"`
		Key     string `json:"key"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &confirmed); err != nil || confirmed.Status != "confirmed" {
		t.Fatalf("确认失败: %d %s", rec.Code, rec.Body.String())
	}
	if confirmed.Key != "称呼" || confirmed.Version != 1 {
		t.Fatalf("应为 称呼 v1，实际 %+v", confirmed)
	}

	// 纠正：确认第二条（同槽位）→ v2 替换，旧值保留在响应与历史里。
	rec = env.do(t, http.MethodPost, "/api/v1/memory/confirm",
		map[string]string{"candidate_id": "c_life_2"}, testAdminToken)
	var corrected struct {
		Version       int    `json:"version"`
		Replaced      bool   `json:"replaced"`
		PreviousValue string `json:"previous_value"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &corrected)
	if !corrected.Replaced || corrected.Version != 2 || corrected.PreviousValue != "叫我小梁" {
		t.Fatalf("纠正应替换并保留旧值，实际 %+v (%s)", corrected, rec.Body.String())
	}

	// 视图：只有一个当前值，历史两条。
	rec = env.do(t, http.MethodGet, "/api/v1/profile?user_id=ou_test_user", nil, testAdminToken)
	var view struct {
		Count   int `json:"count"`
		Entries []struct {
			Key     string `json:"key"`
			Value   string `json:"value"`
			Version int    `json:"version"`
		} `json:"entries"`
		History []struct {
			Version int    `json:"version"`
			Value   string `json:"value"`
			Action  string `json:"action"`
		} `json:"history"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Count != 1 || view.Entries[0].Value != "叫我梁哥" || view.Entries[0].Version != 2 {
		t.Fatalf("读模型应只有新值一行，实际 %+v", view)
	}
	if len(view.History) != 2 || view.History[1].Value != "叫我小梁" {
		t.Fatalf("历史应保留两版（旧值可回溯），实际 %+v", view.History)
	}

	// 幂等：重复确认同一候选返回 already_confirmed，版本不变。
	rec = env.do(t, http.MethodPost, "/api/v1/memory/confirm",
		map[string]string{"candidate_id": "c_life_1"}, testAdminToken)
	var again struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if again.Status != "already_confirmed" {
		t.Fatalf("重复确认应幂等，实际 %+v (%s)", again, rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, "/api/v1/profile?user_id=ou_test_user", nil, testAdminToken)
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Entries[0].Version != 2 {
		t.Fatalf("重复确认不应产生新版本，实际 %+v", view.Entries)
	}
}

// TestMemoryRejectAndConflicts 覆盖拒绝与 409/404 语义。
func TestMemoryRejectAndConflicts(t *testing.T) {
	env, _ := newMemoryEnv(t)
	seedCandidate(t, env, "c_rej_1", "preference", "称呼", "叫我梁哥")
	seedCandidate(t, env, "c_rej_2", "preference", "作息", "我的服务器密码是 abc123")
	seedCandidate(t, env, "c_rej_3", "fact", "", "记录链路已接通")

	// 拒绝 → 409 语义：再确认报冲突；Profile 无投影。
	rec := env.do(t, http.MethodPost, "/api/v1/memory/reject",
		map[string]string{"candidate_id": "c_rej_1", "reason": "不是这么叫的"}, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("拒绝失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/api/v1/memory/confirm",
		map[string]string{"candidate_id": "c_rej_1"}, testAdminToken)
	if rec.Code != http.StatusConflict {
		t.Fatalf("拒绝后再确认应 409，实际 %d", rec.Code)
	}
	rec = env.do(t, http.MethodGet, "/api/v1/profile?user_id=ou_test_user", nil, testAdminToken)
	if strings.Contains(rec.Body.String(), "叫我梁哥") {
		t.Fatalf("拒绝的候选不得进入读模型: %s", rec.Body.String())
	}

	// 敏感内容：确认被拒（409 + 原因），候选保持 candidate。
	rec = env.do(t, http.MethodPost, "/api/v1/memory/confirm",
		map[string]string{"candidate_id": "c_rej_2"}, testAdminToken)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "敏感") {
		t.Fatalf("敏感内容确认应 409 并说明原因，实际 %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, "/api/v1/memory/candidates?status=candidate", nil, testAdminToken)
	if !strings.Contains(rec.Body.String(), "c_rej_2") {
		t.Fatal("被拒确认的候选应保持 candidate 可见")
	}

	// fact 类可确认但不进 Profile。
	rec = env.do(t, http.MethodPost, "/api/v1/memory/confirm",
		map[string]string{"candidate_id": "c_rej_3"}, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("fact 类确认失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, "/api/v1/profile?user_id=ou_test_user", nil, testAdminToken)
	if strings.Contains(rec.Body.String(), "记录链路") {
		t.Fatal("fact 类不应进入读模型")
	}

	// 不存在的候选：404。
	rec = env.do(t, http.MethodPost, "/api/v1/memory/confirm",
		map[string]string{"candidate_id": "c_missing"}, testAdminToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的候选应 404，实际 %d", rec.Code)
	}
}

// TestMemoryConfirmTransactionFailure 覆盖阻断 2（API 层）：
// 投影事务失败时确认整体回滚——接口返回 500，候选仍是 candidate，
// 已确认信息无任何痕迹；注入解除后同一确认正常生效。
func TestMemoryConfirmTransactionFailure(t *testing.T) {
	env, _ := newMemoryEnv(t)
	seedCandidate(t, env, "c_tx_1", "preference", "称呼", "叫我梁哥")

	// 注入可控事务错误：历史表一写入就 ABORT（在确认事务内失败 → 整体回滚）。
	if _, err := env.store.DB.ExecContext(context.Background(), `
		CREATE TRIGGER fail_history_api BEFORE INSERT ON user_profile_history
		BEGIN SELECT RAISE(ABORT, 'injected'); END;`); err != nil {
		t.Fatalf("注入触发器失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.store.DB.Exec("DROP TRIGGER IF EXISTS fail_history_api")
	})

	rec := env.do(t, http.MethodPost, "/api/v1/memory/confirm",
		map[string]string{"candidate_id": "c_tx_1"}, testAdminToken)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("事务失败应返回 500，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 无半状态：候选仍是 candidate，profile 为空。
	rec = env.do(t, http.MethodGet, "/api/v1/memory/candidates?status=candidate", nil, testAdminToken)
	if !strings.Contains(rec.Body.String(), "c_tx_1") {
		t.Fatalf("失败后候选必须仍是 candidate，实际 %s", rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, "/api/v1/profile?user_id=debug", nil, testAdminToken)
	if strings.Contains(rec.Body.String(), "叫我梁哥") {
		t.Fatalf("失败后读模型必须为空，实际 %s", rec.Body.String())
	}

	// 解除注入后同一确认正常生效。
	if _, err := env.store.DB.Exec("DROP TRIGGER fail_history_api"); err != nil {
		t.Fatalf("清理触发器失败: %v", err)
	}
	rec = env.do(t, http.MethodPost, "/api/v1/memory/confirm",
		map[string]string{"candidate_id": "c_tx_1"}, testAdminToken)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":1`) {
		t.Fatalf("回滚后重新确认应完整生效，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// TestProfileDisabledEndpoint 覆盖：未装配生命周期服务时端点诚实返回。
func TestProfileDisabledEndpoint(t *testing.T) {
	env := newTestEnv(t, nil)
	rec := env.do(t, http.MethodPost, "/api/v1/memory/confirm",
		map[string]string{"candidate_id": "c1"}, testAdminToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未启用应 503，实际 %d", rec.Code)
	}
}
