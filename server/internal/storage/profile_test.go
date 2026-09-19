package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newProfileStore 建一个空库供读模型测试用。
func newProfileStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "profile.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestConfirmProfileValueVersions 覆盖：首次确认建立槽位，纠正确认
// version+1 替换当前值并保留历史（previous_value / action）。
func TestConfirmProfileValueVersions(t *testing.T) {
	store := newProfileStore(t)
	ctx := context.Background()

	change, replaced, err := store.ConfirmProfileValue(ctx, ProfileEntry{
		UserID: "u1", Key: "称呼", Kind: "preference", Value: "叫我小梁",
		SourceCandidateID: "c1", SourceIDs: []string{"u_turn_1"},
	}, "c1", "admin")
	if err != nil {
		t.Fatalf("首次确认失败: %v", err)
	}
	if replaced || change.Version != 1 || change.Action != ConfirmActionInitial {
		t.Fatalf("首次确认应为 v1/initial，实际 %+v replaced=%v", change, replaced)
	}

	change, replaced, err = store.ConfirmProfileValue(ctx, ProfileEntry{
		UserID: "u1", Key: "称呼", Kind: "preference", Value: "叫我梁哥",
		SourceCandidateID: "c2", SourceIDs: []string{"u_turn_2"},
	}, "c2", "admin")
	if err != nil {
		t.Fatalf("纠正确认失败: %v", err)
	}
	if !replaced || change.Version != 2 || change.Action != ConfirmActionReplace {
		t.Fatalf("纠正应为 v2/replace，实际 %+v replaced=%v", change, replaced)
	}
	if change.PreviousValue != "叫我小梁" {
		t.Fatalf("历史应保留旧值，实际 %q", change.PreviousValue)
	}

	// 读模型只看得到当前值一个。
	entries, err := store.ProfileEntries(ctx, "u1")
	if err != nil || len(entries) != 1 {
		t.Fatalf("同槽位只应有一行当前值，实际 %+v err=%v", entries, err)
	}
	if entries[0].Value != "叫我梁哥" || entries[0].Version != 2 {
		t.Fatalf("当前值应为新值 v2，实际 %+v", entries[0])
	}

	// 历史两条都在（旧→新可回溯）。
	history, err := store.ProfileHistory(ctx, "u1", "称呼", 50)
	if err != nil || len(history) != 2 {
		t.Fatalf("应有两版历史，实际 %+v err=%v", history, err)
	}
	if history[0].Value != "叫我梁哥" || history[1].Value != "叫我小梁" {
		t.Fatalf("历史顺序应为新→旧，实际 %+v", history)
	}
}

// TestProfileEntriesIndependentKeys 覆盖：不同槽位互不替换，各自成行。
func TestProfileEntriesIndependentKeys(t *testing.T) {
	store := newProfileStore(t)
	ctx := context.Background()
	for _, key := range []string{"称呼", "作息"} {
		if _, _, err := store.ConfirmProfileValue(ctx, ProfileEntry{
			UserID: "u1", Key: key, Kind: "preference", Value: key + "的值",
		}, "c", "admin"); err != nil {
			t.Fatalf("确认 %s 失败: %v", key, err)
		}
	}
	entries, err := store.ProfileEntries(ctx, "u1")
	if err != nil || len(entries) != 2 {
		t.Fatalf("两个槽位应各自成行，实际 %+v err=%v", entries, err)
	}
	if _, ok, err := store.ProfileEntryByKey(ctx, "u1", "作息"); err != nil || !ok {
		t.Fatalf("按键查询应命中，ok=%v err=%v", ok, err)
	}
	if _, ok, _ := store.ProfileEntryByKey(ctx, "u1", "不存在"); ok {
		t.Fatal("不存在的槽位不应命中")
	}
}

