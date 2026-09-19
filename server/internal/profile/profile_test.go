package profile

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lumen/server/internal/storage"
)

// newService 建一个空库上的生命周期服务。
func newService(t *testing.T) (*Service, *storage.Store) {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "life.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewService(store, nil), store
}

// seed 塞一条候选（绕过工具层，直接测生命周期）。
func seed(t *testing.T, store *storage.Store, id, kind, key, content string) {
	t.Helper()
	err := store.SaveMemoryCandidate(context.Background(), storage.MemoryCandidate{
		ID: id, UserID: "u1", Kind: kind, Content: content, Key: key,
		SourceIDs: []string{"u_turn_1"}, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("写入候选失败: %v", err)
	}
}

// TestConfirmPreferenceCreatesEntry 覆盖验收 P1-5：确认后进读模型。
func TestConfirmPreferenceCreatesEntry(t *testing.T) {
	svc, store := newService(t)
	seed(t, store, "c1", "preference", "称呼", "叫我梁哥")

	res, err := svc.Confirm(context.Background(), "c1", "admin")
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if res.Status != storage.MemoryStatusConfirmed || res.Key != "称呼" || res.Version != 1 {
		t.Fatalf("确认结果不符，实际 %+v", res)
	}
	entries, _ := store.ProfileEntries(context.Background(), "u1")
	if len(entries) != 1 || entries[0].Value != "叫我梁哥" {
		t.Fatalf("读模型应有当前值，实际 %+v", entries)
	}
	// 候选行标记确认且保留原文。
	c, _, _ := store.MemoryCandidateByID(context.Background(), "c1")
	if c.Status != storage.MemoryStatusConfirmed || c.Content != "叫我梁哥" || c.DecidedBy != "admin" {
		t.Fatalf("候选行应记录决策且不改写内容，实际 %+v", c)
	}
}

// TestConfirmIdempotent 覆盖验收 P1-8：重复确认幂等，不重复计版本。
func TestConfirmIdempotent(t *testing.T) {
	svc, store := newService(t)
	seed(t, store, "c1", "preference", "称呼", "叫我梁哥")

	if _, err := svc.Confirm(context.Background(), "c1", "admin"); err != nil {
		t.Fatalf("首次确认失败: %v", err)
	}
	res, err := svc.Confirm(context.Background(), "c1", "admin")
	if err != nil {
		t.Fatalf("重复确认不应报错: %v", err)
	}
	if res.Status != "already_confirmed" {
		t.Fatalf("重复确认应返回 already_confirmed，实际 %+v", res)
	}
	entries, _ := store.ProfileEntries(context.Background(), "u1")
	if len(entries) != 1 || entries[0].Version != 1 {
		t.Fatalf("重复确认不应产生新版本，实际 %+v", entries)
	}
	history, _ := store.ProfileHistory(context.Background(), "u1", "", 50)
	if len(history) != 1 {
		t.Fatalf("历史应只有一条，实际 %d", len(history))
	}
}

// TestConfirmCorrectionReplaces 覆盖验收 P1-7：同 key 新确认值替换旧值。
func TestConfirmCorrectionReplaces(t *testing.T) {
	svc, store := newService(t)
	seed(t, store, "c1", "preference", "称呼", "叫我小梁")
	seed(t, store, "c2", "preference", "称呼", "叫我梁哥")

	if _, err := svc.Confirm(context.Background(), "c1", "admin"); err != nil {
		t.Fatalf("首次确认失败: %v", err)
	}
	res, err := svc.Confirm(context.Background(), "c2", "admin")
	if err != nil {
		t.Fatalf("纠正确认失败: %v", err)
	}
	if !res.Replaced || res.Version != 2 || res.PreviousValue != "叫我小梁" {
		t.Fatalf("纠正应替换并保留旧值，实际 %+v", res)
	}
	entries, _ := store.ProfileEntries(context.Background(), "u1")
	// 不存在两个互相矛盾的值同时注入：同槽位只有当前值一行。
	if len(entries) != 1 || entries[0].Value != "叫我梁哥" {
		t.Fatalf("槽位应只有新值一行，实际 %+v", entries)
	}
}

// TestRejectNotVisible 覆盖验收 P1-6：拒绝的候选不产生任何 Profile 投影。
func TestRejectNotVisible(t *testing.T) {
	svc, store := newService(t)
	seed(t, store, "c1", "preference", "称呼", "叫我梁哥")

	res, err := svc.Reject(context.Background(), "c1", "admin", "不是这么叫的")
	if err != nil || res.Status != storage.MemoryStatusRejected {
		t.Fatalf("拒绝失败: %+v err=%v", res, err)
	}
	entries, _ := store.ProfileEntries(context.Background(), "u1")
	if len(entries) != 0 {
		t.Fatalf("拒绝的候选不得进入读模型，实际 %+v", entries)
	}
	// 已拒绝的候选不能再确认（拒绝是终态）。
	if _, err := svc.Confirm(context.Background(), "c1", "admin"); !errors.Is(err, ErrRejected) {
		t.Fatalf("拒绝后确认应报 ErrRejected，实际 %v", err)
	}
	// 重复拒绝幂等。
	if res, err := svc.Reject(context.Background(), "c1", "admin", ""); err != nil || res.Status != "already_rejected" {
		t.Fatalf("重复拒绝应幂等，实际 %+v err=%v", res, err)
	}
}

// TestConfirmSensitiveRefused 覆盖验收 P1-9：敏感内容拒绝确认并记录原因，
// 候选保持 candidate（管理端可见，不是悄悄吞掉）。
func TestConfirmSensitiveRefused(t *testing.T) {
	svc, store := newService(t)
	// 工具写入侧会拦这类内容，这里直接落库模拟历史脏数据。
	seed(t, store, "c1", "preference", "称呼", "我的服务器密码是 abc123")

	_, err := svc.Confirm(context.Background(), "c1", "admin")
	if !errors.Is(err, ErrSensitive) {
		t.Fatalf("敏感内容确认应报 ErrSensitive，实际 %v", err)
	}
	entries, _ := store.ProfileEntries(context.Background(), "u1")
	if len(entries) != 0 {
		t.Fatalf("敏感内容不得进入读模型，实际 %+v", entries)
	}
	c, _, _ := store.MemoryCandidateByID(context.Background(), "c1")
	if c.Status != storage.MemoryStatusCandidate {
		t.Fatalf("候选应保持 candidate，实际 %s", c.Status)
	}
	// 拒绝原因进安全审计（不扩散正文）。
	events, _ := store.RecentSecurityEvents(context.Background(), 10)
	found := false
	for _, e := range events {
		if e["kind"] == "profile_confirm_refused" && strings.Contains(e["detail"], "敏感") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应记录确认被拒的审计事件，实际 %+v", events)
	}
}

// TestRejectConfirmedRefused 覆盖：已确认的候选不能改为拒绝（撤销不在本版本）。
func TestRejectConfirmedRefused(t *testing.T) {
	svc, store := newService(t)
	seed(t, store, "c1", "preference", "称呼", "叫我梁哥")
	if _, err := svc.Confirm(context.Background(), "c1", "admin"); err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if _, err := svc.Reject(context.Background(), "c1", "admin", ""); !errors.Is(err, ErrConfirmed) {
		t.Fatalf("拒绝已确认候选应报 ErrConfirmed，实际 %v", err)
	}
}

// TestFactKindNotInProfile 覆盖：非偏好类确认只标记状态，不进读模型。
func TestFactKindNotInProfile(t *testing.T) {
	svc, store := newService(t)
	seed(t, store, "c1", "fact", "", "记录链路已接通")

	res, err := svc.Confirm(context.Background(), "c1", "admin")
	if err != nil || res.Status != storage.MemoryStatusConfirmed {
		t.Fatalf("fact 类应可确认，实际 %+v err=%v", res, err)
	}
	if res.Key != "" {
		t.Fatalf("fact 类不应写读模型槽位，实际 %+v", res)
	}
	entries, _ := store.ProfileEntries(context.Background(), "u1")
	if len(entries) != 0 {
		t.Fatalf("fact 类不得进入读模型，实际 %+v", entries)
	}
}

// TestLegacyKeylessGoesToGeneralSlot 覆盖迁移兼容：旧库 key=” 的偏好
// 确认时落到 general 槽位。
func TestLegacyKeylessGoesToGeneralSlot(t *testing.T) {
	svc, store := newService(t)
	seed(t, store, "c1", "preference", "", "旧格式偏好内容")

	res, err := svc.Confirm(context.Background(), "c1", "admin")
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if res.Key != "general" {
		t.Fatalf("无 key 的旧候选应落到 general，实际 %q", res.Key)
	}
}

// TestNoHalfStateOnProjectionFailure 覆盖阻断 2（服务层）：
// 投影失败时整个确认回滚——候选仍是 candidate，读模型无痕迹；
// 注入解除后同一确认完整生效。
func TestNoHalfStateOnProjectionFailure(t *testing.T) {
	svc, store := newService(t)
	seed(t, store, "c1", "preference", "称呼", "叫我梁哥")

	// 注入可控事务错误：历史表一写入就 ABORT（事务内失败 → 整体回滚）。
	if _, err := store.DB.ExecContext(context.Background(), `
		CREATE TRIGGER fail_history_inject BEFORE INSERT ON user_profile_history
		BEGIN SELECT RAISE(ABORT, 'injected'); END;`); err != nil {
		t.Fatalf("注入触发器失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.DB.Exec("DROP TRIGGER IF EXISTS fail_history_inject")
	})

	if _, err := svc.Confirm(context.Background(), "c1", "admin"); err == nil {
		t.Fatal("注入点应使确认失败")
	}
	// 半状态检查：候选必须仍是 candidate，读模型/历史必须为空。
	c, _, _ := store.MemoryCandidateByID(context.Background(), "c1")
	if c.Status != storage.MemoryStatusCandidate {
		t.Fatalf("投影失败后候选必须仍是 candidate，实际 %s", c.Status)
	}
	if entries, _ := store.ProfileEntries(context.Background(), "u1"); len(entries) != 0 {
		t.Fatalf("投影失败后读模型必须为空，实际 %+v", entries)
	}
	if history, _ := store.ProfileHistory(context.Background(), "u1", "", 10); len(history) != 0 {
		t.Fatalf("投影失败后历史必须为空，实际 %d", len(history))
	}

	// 解除注入，确认完整生效（v1，无脏版本）。
	if _, err := store.DB.Exec("DROP TRIGGER fail_history_inject"); err != nil {
		t.Fatalf("清理触发器失败: %v", err)
	}
	res, err := svc.Confirm(context.Background(), "c1", "admin")
	if err != nil || res.Version != 1 || res.Status != storage.MemoryStatusConfirmed {
		t.Fatalf("回滚后重新确认应完整生效，实际 %+v err=%v", res, err)
	}
}

// TestConcurrentConfirmExactlyOneWinner 覆盖阻断 2 的并发要求：
// N 个并发确认同一候选——只有一个真正生效，其余全部幂等返回
// already_confirmed；version 只加一次、历史只有一条、无半状态。
func TestConcurrentConfirmExactlyOneWinner(t *testing.T) {
	svc, store := newService(t)
	seed(t, store, "c_race", "preference", "称呼", "叫我梁哥")

	const n = 8
	results := make(chan Result, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			res, err := svc.Confirm(context.Background(), "c_race", "admin")
			if err != nil {
				errs <- err
				return
			}
			results <- res
		}()
	}
	confirmed := 0
	already := 0
	errCount := 0
	for i := 0; i < n; i++ {
		select {
		case res := <-results:
			switch res.Status {
			case storage.MemoryStatusConfirmed:
				confirmed++
			case "already_confirmed":
				already++
			default:
				errCount++
			}
		case err := <-errs:
			t.Logf("并发确认错误（应为零）: %v", err)
			errCount++
		}
	}
	if confirmed != 1 || already != n-1 || errCount != 0 {
		t.Fatalf("应恰好 1 个生效、%d 个幂等、0 个错误，实际 confirmed=%d already=%d err=%d",
			n-1, confirmed, already, errCount)
	}
	// 版本只加一次；历史只有一条。
	entries, _ := store.ProfileEntries(context.Background(), "u1")
	if len(entries) != 1 || entries[0].Version != 1 {
		t.Fatalf("并发确认后版本必须恰好 1，实际 %+v", entries)
	}
	if history, _ := store.ProfileHistory(context.Background(), "u1", "称呼", 10); len(history) != 1 {
		t.Fatalf("并发确认后历史必须恰好 1 条，实际 %d", len(history))
	}
}

