package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/identity"
	"lumen/server/internal/storage"
	"lumen/server/internal/temporal"
	"lumen/server/internal/tooling"
)

var testLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// fixedNow 是测试用的固定"现在"（13:19，下午），让时间相关工具可复现。
// 所有需要时间的工具调用都经 invTemporal 携带同一份快照——
// 与生产链路"一轮一份、执行器注入"一致。
func fixedNow() time.Time {
	return time.Date(2026, 9, 18, 13, 19, 0, 0, testLoc)
}

// fixedTemporal 构造固定时刻的可信时间快照。
func fixedTemporal() temporal.Context {
	return temporal.Build(temporal.FixedClock(fixedNow()), testLoc)
}

// inv 构造一个携带可信时间与请求者的调用（多数测试用）。
func inv(args tooling.Args) tooling.Invocation {
	return tooling.Invocation{Actor: "u1", Args: args, Temporal: fixedTemporal()}
}

// testStore 建一个内存库。
func testStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "tools.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedSession 写入一段工作时段。
func seedSession(t *testing.T, store *storage.Store, id, date, project string,
	start time.Time, minutes float64) {
	t.Helper()
	apps, _ := json.Marshal([]map[string]any{{"app": "ZCode", "duration_minutes": minutes}})
	gits, _ := json.Marshal([]map[string]any{{"branch": "main", "commit_message": "feat: 接通工具层"}})
	stats, _ := json.Marshal(map[string]any{"duration_minutes": minutes})

	sess := storage.Session{
		ID: id, Date: date, Project: project, StartAt: start,
		EndAt:    start.Add(time.Duration(minutes) * time.Minute),
		AppsJSON: string(apps), GitJSON: string(gits), StatsJSON: string(stats),
		AlgorithmVersion: "rules-v2",
		SourceStartAt:    start, SourceEndAt: start.Add(24 * time.Hour),
		UpdatedAt: time.Now().UTC(),
	}
	from := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, testLoc).UTC()
	if err := store.ReplaceSessions(context.Background(), date, from, from.Add(24*time.Hour),
		[]storage.Session{sess}); err != nil {
		t.Fatalf("写入 Session 失败: %v", err)
	}
}

