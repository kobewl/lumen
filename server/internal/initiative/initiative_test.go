package initiative

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/contextassembler"
	"lumen/server/internal/identity"
	"lumen/server/internal/notification"
	"lumen/server/internal/storage"
	"lumen/server/internal/temporal"
	"lumen/server/internal/tooling"
	"lumen/server/internal/tools"
)

var testLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// at 构造上海时区固定时刻。
func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, testLoc)
	if err != nil {
		t.Fatalf("解析时间 %q 失败: %v", value, err)
	}
	return parsed
}

// ---- Policy ----

// TestPolicyQuietHours 覆盖验收 P1-2：静默 22:30–08:30（含边界）。
func TestPolicyQuietHours(t *testing.T) {
	policy := DefaultPolicy()
	policy.Enabled = true

	cases := []struct {
		clock  string
		quiet  bool
		reason string
	}{
		{"2026-09-18 22:29", false, ""},
		{"2026-09-18 22:30", true, "静默"}, // 边界：静默开始
		{"2026-09-18 23:59", true, "静默"},
		{"2026-09-19 00:30", true, "静默"},
		{"2026-09-19 08:29", true, "静默"},
		{"2026-09-19 08:30", false, ""}, // 边界：静默结束
		{"2026-09-18 13:19", false, ""},
	}
	for _, c := range cases {
		t.Run(c.clock, func(t *testing.T) {
			tc := temporal.Build(temporal.FixedClock(at(t, c.clock)), testLoc)
			d := policy.Decide(tc, 0, time.Time{}, false, false)
			if d.Allowed == c.quiet {
				if c.quiet {
					t.Fatalf("%s 应处于静默时段，实际放行", c.clock)
				}
				t.Fatalf("%s 不应被拦（原因 %q）", c.clock, d.Reason)
			}
			if c.quiet && !strings.Contains(d.Reason, c.reason) {
				t.Fatalf("静默原因应说明静默时段，实际 %q", d.Reason)
			}
		})
	}
}

// TestPolicyDailyLimitAndInterval 覆盖验收 P1-3/P1-4。
func TestPolicyDailyLimitAndInterval(t *testing.T) {
	policy := DefaultPolicy()
	policy.Enabled = true
	tc := temporal.Build(temporal.FixedClock(at(t, "2026-09-18 13:19")), testLoc)

	// 当日上限：2 条后拒绝。
	if d := (policy).Decide(tc, 1, time.Time{}, false, false); !d.Allowed {
		t.Fatalf("发过 1 条仍应放行，实际 %q", d.Reason)
	}
	d := policy.Decide(tc, 2, time.Time{}, false, false)
	if d.Allowed || !strings.Contains(d.Reason, "上限") {
		t.Fatalf("发过 2 条应达上限，实际 %+v", d)
	}

	// 间隔：3h59m 拒，4h 整放。
	last := at(t, "2026-09-18 09:20")
	if d := policy.Decide(tc, 0, last, true, false); d.Allowed {
		t.Fatal("距上次 3h59m 应被间隔拦下")
	}
	last = at(t, "2026-09-18 09:19")
	if d := policy.Decide(tc, 0, last, true, false); !d.Allowed {
		t.Fatalf("距上次 4h 应放行，实际 %q", d.Reason)
	}
}

// TestPolicySwitchAndForce 覆盖验收 P1-1 与 force 的边界。
func TestPolicySwitchAndForce(t *testing.T) {
	policy := DefaultPolicy() // Enabled=false
	tc := temporal.Build(temporal.FixedClock(at(t, "2026-09-18 13:19")), testLoc)

	// 关闭时 force 也无效：功能关闭不是频率限制。
	if d := policy.Decide(tc, 0, time.Time{}, false, true); d.Allowed {
		t.Fatal("开关关闭时 force 也不应放行")
	}

	policy.Enabled = true
	// 静默时段 force 可以跳过（本地验收用）。
	tcQuiet := temporal.Build(temporal.FixedClock(at(t, "2026-09-18 23:30")), testLoc)
	if d := policy.Decide(tcQuiet, 0, time.Time{}, false, true); !d.Allowed {
		t.Fatalf("force 应跳过静默限制，实际 %q", d.Reason)
	}
}

// ---- 服务闭环（fake planner + fake messenger）----

// fakeInitPlanner 是测试用假提案模型。
type fakeInitPlanner struct {
	proposal Proposal
	err      error
	reqs     []ProposalRequest
}

func (f *fakeInitPlanner) Propose(_ context.Context, req ProposalRequest) (Proposal, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return Proposal{}, f.err
	}
	return f.proposal, nil
}

