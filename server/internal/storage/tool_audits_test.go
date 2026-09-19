package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"lumen/server/internal/tooling"
)

// TestToolAuditMigrationApplies 覆盖迁移 v4 让审计表可用。
func TestToolAuditMigrationApplies(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "migrate.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 迁移后表必须存在且可写——否则"审计"只是接口上的一句承诺。
	if _, err := store.CountToolAudits(context.Background()); err != nil {
		t.Fatalf("审计表不可用: %v", err)
	}

	var version int
	if err := store.DB.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("读取迁移版本失败: %v", err)
	}
	if version < 4 {
		t.Fatalf("迁移版本应至少为 4，实际 %d", version)
	}

	// 重复迁移必须幂等。
	if err := Migrate(context.Background(), store.DB); err != nil {
		t.Fatalf("重复迁移应成功: %v", err)
	}
}

// TestToolAuditRoundTrip 覆盖审计写入与读出。
func TestToolAuditRoundTrip(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	sink := ToolAuditSink{Store: store}
	ctx := context.Background()
	at := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

	if err := sink.Record(ctx, tooling.AuditRecord{
		At: at, Actor: "u1", Tool: "get_task_summaries", Risk: tooling.RiskRead,
		Args:       map[string]any{"date": "2026-09-18"},
		Decision:   tooling.DecisionAllowed,
		ResultKind: tooling.KindReportedTasks, Count: 2, EvidenceN: 2, DurationMS: 7,
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := sink.Record(ctx, tooling.AuditRecord{
		At: at.Add(time.Second), Actor: "u1", Tool: "save_memory_candidate",
		Risk: tooling.RiskWriteLow, Decision: tooling.DecisionDenied,
		Reason: "本轮没有可核实的来源证据", Truncated: true,
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 空参数与空时间也要能落库（防御性：不要让"审计"因为字段缺失而失败）。
	if err := sink.Record(ctx, tooling.AuditRecord{
		Tool: "get_today_status", Decision: tooling.DecisionError,
	}); err != nil {
		t.Fatalf("最小审计记录应可写入: %v", err)
	}

	records, err := store.RecentToolAudits(ctx, 10)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("应有 3 条审计，实际 %d", len(records))
	}
	// 最近的在前。
	if records[0].Tool != "get_today_status" {
		t.Fatalf("应按时间倒序返回，实际第一条 %+v", records[0])
	}

	latest := records[1]
	if latest.Tool != "save_memory_candidate" || latest.Decision != tooling.DecisionDenied {
		t.Fatalf("内容不符: %+v", latest)
	}
	if latest.Reason != "本轮没有可核实的来源证据" {
		t.Fatalf("拒绝原因应完整保存，实际 %q", latest.Reason)
	}
	if latest.Risk != string(tooling.RiskWriteLow) {
		t.Fatalf("风险级别应保存，实际 %q", latest.Risk)
	}
	if !latest.Truncated {
		t.Fatal("截断标记应保存")
	}
	if len(latest.Args) != 0 {
		t.Fatalf("没有参数时 Args 应为空 map，实际 %v", latest.Args)
	}
	if latest.At.IsZero() {
		t.Fatal("没有给时间时也应落一个时间戳")
	}

	first := records[2]
	if first.Args["date"] != "2026-09-18" {
		t.Fatalf("参数应完整往返，实际 %v", first.Args)
	}
	if first.ItemCount != 2 || first.EvidenceN != 2 || first.DurationMS != 7 {
		t.Fatalf("计数与耗时应保存，实际 %+v", first)
	}
	if !first.At.Equal(at) {
		t.Fatalf("时间应保存为 UTC，实际 %v 期望 %v", first.At, at)
	}

	n, err := store.CountToolAudits(ctx)
	if err != nil || n != 3 {
		t.Fatalf("统计应为 3，实际 %d (%v)", n, err)
	}
}

// TestRecentToolAuditsLimit 覆盖条数上限与默认值。
func TestRecentToolAuditsLimit(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "limit.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	sink := ToolAuditSink{Store: store}
	for i := 0; i < 8; i++ {
		if err := sink.Record(context.Background(), tooling.AuditRecord{
			Tool: "get_today_status", Decision: tooling.DecisionAllowed,
		}); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	got, err := store.RecentToolAudits(context.Background(), 3)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应遵守条数上限，实际 %d", len(got))
	}
	// 非法上限回退到默认值，而不是返回全部。
	got, err = store.RecentToolAudits(context.Background(), -1)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("默认上限应覆盖全部 8 条，实际 %d", len(got))
	}
}

// TestToolAuditSinkWithoutStore 覆盖装配缺失时报错而不是 panic。
func TestToolAuditSinkWithoutStore(t *testing.T) {
	sink := ToolAuditSink{}
	if err := sink.Record(context.Background(), tooling.AuditRecord{Tool: "x"}); err == nil {
		t.Fatal("缺少存储时应报错")
	}
}