// seedTask 写入一条 Agent 任务摘要。
func seedTask(t *testing.T, store *storage.Store, taskID, title, status string,
	outcomes, openLoops []string, at time.Time) {
	t.Helper()
	_, err := store.UpsertTaskSummary(context.Background(), storage.TaskSummary{
		ID: "evt-" + taskID, DeviceID: "dev-1", TaskID: taskID,
		Project: "lumen", App: "ZCode", Title: title, Status: status,
		Outcomes: outcomes, OpenLoops: openLoops,
		SourceAgent: "zcode-cli", SourceSessionID: "sess-hidden-001",
		OccurredAt: at, UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("写入任务摘要失败: %v", err)
	}
}

// allTools 返回按名索引的工具集合。
func allTools(t *testing.T, store Store, profile identity.Profile) map[string]tooling.Tool {
	t.Helper()
	set := map[string]tooling.Tool{}
	for _, tool := range All(Options{
		Store: store, Profile: profile, Location: testLoc,
	}) {
		set[tool.Spec().Name] = tool
	}
	return set
}

// TestAllToolsHaveValidSpecs 覆盖"八个工具都能通过注册表校验"。
//
// 注册表构造期会校验每个声明（名称、风险、参数 schema、
// 结果 schema 是否为合法 JSON）。因此这条测试保证：
// 八个工具的声明都是完整且自洽的，不存在"注册不进去"的工具。
func TestAllToolsHaveValidSpecs(t *testing.T) {
	store := testStore(t)
	set := All(Options{Store: store, Profile: identity.Default(), Location: testLoc})

	registry, err := tooling.NewRegistry(set...)
	if err != nil {
		t.Fatalf("默认工具集必须能注册：%v", err)
	}
	if registry.Len() != 8 {
		t.Fatalf("第一批应有 8 个工具，实际 %d 个: %v", registry.Len(), registry.Names())
	}

	want := []string{
		"get_current_time", "get_assistant_profile", "get_conversation_state",
		"get_today_status", "get_sessions", "get_known_projects",
		"get_task_summaries", "save_memory_candidate",
	}
	for _, name := range want {
		spec, ok := registry.Spec(name)
		if !ok {
			t.Fatalf("缺少工具 %s", name)
		}
		// 每个工具都必须有中文说明与结果 schema（模型据此决定怎么用）。
		if !containsHan(spec.Summary) {
			t.Fatalf("工具 %s 的说明应是中文", name)
		}
		if spec.ResultSchema == "" {
			t.Fatalf("工具 %s 缺少结果 schema", name)
		}
	}

	// 只有一个是写入工具，且是低风险、必须带来源。
	writes := 0
	for _, spec := range registry.Specs() {
		if spec.Risk != tooling.RiskRead {
			writes++
			if spec.Risk != tooling.RiskWriteLow {
				t.Fatalf("工具 %s 的风险级别应为低风险写，实际 %s", spec.Name, spec.Risk)
			}
			if !spec.RequiresEvidence {
				t.Fatalf("写入工具 %s 必须要求来源证据", spec.Name)
			}
		}
	}
	if writes != 1 {
		t.Fatalf("第一批应只有 1 个写入工具，实际 %d 个", writes)
	}
}

// TestNoToolAcceptsDangerousParams 覆盖"没有任何工具接受危险参数"。
//
// 这是能力边界的一部分：工具层不提供 SQL、Shell、文件路径或网址入口，
// 因此即使模型想拼一个"读文件"的调用，也没有任何工具能承接它。
func TestNoToolAcceptsDangerousParams(t *testing.T) {
	store := testStore(t)
	for _, tool := range All(Options{Store: store, Profile: identity.Default(), Location: testLoc}) {
		for _, p := range tool.Spec().Parameters {
			lower := strings.ToLower(p.Name)
			for _, bad := range []string{"sql", "query", "command", "cmd", "shell", "path", "url", "file", "exec"} {
				if strings.Contains(lower, bad) {
					t.Fatalf("工具 %s 不应接受参数 %q（危险入口）", tool.Spec().Name, p.Name)
				}
			}
		}
	}
}

// ---- 时间与身份 ----

// TestCurrentTimeTool 覆盖当前时间与时段判断。
func TestCurrentTimeTool(t *testing.T) {
	set := allTools(t, testStore(t), identity.Default())
	tool := set["get_current_time"]

	res, err := tool.Execute(context.Background(), inv(tooling.Args{}))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	out, ok := res.Model.(CurrentTimeResult)
	if !ok {
		t.Fatalf("返回类型不符: %T", res.Model)
	}
	if out.Date != "2026-09-18" || out.Time != "13:19" {
		t.Fatalf("时间不符: %+v", out)
	}
	if out.Weekday != "周五" {
		t.Fatalf("星期应为周五，实际 %q", out.Weekday)
	}
	if out.DayPart != "下午" {
		t.Fatalf("时段应为下午，实际 %q", out.DayPart)
	}
	if len(res.Digest) == 0 || !strings.Contains(res.Digest[0], "2026-09-18") {
		t.Fatalf("应给出确定性事实行，实际 %v", res.Digest)
	}
}

// TestCurrentTimeUsesSameSnapshot 覆盖"工具与可信时间同源"：
// 工具输出的时段必须与快照的 DayPart 一致（不再有自己的第二份答案）。
func TestCurrentTimeUsesSameSnapshot(t *testing.T) {
	tc := fixedTemporal()
	set := allTools(t, testStore(t), identity.Default())
	res, err := set["get_current_time"].Execute(context.Background(),
		tooling.Invocation{Actor: "u1", Temporal: tc})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	out := res.Model.(CurrentTimeResult)
	if out.DayPart != tc.DayPart {
		t.Fatalf("工具时段 %q 应与快照 %q 一致", out.DayPart, tc.DayPart)
	}
}

// TestAssistantProfileTool 覆盖身份来自配置。
func TestAssistantProfileTool(t *testing.T) {
	profile := identity.Profile{
		Name: "小灯", Role: "学习助手", OwnerDisplayName: "liang",
		Language: "zh-CN", Tone: "温和", Proactivity: "低",
	}
	set := allTools(t, testStore(t), profile)

	res, err := set["get_assistant_profile"].Execute(context.Background(), inv(tooling.Args{}))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	out := res.Model.(AssistantProfileResult)
	if out.Name != "小灯" || out.Role != "学习助手" || out.OwnerDisplayName != "liang" {
		t.Fatalf("身份应来自配置，实际 %+v", out)
	}
}

// TestConversationStateTool 覆盖对话状态读取与空状态语义。
func TestConversationStateTool(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	set := allTools(t, store, identity.Default())
	tool := set["get_conversation_state"]

	// 空状态：必须说明"这是第一次对话"，而不是显示成"暂未识别项目"。
	res, err := tool.Execute(ctx, inv(tooling.Args{}))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	empty := res.Model.(ConversationStateResult)
	if empty.CurrentProject != "" {
		t.Fatalf("空状态不应显示项目名，实际 %q", empty.CurrentProject)
	}
	if empty.Note == "" {
		t.Fatal("空状态应说明原因")
	}

	// 有状态：项目名经过展示转换。
	if err := store.SaveConversationState(ctx, storage.ConversationState{
		UserID: "u1", CurrentProject: "lumen", PendingQuestion: "你指的是哪个项目？",
		LastMode: "clarify", LastTimeRange: "today",
	}); err != nil {
		t.Fatalf("写入状态失败: %v", err)
	}
	res, err = tool.Execute(ctx, inv(tooling.Args{}))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	got := res.Model.(ConversationStateResult)
	if got.CurrentProject != "lumen" || got.PendingQuestion == "" {
		t.Fatalf("应读到已保存的状态，实际 %+v", got)
	}

	// 另一个用户的请求者身份不会读到 u1 的状态（身份由代码注入）。
	res, _ = tool.Execute(ctx, tooling.Invocation{Actor: "u2", Temporal: fixedTemporal()})
	if other := res.Model.(ConversationStateResult); other.CurrentProject != "" {
		t.Fatalf("不同请求者不应共享状态，实际 %+v", other)
	}
}

// TestConversationStateHidesUnclassified 覆盖内部标记不外泄。
func TestConversationStateHidesUnclassified(t *testing.T) {
	store := testStore(t)
	if err := store.SaveConversationState(context.Background(), storage.ConversationState{
		UserID: "u1", CurrentProject: "unclassified", LastMode: "recall",
	}); err != nil {
		t.Fatalf("写入状态失败: %v", err)
	}
	set := allTools(t, store, identity.Default())

	res, _ := set["get_conversation_state"].Execute(context.Background(), inv(tooling.Args{}))
	out := res.Model.(ConversationStateResult)
	if out.CurrentProject == "unclassified" {
		t.Fatal("unclassified 是内部标记，不能进入模型上下文")
	}
	if !strings.Contains(out.CurrentProject, "暂未识别项目") {
		t.Fatalf("应转成用户可读文案，实际 %q", out.CurrentProject)
	}
}

// ---- 活动与项目 ----

// TestTodayStatusTool 覆盖今天的活动摘要。
func TestTodayStatusTool(t *testing.T) {
	store := testStore(t)
	start := time.Date(2026, 9, 18, 9, 10, 0, 0, testLoc)
	seedSession(t, store, "s_today", "2026-09-18", "lumen", start, 90)
	set := allTools(t, store, identity.Default())

	res, err := set["get_today_status"].Execute(context.Background(), inv(tooling.Args{}))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	out := res.Model.(ActivityResult)
	if out.Date != "2026-09-18" {
		t.Fatalf("日期应为 2026-09-18，实际 %q", out.Date)
	}
	if out.Count != 1 || out.TotalMinutes != 90 {
		t.Fatalf("应为 1 段 90 分钟，实际 count=%d total=%v", out.Count, out.TotalMinutes)
	}
	if len(out.Sessions[0].Apps) == 0 || !strings.Contains(out.Sessions[0].Apps[0], "ZCode") {
		t.Fatalf("应包含应用与时长，实际 %+v", out.Sessions[0])
	}
	// 证据必须是真实时段 ID（供审计核实），但不进模型视图。
	if len(res.Evidence) != 1 || res.Evidence[0] != "s_today" {
		t.Fatalf("证据应为真实时段 ID，实际 %v", res.Evidence)
	}
	if strings.Contains(res.ModelJSON(), "s_today") {
		t.Fatalf("内部 ID 不应出现在模型视图里: %s", res.ModelJSON())
	}
	if res.Count != 1 || !res.Span.Valid() {
		t.Fatalf("应给出条数与区间，实际 count=%d span=%v", res.Count, res.Span)
	}
}

// TestSessionsToolRequiresScope 覆盖"必须给出范围"。
//
// 不给范围就等于拉全库，这是工具内的硬约束（策略之外的第二道）。
func TestSessionsToolRequiresScope(t *testing.T) {
	store := testStore(t)
	set := allTools(t, store, identity.Default())
	tool := set["get_sessions"]

	_, err := tool.Execute(context.Background(), inv(tooling.Args{}))
	if _, denied := tooling.AsDenial(err); !denied {
		t.Fatalf("缺少范围应返回工具级拒绝，实际 %v", err)
	}

	_, err = tool.Execute(context.Background(), inv(tooling.Args{"date": "不是日期"}))
	if _, denied := tooling.AsDenial(err); !denied {
		t.Fatalf("非法日期应返回工具级拒绝，实际 %v", err)
	}
}

// TestSessionsToolByProjectAndDate 覆盖两种检索方式。
func TestSessionsToolByProjectAndDate(t *testing.T) {
	store := testStore(t)
	at := time.Date(2026, 9, 17, 9, 0, 0, 0, testLoc)
	seedSession(t, store, "s_1", "2026-09-17", "lumen", at, 60)
	set := allTools(t, store, identity.Default())
	tool := set["get_sessions"]
	ctx := context.Background()

	byDate, err := tool.Execute(ctx, inv(tooling.Args{"date": "2026-09-17"}))
	if err != nil {
		t.Fatalf("按日期执行失败: %v", err)
	}
	if got := byDate.Model.(ActivityResult); got.Count != 1 {
		t.Fatalf("按日期应查到 1 段，实际 %d", got.Count)
	}

	byProject, err := tool.Execute(ctx, inv(tooling.Args{"project": "lumen"}))
	if err != nil {
		t.Fatalf("按项目执行失败: %v", err)
	}
	if got := byProject.Model.(ActivityResult); got.Count != 1 {
		t.Fatalf("按项目应查到 1 段，实际 %d", got.Count)
	}

	// 未分类项目名在查询时应使用内部标记（这里是查询入口，不是展示）。
	unclassified, err := tool.Execute(ctx, inv(tooling.Args{"project": "unclassified"}))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if got := unclassified.Model.(ActivityResult); got.Count != 0 {
		t.Fatalf("库里没有未分类时段，实际 %d 段", got.Count)
	}
}

// TestKnownProjectsTool 覆盖项目清单与稳定排序。
func TestKnownProjectsTool(t *testing.T) {
	store := testStore(t)
	now := fixedNow()
	for i, name := range []string{"zebra", "alpha", "lumen"} {
		day := now.AddDate(0, 0, -i)
		seedSession(t, store, "s_"+name, day.Format("2006-01-02"), name,
			day.Add(-5*time.Hour), 30)
	}
	set := allTools(t, store, identity.Default())

	res, err := set["get_known_projects"].Execute(context.Background(), inv(tooling.Args{}))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	out := res.Model.(KnownProjectsResult)
	if out.Count != 3 {
		t.Fatalf("应有 3 个项目，实际 %d", out.Count)
	}
	// 排序必须稳定，否则提示词每次都不同。
	if out.Projects[0] != "alpha" || out.Projects[2] != "zebra" {
		t.Fatalf("项目名应排序，实际 %v", out.Projects)
	}
	if out.Days != 14 {
		t.Fatalf("默认回看 14 天，实际 %d", out.Days)
	}
}

// TestKnownProjectsToolEmptyNote 覆盖"没有项目"时的说明。
func TestKnownProjectsToolEmptyNote(t *testing.T) {
	set := allTools(t, testStore(t), identity.Default())
	res, err := set["get_known_projects"].Execute(context.Background(), inv(tooling.Args{}))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if got := res.Model.(KnownProjectsResult); got.Note == "" {
		t.Fatal("没有项目时应说明原因，避免模型把空当成错误")
	}
	if len(res.Digest) == 0 {
		t.Fatal("应给出确定性事实行")
	}
}

// ---- 任务摘要 ----

// TestTaskSummariesTool 覆盖任务摘要读取与状态汇总。
func TestTaskSummariesTool(t *testing.T) {
	store := testStore(t)
	at := time.Date(2026, 9, 18, 15, 42, 0, 0, testLoc)
	seedTask(t, store, "t-done", "接通工具层", "done", []string{"八个工具可用"}, nil, at)
	seedTask(t, store, "t-blocked", "接入上报", "blocked", nil, []string{"等上游接口"}, at)
	set := allTools(t, store, identity.Default())

	res, err := set["get_task_summaries"].Execute(context.Background(), inv(tooling.Args{"date": "2026-09-18"}))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	out := res.Model.(TaskSummariesResult)
	if out.Count != 2 {
		t.Fatalf("应有 2 条，实际 %d", out.Count)
	}
	if out.StatusCounts["done"] != 1 || out.StatusCounts["blocked"] != 1 {
		t.Fatalf("状态汇总不符: %v", out.StatusCounts)
	}
	// 证据是 task_id（来源 Agent 的稳定标识）。
	if len(res.Evidence) != 2 {
		t.Fatalf("应带回 2 个任务来源，实际 %v", res.Evidence)
	}
	// 内部会话 ID 不外泄。
	if strings.Contains(res.ModelJSON(), "sess-hidden-001") {
		t.Fatalf("来源会话 ID 不应进入模型视图: %s", res.ModelJSON())
	}
}

// TestTaskSummariesToolRequiresScope 覆盖必须给出范围。
func TestTaskSummariesToolRequiresScope(t *testing.T) {
	set := allTools(t, testStore(t), identity.Default())
	_, err := set["get_task_summaries"].Execute(context.Background(), inv(tooling.Args{}))
	if _, denied := tooling.AsDenial(err); !denied {
		t.Fatalf("缺少范围应返回工具级拒绝，实际 %v", err)
	}
}

// TestTaskSummariesToolClipsLongHistory 覆盖历史长文本的裁剪。
func TestTaskSummariesToolClipsLongHistory(t *testing.T) {
	store := testStore(t)
	at := time.Date(2026, 9, 18, 15, 42, 0, 0, testLoc)
	many := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		many = append(many, strings.Repeat("果", 300))
	}
	seedTask(t, store, "t-long", strings.Repeat("长", 500), "done", many, nil, at)
	set := allTools(t, store, identity.Default())

	res, err := set["get_task_summaries"].Execute(context.Background(), inv(tooling.Args{"date": "2026-09-18"}))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	body := res.ModelJSON()
	if len([]rune(body)) > 3000 {
		t.Fatalf("模型视图应被裁剪，实际 %d 字", len([]rune(body)))
	}
}