// fakeMessenger 记录投递调用（验收 P1-6 的 fake messenger）。
type fakeMessenger struct {
	sent []string
	err  error
}

func (f *fakeMessenger) Name() string  { return "fake" }
func (f *fakeMessenger) Enabled() bool { return true }
func (f *fakeMessenger) SendText(_ context.Context, _, text string) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, text)
	return nil
}

// newService 组装一个带种子的测试服务。
//
// store 里有今天的一条时段与一条 Agent 任务摘要，保证"有事实可依据"。
func newService(t *testing.T, clockAt string, planner Planner, sender notification.Sender,
	dryRun bool, policy Policy, targets []string) (*Service, *storage.Store) {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "init.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := at(t, clockAt)
	seedFacts(t, store, now)

	registry, err := tooling.NewRegistry(tools.All(tools.Options{
		Store: store, Profile: identity.Default(), Location: testLoc,
	})...)
	if err != nil {
		t.Fatalf("构造注册表失败: %v", err)
	}
	executor, err := tooling.NewExecutor(tooling.ExecutorOptions{
		Registry: registry, Audit: &tooling.MemorySink{}, Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("构造执行器失败: %v", err)
	}

	svc := New(Options{
		Store: store, Executor: executor, Planner: planner, Profile: identity.Default(),
		Loc: testLoc, Clock: temporal.FixedClock(now),
		Sender: sender, Targets: targets, DryRun: dryRun, Policy: policy,
		Logger: silentLogger(),
	})
	return svc, store
}

// seedFacts 写入今天的时段与任务摘要。
func seedFacts(t *testing.T, store *storage.Store, now time.Time) {
	t.Helper()
	date := now.Format("2006-01-02")
	apps, _ := json.Marshal([]map[string]any{{"app": "ZCode", "duration_minutes": 90}})
	stats, _ := json.Marshal(map[string]any{"duration_minutes": 90})
	start := now.Add(-2 * time.Hour)
	sess := storage.Session{
		ID: "s_init_1", Date: date, Project: "lumen", StartAt: start,
		EndAt: start.Add(90 * time.Minute), AppsJSON: string(apps), StatsJSON: string(stats),
		AlgorithmVersion: "rules-v2", SourceStartAt: start, SourceEndAt: start.Add(24 * time.Hour),
		UpdatedAt: time.Now().UTC(),
	}
	from := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, testLoc).UTC()
	if err := store.ReplaceSessions(context.Background(), date, from, from.Add(24*time.Hour),
		[]storage.Session{sess}); err != nil {
		t.Fatalf("写入 Session 失败: %v", err)
	}
	if _, err := store.UpsertTaskSummary(context.Background(), storage.TaskSummary{
		ID: "evt-1", DeviceID: "dev-1", TaskID: "t_init_1", Project: "lumen",
		Title: "接通主动关怀", Status: "done", SourceAgent: "zcode-cli",
		OccurredAt: now.Add(-time.Hour), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("写入任务摘要失败: %v", err)
	}
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// proposalFor 返回一条合法提案（依据种子数据里的证据 ID）。
func proposalFor(basis ...string) Proposal {
	return Proposal{
		Reason: "用户上午在推进项目", Question: "上午在 lumen 上花了不少时间，进展还顺利吗？",
		Basis: basis,
	}
}

// TestServiceDryRunDoesNotSend 覆盖验收 P1-5：dry-run 写 outbox 但不调用渠道。
func TestServiceDryRunDoesNotSend(t *testing.T) {
	policy := DefaultPolicy()
	policy.Enabled = true
	messenger := &fakeMessenger{}
	planner := &fakeInitPlanner{proposal: proposalFor("t_init_1", "get_today_status")}
	svc, store := newService(t, "2026-09-18 13:19", planner, messenger, true, policy,
		[]string{"ou_target"})

	out, err := svc.RunOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out.Action != "dry_run" {
		t.Fatalf("应为 dry_run，实际 %s (%s)", out.Action, out.Reason)
	}
	if len(messenger.sent) != 0 {
		t.Fatalf("dry-run 不应真实发送，实际发送 %d 条", len(messenger.sent))
	}
	if out.Text == "" || !strings.Contains(out.Text, "顺利吗") {
		t.Fatalf("应生成问题全文，实际 %q", out.Text)
	}
	if len(planner.reqs) != 1 {
		t.Fatalf("应调用一次提案，实际 %d", len(planner.reqs))
	}
	// 提案输入必须含可信时间与事实。
	facts := planner.reqs[0].FactsJSON
	for _, want := range []string{"trusted_context", "13:19", "sessions", "task_summaries"} {
		if !strings.Contains(facts, want) {
			t.Fatalf("提案输入应含 %q，实际: %s", want, facts)
		}
	}

	// outbox 落了 dry_run 行，basis 与 evidence 可追溯。
	records, err := store.RecentInitiativeOutbox(context.Background(), 10)
	if err != nil || len(records) != 1 {
		t.Fatalf("应有 1 条 outbox 记录，实际 %d (%v)", len(records), err)
	}
	rec := records[0]
	if rec.Status != storage.InitiativeStatusDryRun {
		t.Fatalf("状态应为 dry_run，实际 %s", rec.Status)
	}
	if len(rec.Basis) != 2 || len(rec.Evidence) != 2 {
		t.Fatalf("依据与证据应落库，实际 basis=%v evidence=%v", rec.Basis, rec.Evidence)
	}
	if rec.Channel != "" || !rec.Delivered.IsZero() {
		t.Fatalf("dry-run 不应有投递痕迹，实际 channel=%q delivered=%v", rec.Channel, rec.Delivered)
	}
}

// TestServiceRealSendUsesFakeMessenger 覆盖验收 P1-6。
func TestServiceRealSendUsesFakeMessenger(t *testing.T) {
	policy := DefaultPolicy()
	policy.Enabled = true
	messenger := &fakeMessenger{}
	planner := &fakeInitPlanner{proposal: proposalFor("get_today_status")}
	svc, store := newService(t, "2026-09-18 13:19", planner, messenger, false, policy,
		[]string{"ou_target"})

	out, err := svc.RunOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out.Action != "sent" {
		t.Fatalf("应为 sent，实际 %s (%s)", out.Action, out.Reason)
	}
	if len(messenger.sent) != 1 || messenger.sent[0] != out.Text {
		t.Fatalf("应投递 1 次且内容一致，实际 %v", messenger.sent)
	}

	records, _ := store.RecentInitiativeOutbox(context.Background(), 10)
	if len(records) != 1 || records[0].Status != storage.InitiativeStatusSent {
		t.Fatalf("outbox 应记录 sent，实际 %+v", records)
	}
	if records[0].Delivered.IsZero() || records[0].Channel != "fake" {
		t.Fatalf("应记录投递时间与渠道，实际 %+v", records[0])
	}
}

// TestServiceFrequencyAndIdempotency 覆盖验收 P1-3/P1-4 与幂等：
// 同一天第二次触发被间隔拦住；间隔计数包含 rejected。
func TestServiceFrequencyAndIdempotency(t *testing.T) {
	policy := DefaultPolicy()
	policy.Enabled = true
	planner := &fakeInitPlanner{proposal: proposalFor("get_today_status")}
	svc, _ := newService(t, "2026-09-18 13:19", planner, nil, true, policy,
		[]string{"ou_target"})

	if out, err := svc.RunOnce(context.Background(), false); err != nil || out.Action != "dry_run" {
		t.Fatalf("首次应 dry_run，实际 %s (%v)", out.Action, err)
	}
	// 第二次（间隔不足 4h）：跳过且不调模型。
	out, err := svc.RunOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out.Action != "skipped" || !strings.Contains(out.Reason, "间隔") {
		t.Fatalf("第二次应因间隔跳过，实际 %s (%s)", out.Action, out.Reason)
	}
	if len(planner.reqs) != 1 {
		t.Fatalf("被跳过时不应调用模型，实际 %d 次", len(planner.reqs))
	}
	// force 可以连跑（本地验收）。
	if out, err := svc.RunOnce(context.Background(), true); err != nil || out.Action != "dry_run" {
		t.Fatalf("force 应放行，实际 %s (%s, %v)", out.Action, out.Reason, err)
	}
}

// TestServiceRejectsBadProposals 覆盖验收 P1-7 的三类拒绝。
func TestServiceRejectsBadProposals(t *testing.T) {
	cases := []struct {
		name     string
		proposal Proposal
		want     string
	}{
		{"依据不在本轮事实里", Proposal{Question: "进展顺利吗？", Basis: []string{"s_forged"}},
			"不在本轮已核实的事实"},
		{"没有任何依据", Proposal{Question: "喝水了吗？"}, "任何依据"},
		{"把活动说成完成", Proposal{Question: "听说你完成了功能，接下来也顺利吗？", Basis: []string{"get_today_status"}},
			"没有 Agent 报告"},
		{"财务判断", Proposal{Question: "最近理财收益怎么样？", Basis: []string{"get_today_status"}},
			"不允许的内容"},
		{"医疗判断", Proposal{Question: "需要治疗建议吗？", Basis: []string{"get_today_status"}},
			"不允许的内容"},
		{"不是提问", Proposal{Question: "祝你今天顺利。", Basis: []string{"get_today_status"}},
			"提问"},
		{"内部术语", Proposal{Question: "session 记录怎么样？", Basis: []string{"get_today_status"}},
			"内部术语"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			policy := DefaultPolicy()
			policy.Enabled = true
			planner := &fakeInitPlanner{proposal: c.proposal}
			svc, store := newService(t, "2026-09-18 13:19", planner, nil, true, policy,
				[]string{"ou_target"})

			out, err := svc.RunOnce(context.Background(), false)
			if err != nil {
				t.Fatalf("执行失败: %v", err)
			}
			if out.Action != "rejected" || !strings.Contains(out.Reason, c.want) {
				t.Fatalf("应拒绝（含 %q），实际 %s (%s)", c.want, out.Action, out.Reason)
			}
			// 拒绝同样落审计（并计入间隔），但不投递。
			records, _ := store.RecentInitiativeOutbox(context.Background(), 10)
			if len(records) != 1 || records[0].Status != storage.InitiativeStatusRejected {
				t.Fatalf("拒绝应落 outbox，实际 %+v", records)
			}
			if records[0].SkipReason == "" {
				t.Fatal("拒绝原因应落库")
			}
		})
	}
}

// TestServiceModelSkipIsAuditedAndCounted 覆盖"模型判断不打扰"的路径：
// 落审计、计入间隔（不烧钱重试）。
func TestServiceModelSkipIsAuditedAndCounted(t *testing.T) {
	policy := DefaultPolicy()
	policy.Enabled = true
	planner := &fakeInitPlanner{proposal: Proposal{Skip: true, Reason: "刚聊过，不打扰"}}
	svc, store := newService(t, "2026-09-18 13:19", planner, nil, true, policy,
		[]string{"ou_target"})

	out, err := svc.RunOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out.Action != "rejected" || !strings.Contains(out.Reason, "不打扰") {
		t.Fatalf("应为 rejected（模型跳过），实际 %s (%s)", out.Action, out.Reason)
	}
	records, _ := store.RecentInitiativeOutbox(context.Background(), 10)
	if len(records) != 1 || records[0].Status != storage.InitiativeStatusRejected {
		t.Fatalf("模型跳过应落审计，实际 %+v", records)
	}
	// 被跳过后 30 分钟内的下一次 tick 应被间隔拦住（不再调模型）。
	before := len(planner.reqs)
	svc2 := New(Options{
		Store: mustSameStore(t, store), Executor: mustExecutor(t, store), Planner: planner,
		Profile: identity.Default(), Loc: testLoc,
		Clock:   temporal.FixedClock(at(t, "2026-09-18 13:49")),
		Targets: []string{"ou_target"}, DryRun: true, Policy: policy, Logger: silentLogger(),
	})
	out, err = svc2.RunOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out.Action != "skipped" || !strings.Contains(out.Reason, "间隔") {
		t.Fatalf("模型跳过后的 30 分钟应被间隔拦住，实际 %s (%s)", out.Action, out.Reason)
	}
	if len(planner.reqs) != before {
		t.Fatal("被间隔拦住时不应调用模型")
	}
}

// TestServiceNoFactsSkipsModel 覆盖验收 P1-8：没有可依据记录时不调模型。
func TestServiceNoFactsSkipsModel(t *testing.T) {
	policy := DefaultPolicy()
	policy.Enabled = true
	planner := &fakeInitPlanner{proposal: proposalFor("get_today_status")}

	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry, err := tooling.NewRegistry(tools.All(tools.Options{
		Store: store, Profile: identity.Default(), Location: testLoc,
	})...)
	if err != nil {
		t.Fatalf("构造注册表失败: %v", err)
	}
	executor, err := tooling.NewExecutor(tooling.ExecutorOptions{Registry: registry, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("构造执行器失败: %v", err)
	}
	svc := New(Options{
		Store: store, Executor: executor, Planner: planner, Profile: identity.Default(),
		Loc: testLoc, Clock: temporal.FixedClock(at(t, "2026-09-18 13:19")),
		Targets: []string{"ou_target"}, DryRun: true, Policy: policy, Logger: silentLogger(),
	})

	out, err := svc.RunOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out.Action != "skipped" || !strings.Contains(out.Reason, "没有可依据") {
		t.Fatalf("无事实应跳过，实际 %s (%s)", out.Action, out.Reason)
	}
	if len(planner.reqs) != 0 {
		t.Fatal("无事实时不应调用模型")
	}
}

// TestServiceDisabled 覆盖验收 P1-1：开关关闭时一切都不发生。
func TestServiceDisabled(t *testing.T) {
	policy := DefaultPolicy() // Enabled=false
	planner := &fakeInitPlanner{proposal: proposalFor("get_today_status")}
	svc, store := newService(t, "2026-09-18 13:19", planner, nil, true, policy,
		[]string{"ou_target"})

	out, err := svc.RunOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out.Action != "disabled" {
		t.Fatalf("应为 disabled，实际 %s", out.Action)
	}
	if len(planner.reqs) != 0 {
		t.Fatal("关闭时不应调用模型")
	}
	records, _ := store.RecentInitiativeOutbox(context.Background(), 10)
	if len(records) != 0 {
		t.Fatalf("关闭时不应写 outbox，实际 %d 条", len(records))
	}
}

// TestServiceSenderUnavailableKeepsDraft 覆盖"渠道不可用 → 草稿保留"。
func TestServiceSenderUnavailableKeepsDraft(t *testing.T) {
	policy := DefaultPolicy()
	policy.Enabled = true
	planner := &fakeInitPlanner{proposal: proposalFor("get_today_status")}
	// DryRun=false 但渠道为 nil。
	svc, store := newService(t, "2026-09-18 13:19", planner, nil, false, policy,
		[]string{"ou_target"})

	out, err := svc.RunOnce(context.Background(), false)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out.Action != "draft" {
		t.Fatalf("应为 draft，实际 %s (%s)", out.Action, out.Reason)
	}
	records, _ := store.RecentInitiativeOutbox(context.Background(), 10)
	if len(records) != 1 || records[0].Status != storage.InitiativeStatusDraft {
		t.Fatalf("草稿应保留为 draft，实际 %+v", records)
	}
	// 草稿不算已发送（未消耗当日上限），但计入间隔。
	sent, err := store.CountInitiativeDeliveredToday(context.Background(), "ou_target", "2026-09-18")
	if err != nil || sent != 0 {
		t.Fatalf("draft 不应计入当日已发，实际 %d (%v)", sent, err)
	}
}

// TestProposalValidator 覆盖校验器的边界（不动模型）。
func TestProposalValidator(t *testing.T) {
	evidence := []string{"s_1", "t_1"}
	toolNames := []string{"get_today_status", "get_task_summaries"}
	if reason := validateProposal(proposalFor("t_1"), evidence, []string{"t_1"}, toolNames); reason != "" {
		t.Fatalf("合法提案应通过，实际 %q", reason)
	}
	// 工具名也是合法依据（时段内部 ID 刻意不给模型，来源声明由代码映射核实）。
	if reason := validateProposal(proposalFor("get_today_status"), evidence, []string{"t_1"}, toolNames); reason != "" {
		t.Fatalf("引用事实来源工具名应通过，实际 %q", reason)
	}
	// Agent 报告作依据时可以说"完成"。
	p := Proposal{Question: "听说 ZCode 报告完成了任务，收尾顺利吗？", Basis: []string{"t_1"}}
	if reason := validateProposal(p, evidence, []string{"t_1"}, toolNames); reason != "" {
		t.Fatalf("有任务依据的完成陈述应通过，实际 %q", reason)
	}
	// 只有活动来源却宣称"完成" → 拒。
	p = Proposal{Question: "听说你完成了功能，接下来顺利吗？", Basis: []string{"get_today_status"}}
	if reason := validateProposal(p, evidence, []string{"t_1"}, toolNames); reason == "" {
		t.Fatal("活动来源支撑完成宣称应被拒")
	}
}

// ---- 测试辅助 ----

func mustSameStore(t *testing.T, store *storage.Store) *storage.Store {
	t.Helper()
	return store
}

func mustExecutor(t *testing.T, store *storage.Store) *tooling.Executor {
	t.Helper()
	registry, err := tooling.NewRegistry(tools.All(tools.Options{
		Store: store, Profile: identity.Default(), Location: testLoc,
	})...)
	if err != nil {
		t.Fatalf("构造注册表失败: %v", err)
	}
	exec, err := tooling.NewExecutor(tooling.ExecutorOptions{Registry: registry, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("构造执行器失败: %v", err)
	}
	return exec
}

// 防止 fmt 未用（保留给以后调试输出）。
var _ = fmt.Sprintf
var _ = contextassembler.ConversationSummary{}