// TestConcurrentConfirmRejectRace 覆盖确认/拒绝并发竞争：
// 只有一个方向生效，结果与最终状态一致（不互相覆盖、无半状态）。
func TestConcurrentConfirmRejectRace(t *testing.T) {
	for attempt := 0; attempt < 3; attempt++ {
		svc, store := newService(t)
		seed(t, store, "c_cr", "preference", "称呼", "叫我梁哥")

		type outcome struct {
			confirmErr, rejectErr error
			confirmRes, rejectRes Result
		}
		done := make(chan outcome, 1)
		go func() {
			o := outcome{}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				o.confirmRes, o.confirmErr = svc.Confirm(context.Background(), "c_cr", "admin")
			}()
			go func() {
				defer wg.Done()
				o.rejectRes, o.rejectErr = svc.Reject(context.Background(), "c_cr", "admin", "测试拒绝")
			}()
			wg.Wait()
			done <- o
		}()
		o := <-done

		c, _, _ := store.MemoryCandidateByID(context.Background(), "c_cr")
		entries, _ := store.ProfileEntries(context.Background(), "u1")
		switch c.Status {
		case storage.MemoryStatusConfirmed:
			// 确认胜出：拒绝方必须报 ErrConfirmed；投影恰好一条 v1。
			if o.confirmErr != nil || !errors.Is(o.rejectErr, ErrConfirmed) {
				t.Fatalf("确认胜出时结果不一致：confirm=%+v/%v reject=%+v/%v",
					o.confirmRes, o.confirmErr, o.rejectRes, o.rejectErr)
			}
			if len(entries) != 1 || entries[0].Version != 1 {
				t.Fatalf("确认胜出后应有唯一 v1 投影，实际 %+v", entries)
			}
		case storage.MemoryStatusRejected:
			// 拒绝胜出：确认方必须报 ErrRejected；投影必须为空。
			if o.rejectErr != nil || !errors.Is(o.confirmErr, ErrRejected) {
				t.Fatalf("拒绝胜出时结果不一致：confirm=%+v/%v reject=%+v/%v",
					o.confirmRes, o.confirmErr, o.rejectRes, o.rejectErr)
			}
			if len(entries) != 0 {
				t.Fatalf("拒绝胜出后不得有投影，实际 %+v", entries)
			}
		default:
			t.Fatalf("竞争后状态必须是终态，实际 %s", c.Status)
		}
	}
}

// TestUnknownAndMissing 覆盖 404 语义与 NormalizeSlotKey 的归一化。
func TestUnknownAndMissing(t *testing.T) {
	svc, _ := newService(t)
	if _, err := svc.Confirm(context.Background(), "missing", "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的候选应 ErrNotFound，实际 %v", err)
	}
	if _, err := svc.Reject(context.Background(), "", "admin", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空 ID 应 ErrNotFound，实际 %v", err)
	}
	cases := map[string]string{
		" 称呼 ": "称呼", "Answer Style": "answer_style", "作 息": "作_息",
	}
	for raw, want := range cases {
		if got := NormalizeSlotKey(raw); got != want {
			t.Fatalf("NormalizeSlotKey(%q)=%q，期望 %q", raw, got, want)
		}
	}
}