// ---- 记忆候选写入 ----

// TestSaveMemoryCandidate 覆盖低风险写入的正常路径。
func TestSaveMemoryCandidate(t *testing.T) {
	store := testStore(t)
	set := allTools(t, store, identity.Default())

	res, err := set["save_memory_candidate"].Execute(context.Background(), tooling.Invocation{
		Actor:      "u1",
		Temporal:   fixedTemporal(),
		TurnSource: "u_test_turn_001",
		Args: tooling.Args{
			"kind": "preference", "key": "回答风格", "content": "用户偏好先看结论再看细节", "confidence": 0.8,
		},
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	out := res.Model.(MemoryCandidateResult)
	if !out.Saved || out.Status != storage.MemoryStatusCandidate {
		t.Fatalf("应保存为候选状态，实际 %+v", out)
	}
	if out.SourceCount != 1 {
		t.Fatalf("偏好来源应只有本轮用户消息来源，实际 %d", out.SourceCount)
	}

	candidates, err := store.MemoryCandidatesByUser(context.Background(), "u1", 10)
	if err != nil {
		t.Fatalf("读取候选失败: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("应有 1 条候选，实际 %d", len(candidates))
	}
	if candidates[0].Status != storage.MemoryStatusCandidate {
		t.Fatalf("状态必须是 candidate，实际 %q", candidates[0].Status)
	}
	// 审查修复：偏好只能以本轮用户消息来源为依据，不得挂活动/任务证据。
	if len(candidates[0].SourceIDs) != 1 || candidates[0].SourceIDs[0] != "u_test_turn_001" {
		t.Fatalf("偏好来源应为代码生成的本轮用户消息，实际 %v", candidates[0].SourceIDs)
	}
	// 已确认记忆必须仍为空：候选不会自动晋升。
	confirmed, _ := store.MemoryCandidatesByStatus(context.Background(), storage.MemoryStatusConfirmed, 10)
	if len(confirmed) != 0 {
		t.Fatalf("候选不应自动晋升，实际 %d 条", len(confirmed))
	}
}

// TestSaveMemoryCandidateDeniesWithoutEvidence 覆盖无来源、无身份、敏感内容三种拒绝。
func TestSaveMemoryCandidateDeniesWithoutEvidence(t *testing.T) {
	store := testStore(t)
	set := allTools(t, store, identity.Default())
	tool := set["save_memory_candidate"]
	ctx := context.Background()

	cases := map[string]tooling.Invocation{
		"事实没有读取证据": {Actor: "u1", Temporal: fixedTemporal(), Args: tooling.Args{
			"kind": "fact", "content": "一条没有出处的候选记忆"}},
		"偏好没有用户消息来源": {Actor: "u1", Temporal: fixedTemporal(), Evidence: []string{"s_1"}, Args: tooling.Args{
			"kind": "preference", "content": "一条没有用户消息来源的偏好"}},
		"没有请求者身份": {Actor: "", Temporal: fixedTemporal(), TurnSource: "u_x", Evidence: []string{"s_1"}, Args: tooling.Args{
			"kind": "fact", "content": "一条没有归属的候选记忆"}},
		"疑似凭证": {Actor: "u1", Temporal: fixedTemporal(), TurnSource: "u_x", Evidence: []string{"s_1"}, Args: tooling.Args{
			"kind": "fact", "content": "用户的 API key 是 sk-abcdef123456"}},
		"信息量不足": {Actor: "u1", Temporal: fixedTemporal(), TurnSource: "u_x", Evidence: []string{"s_1"}, Args: tooling.Args{
			"kind": "fact", "content": "嗯"}},
	}
	for name, inv := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tool.Execute(ctx, inv)
			if _, denied := tooling.AsDenial(err); !denied {
				t.Fatalf("%s 应被拒绝，实际 %v", name, err)
			}
		})
	}
	if n, _ := store.CountMemoryCandidates(ctx); n != 0 {
		t.Fatalf("被拒的内容不应落库，实际 %d 条", n)
	}
}