// TestMemoryLifecycleConditionalTransitions 覆盖：候选决策是**条件状态转换**。
// 只有 candidate 能确认/拒绝；重复转换幂等；确认/拒绝互不覆盖。
func TestMemoryLifecycleConditionalTransitions(t *testing.T) {
	store := newProfileStore(t)
	ctx := context.Background()

	seedCand := func(id string) {
		t.Helper()
		if err := store.SaveMemoryCandidate(ctx, MemoryCandidate{
			ID: id, UserID: "u1", Kind: "preference", Content: "叫我梁哥",
			Key: "称呼", SourceIDs: []string{"u_turn_1"}, Confidence: 0.9,
			CreatedAt: time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("写入候选失败: %v", err)
		}
	}
	seedCand("c1")

	// 未确认前：无决策列。
	got, ok, err := store.MemoryCandidateByID(ctx, "c1")
	if err != nil || !ok {
		t.Fatalf("按 ID 查询失败 ok=%v err=%v", ok, err)
	}
	if got.Key != "称呼" || got.DecidedAt != (time.Time{}) || got.Status != MemoryStatusCandidate {
		t.Fatalf("新候选应无决策信息，实际 %+v", got)
	}
	if _, ok, _ := store.MemoryCandidateByID(ctx, "c_missing"); ok {
		t.Fatal("不存在的候选不应命中")
	}

	// 确认（fact/project 类：只转换状态，不写投影）。
	change, replaced, already, err := store.ConfirmMemoryCandidate(ctx, "c1", "admin",
		ProfileEntry{}, false)
	if err != nil || already || replaced {
		t.Fatalf("首次确认应生效，实际 already=%v replaced=%v err=%v", already, replaced, err)
	}
	if change.Version != 0 {
		t.Fatalf("非偏好类确认不应产生版本，实际 %+v", change)
	}
	got, _, _ = store.MemoryCandidateByID(ctx, "c1")
	if got.Status != MemoryStatusConfirmed || got.DecidedBy != "admin" || got.DecidedAt.IsZero() {
		t.Fatalf("决策列应写入，实际 %+v", got)
	}

	// 重复确认：幂等（already=true），不产生写入。
	if _, _, already, err := store.ConfirmMemoryCandidate(ctx, "c1", "admin", ProfileEntry{}, false); err != nil || !already {
		t.Fatalf("重复确认应幂等，实际 already=%v err=%v", already, err)
	}

	// 已确认的候选不能改为拒绝。
	if _, err := store.RejectMemoryCandidate(ctx, "c1", "admin", ""); err == nil ||
		!strings.Contains(err.Error(), "已确认") {
		t.Fatalf("拒绝已确认候选应报 ErrCandidateConfirmed，实际 %v", err)
	}

	// 拒绝路径：candidate → rejected → 幂等。
	seedCand("c2")
	if already, err := store.RejectMemoryCandidate(ctx, "c2", "admin", "叫错了"); err != nil || already {
		t.Fatalf("首次拒绝应生效，实际 already=%v err=%v", already, err)
	}
	got, _, _ = store.MemoryCandidateByID(ctx, "c2")
	if got.Status != MemoryStatusRejected || got.RejectReason != "叫错了" {
		t.Fatalf("拒绝列应写入，实际 %+v", got)
	}
	if already, err := store.RejectMemoryCandidate(ctx, "c2", "admin", ""); err != nil || !already {
		t.Fatalf("重复拒绝应幂等，实际 already=%v err=%v", already, err)
	}
	if _, _, _, err := store.ConfirmMemoryCandidate(ctx, "c2", "admin", ProfileEntry{}, false); err == nil ||
		!strings.Contains(err.Error(), "已被拒绝") {
		t.Fatalf("拒绝后确认应报 ErrCandidateRejected，实际 %v", err)
	}
}

// TestConfirmMemoryCandidateAtomicRollback 覆盖阻断 2 的失败注入：
// 投影失败（历史表插入被注入的触发器 ABORT）必须**整体回滚**——
// 候选仍是 candidate、读模型与历史没有任何痕迹。
func TestConfirmMemoryCandidateAtomicRollback(t *testing.T) {
	store := newProfileStore(t)
	ctx := context.Background()
	if err := store.SaveMemoryCandidate(ctx, MemoryCandidate{
		ID: "c_rb", UserID: "u1", Kind: "preference", Content: "叫我梁哥",
		Key: "称呼", SourceIDs: []string{"u_t1"}, Confidence: 0.9,
	}); err != nil {
		t.Fatalf("写入候选失败: %v", err)
	}
	// 注入可控事务错误：历史行一插入就 ABORT。
	if _, err := store.DB.ExecContext(ctx, `
		CREATE TRIGGER fail_history_inject BEFORE INSERT ON user_profile_history
		BEGIN SELECT RAISE(ABORT, 'injected'); END;`); err != nil {
		t.Fatalf("注入触发器失败: %v", err)
	}

	if _, _, _, err := store.ConfirmMemoryCandidate(ctx, "c_rb", "admin", ProfileEntry{
		UserID: "u1", Key: "称呼", Kind: "preference", Value: "叫我梁哥",
		SourceCandidateID: "c_rb", SourceIDs: []string{"u_t1"},
	}, true); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("注入点应使确认失败，实际 %v", err)
	}

	// 半状态检查：候选仍是 candidate；投影与历史为空。
	got, _, _ := store.MemoryCandidateByID(ctx, "c_rb")
	if got.Status != MemoryStatusCandidate {
		t.Fatalf("失败后候选必须仍是 candidate（整体回滚），实际 %s", got.Status)
	}
	entries, _ := store.ProfileEntries(ctx, "u1")
	if len(entries) != 0 {
		t.Fatalf("失败后读模型必须为空，实际 %+v", entries)
	}
	history, _ := store.ProfileHistory(ctx, "u1", "", 10)
	if len(history) != 0 {
		t.Fatalf("失败后历史必须为空，实际 %d", len(history))
	}

	// 解除注入后同一确认可以完整生效。
	if _, err := store.DB.ExecContext(ctx, `DROP TRIGGER fail_history_inject`); err != nil {
		t.Fatalf("清理触发器失败: %v", err)
	}
	change, replaced, already, err := store.ConfirmMemoryCandidate(ctx, "c_rb", "admin", ProfileEntry{
		UserID: "u1", Key: "称呼", Kind: "preference", Value: "叫我梁哥",
		SourceCandidateID: "c_rb", SourceIDs: []string{"u_t1"},
	}, true)
	if err != nil || already || replaced || change.Version != 1 {
		t.Fatalf("回滚后重新确认应完整生效，实际 %+v replaced=%v already=%v err=%v",
			change, replaced, already, err)
	}
}

