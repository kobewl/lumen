package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestInitiativeOutboxRoundTrip 覆盖出站账本的写入、计数与最近尝试查询。
func TestInitiativeOutboxRoundTrip(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "init.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	base := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	rec := InitiativeOutbox{
		ID: "01TEST0001", UserID: "ou_1", LocalDate: "2026-09-18",
		CreatedAt: base, Status: InitiativeStatusDraft,
		Text: "上午进展还顺利吗？", Basis: []string{"s_1"}, Evidence: []string{"s_1"},
	}
	if err := store.SaveInitiativeOutbox(ctx, rec); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 状态机：draft → sent（带投递时间）。
	rec.Status = InitiativeStatusSent
	rec.Channel = "feishu"
	rec.Delivered = base.Add(time.Minute)
	if err := store.SaveInitiativeOutbox(ctx, rec); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	// 第二条：dry_run（计入当日已发）。
	dry := rec
	dry.ID = "01TEST0002"
	dry.Status = InitiativeStatusDryRun
	dry.Channel = ""
	dry.Delivered = time.Time{}
	if err := store.SaveInitiativeOutbox(ctx, dry); err != nil {
		t.Fatalf("写入 dry_run 失败: %v", err)
	}

	// 第三条：rejected（不计入当日已发，但计入最近尝试）。
	rej := rec
	rej.ID = "01TEST0003"
	rej.Status = InitiativeStatusRejected
	rej.SkipReason = "模型判断不打扰"
	if err := store.SaveInitiativeOutbox(ctx, rej); err != nil {
		t.Fatalf("写入 rejected 失败: %v", err)
	}

	sent, err := store.CountInitiativeDeliveredToday(ctx, "ou_1", "2026-09-18")
	if err != nil || sent != 2 {
		t.Fatalf("当日已发应为 2（sent+dry_run），实际 %d (%v)", sent, err)
	}

	last, have, err := store.LastInitiativeAttemptAt(ctx, "ou_1")
	if err != nil || !have {
		t.Fatalf("应有最近尝试: %v (%v)", have, err)
	}
	if !last.Equal(base) {
		t.Fatalf("最近尝试应为最早那条的时间语义无关紧要，但应存在：%v", last)
	}
	// 幂等：同一 ID 重复保存不产生新行。
	if err := store.SaveInitiativeOutbox(ctx, rec); err != nil {
		t.Fatalf("重复保存失败: %v", err)
	}
	records, err := store.RecentInitiativeOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("应只有 3 行（幂等），实际 %d", len(records))
	}
	// 状态更新生效：ID 01TEST0001 应为 sent。
	for _, r := range records {
		if r.ID == "01TEST0001" && r.Status != InitiativeStatusSent {
			t.Fatalf("状态应更新为 sent，实际 %s", r.Status)
		}
	}
}

// TestInitiativeOutboxEmpty 覆盖空账本与未知用户。
func TestInitiativeOutboxEmpty(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	sent, err := store.CountInitiativeDeliveredToday(context.Background(), "ou_x", "2026-09-18")
	if err != nil || sent != 0 {
		t.Fatalf("空账本计数应为 0，实际 %d (%v)", sent, err)
	}
	if _, have, err := store.LastInitiativeAttemptAt(context.Background(), "ou_x"); err != nil || have {
		t.Fatalf("空账本不应有最近尝试: %v (%v)", have, err)
	}
}