// TestSaveMemoryCandidateNormalizesKind 覆盖类别归一。
func TestSaveMemoryCandidateNormalizesKind(t *testing.T) {
	store := testStore(t)
	set := allTools(t, store, identity.Default())
	ctx := context.Background()

	_, err := set["save_memory_candidate"].Execute(ctx, tooling.Invocation{
		Actor: "u1", Temporal: fixedTemporal(), TurnSource: "u_test_turn_002",
		Args: tooling.Args{"kind": "PREFERENCE", "key": " 总结风格", "content": "用户喜欢简短的总结", "confidence": 0.9},
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	rows, _ := store.MemoryCandidatesByUser(ctx, "u1", 10)
	if len(rows) != 1 || rows[0].Kind != "preference" {
		t.Fatalf("类别应归一为 preference，实际 %+v", rows)
	}
	if rows[0].Confidence != 0.9 {
		t.Fatalf("确信度应保留，实际 %v", rows[0].Confidence)
	}
}

// ---- 通用约束 ----

// TestToolsHideInternalIDsInModelView 覆盖"内部 ID 不进模型视图"。
//
// 证据 ID 只走 Result.Evidence（供审计），模型看到的是脱敏视图。
func TestToolsHideInternalIDsInModelView(t *testing.T) {
	store := testStore(t)
	at := time.Date(2026, 9, 18, 9, 0, 0, 0, testLoc)
	seedSession(t, store, "s_secret_internal_id", "2026-09-18", "lumen", at, 30)
	seedTask(t, store, "t_internal_task_id", "标题", "done", nil, nil, at)

	ctx := context.Background()
	for name, tool := range allTools(t, store, identity.Default()) {
		var res tooling.Result
		var err error
		switch name {
		case "get_today_status":
			res, err = tool.Execute(ctx, inv(tooling.Args{}))
		case "get_task_summaries":
			res, err = tool.Execute(ctx, inv(tooling.Args{"date": "2026-09-18"}))
		default:
			continue
		}
		if err != nil {
			t.Fatalf("%s 执行失败: %v", name, err)
		}
		body := res.ModelJSON()
		for _, id := range []string{"s_secret_internal_id", "sess-hidden-001"} {
			if strings.Contains(body, id) {
				t.Fatalf("%s 的模型视图不应包含内部 ID %q: %s", name, id, body)
			}
		}
	}
}

// TestToolsNeverLeakUnclassified 覆盖内部项目标记不外泄。
func TestToolsNeverLeakUnclassified(t *testing.T) {
	store := testStore(t)
	at := time.Date(2026, 9, 18, 9, 0, 0, 0, testLoc)
	seedSession(t, store, "s_unclassified", "2026-09-18", "unclassified", at, 30)
	set := allTools(t, store, identity.Default())

	res, err := set["get_today_status"].Execute(context.Background(), inv(tooling.Args{}))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	body := res.ModelJSON()
	if strings.Contains(body, "unclassified") {
		t.Fatalf("内部标记不应进入模型视图: %s", body)
	}
	if !strings.Contains(body, "暂未识别项目") {
		t.Fatalf("应转成用户可读文案: %s", body)
	}
	// 事实行同样不能泄露内部标记（它会直接发给用户）。
	for _, line := range res.Digest {
		if strings.Contains(line, "unclassified") {
			t.Fatalf("事实行不应包含内部标记: %s", line)
		}
	}
}

// TestToolsWithNilStore 覆盖存储缺失时的行为（拒绝而不是 panic）。
func TestToolsWithNilStore(t *testing.T) {
	set := allTools(t, nil, identity.Default())
	ctx := context.Background()

	// 身份与时间不依赖存储，仍应可用。
	if _, err := set["get_current_time"].Execute(ctx, inv(tooling.Args{})); err != nil {
		t.Fatalf("时间工具不应依赖存储: %v", err)
	}
	if _, err := set["get_assistant_profile"].Execute(ctx, inv(tooling.Args{})); err != nil {
		t.Fatalf("身份工具不应依赖存储: %v", err)
	}
	// 依赖存储的工具应报错或拒绝，而不是 panic。
	for _, name := range []string{"get_today_status", "get_known_projects"} {
		if _, err := set[name].Execute(ctx, inv(tooling.Args{})); err == nil {
			t.Fatalf("%s 在缺少存储时应报错", name)
		}
	}
	// 空状态应被当作"第一次对话"，而不是错误。
	if _, err := set["get_conversation_state"].Execute(ctx, inv(tooling.Args{})); err != nil {
		t.Fatalf("空状态不应报错: %v", err)
	}
	// 写入必须被明确拒绝（而不是静默成功）。
	if _, err := set["save_memory_candidate"].Execute(ctx, tooling.Invocation{
		Actor: "u1", Evidence: []string{"s_1"},
		Args: tooling.Args{"kind": "fact", "content": "一条候选记忆内容"},
	}); err == nil {
		t.Fatal("缺少存储时写入必须被拒绝")
	}
}

// TestToolsRequireTemporal 覆盖"绕过执行器直接调用时大声失败"。
//
// 工具不允许悄悄退回 time.Now：没有可信时间就报错，
// "中午说早呀"才不会换个形式回来。
func TestToolsRequireTemporal(t *testing.T) {
	set := allTools(t, testStore(t), identity.Default())
	for _, name := range []string{"get_current_time", "get_today_status", "get_known_projects"} {
		if _, err := set[name].Execute(context.Background(), tooling.Invocation{Actor: "u1"}); err == nil {
			t.Fatalf("%s 缺少可信时间时应报错", name)
		}
	}
}

// containsHan 判断字符串里是否含中日韩汉字（用于断言"说明是中文"）。
func containsHan(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}