// TestRecentConversationsProjection 覆盖：近期轮次从 conversations 表投影，
// 只取用户原话与回答，最新在前。
func TestRecentConversationsProjection(t *testing.T) {
	store := newProfileStore(t)
	ctx := context.Background()

	rows := []struct {
		at    time.Time
		raw   string
		reply string
	}{
		{time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC), `{"raw":"早上好"}`, "早上好呀"},
		{time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC), `{"raw":"看看记录"}`, "今天 90 分钟"},
		{time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC), `{"raw":"记一下偏好"}`, "记下了（候选）"},
	}
	for i, r := range rows {
		if err := store.RecordConversation(ctx, "m_"+string(rune('a'+i)), "msg_"+string(rune('a'+i)),
			"u1", "chat", r.raw, r.reply, "[]", "ok"); err != nil {
			t.Fatalf("写入对话失败: %v", err)
		}
	}
	// 一条失败状态的记录不应出现。
	if err := store.RecordConversation(ctx, "m_x", "msg_x", "u1", "chat",
		`{"raw":"失败轮"}`, "", "[]", "error"); err != nil {
		t.Fatalf("写入失败对话失败: %v", err)
	}

	turns, err := store.RecentConversations(ctx, "u1", 2)
	if err != nil {
		t.Fatalf("查询近期轮次失败: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("应返回 2 轮，实际 %d", len(turns))
	}
	if turns[0].UserText != "记一下偏好" || turns[1].UserText != "看看记录" {
		t.Fatalf("应最新在前，实际 %+v", turns)
	}
	if turns[0].AnswerText != "记下了（候选）" {
		t.Fatalf("回答文本应投影，实际 %+v", turns[0])
	}
}
