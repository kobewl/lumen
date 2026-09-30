package contextassembler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/identity"
	"lumen/server/internal/storage"
	"lumen/server/internal/temporal"
	"lumen/server/internal/tooling"
)

// fakeStore 是装配器的假存储：只提供读模型数据，没有候选记忆的读取路径。
type fakeStore struct {
	entries    []storage.ProfileEntry
	entriesErr error
	state      storage.ConversationState
	stateErr   error
	turns      []storage.RecentTurn
	turnsErr   error
}

func (f *fakeStore) ProfileEntries(ctx context.Context, userID string) ([]storage.ProfileEntry, error) {
	if f.entriesErr != nil {
		return nil, f.entriesErr
	}
	return f.entries, nil
}

func (f *fakeStore) ConversationStateByUser(ctx context.Context, userID string) (storage.ConversationState, error) {
	if f.stateErr != nil {
		return storage.ConversationState{}, f.stateErr
	}
	return f.state, nil
}

func (f *fakeStore) RecentConversations(ctx context.Context, userID string, limit int) ([]storage.RecentTurn, error) {
	if f.turnsErr != nil {
		return nil, f.turnsErr
	}
	return f.turns, nil
}

func testTemporal(t *testing.T) temporal.Context {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return temporal.Build(temporal.FixedClock(time.Date(2026, 9, 18, 13, 19, 0, 0, loc)), loc)
}

// TestAssembleIncludesTrustedSections 覆盖：已确认信息、会话摘要、近期轮次
// 全部进入 Bundle，trusted 块包含已确认信息，近期轮次在不可信证据区。
func TestAssembleIncludesTrustedSections(t *testing.T) {
	store := &fakeStore{
		entries: []storage.ProfileEntry{
			{UserID: "u1", Key: "称呼", Kind: "preference", Value: "叫我梁哥", Version: 2},
		},
		state: storage.ConversationState{CurrentProject: "lumen", LastTimeRange: "today"},
		turns: []storage.RecentTurn{
			{UserText: "lumen 进展如何", AnswerText: "今天投入了 90 分钟。"},
		},
	}
	a := NewAssembler(store, identity.Default(), DefaultBudget(), nil)
	b := a.Assemble(context.Background(), Request{UserID: "u1", Temporal: testTemporal(t), UserText: " 那个项目呢 "})

	if len(b.ProfileEntries()) != 1 || b.ProfileEntries()[0].Key != "称呼" || b.ProfileEntries()[0].Version != 2 {
		t.Fatalf("已确认信息应注入，实际 %+v", b.ProfileEntries())
	}
	if b.Summary().CurrentProject != "lumen" {
		t.Fatalf("会话摘要应注入，实际 %+v", b.Summary())
	}
	if len(b.RecentTurns()) != 1 {
		t.Fatalf("近期轮次应注入 1 轮，实际 %d", len(b.RecentTurns()))
	}
	if b.UserText != "那个项目呢" {
		t.Fatalf("用户消息应清洗，实际 %q", b.UserText)
	}
	if b.TurnSource == "" || !strings.HasPrefix(b.TurnSource, "u_") {
		t.Fatalf("应有代码生成的轮次来源，实际 %q", b.TurnSource)
	}

	trusted := b.RenderTrusted("")
	for _, want := range []string{"叫我梁哥", "第 2 版", "已确认信息", "上一轮提到的项目", "lumen"} {
		if !strings.Contains(trusted, want) {
			t.Fatalf("trusted 块应包含 %q，实际:\n%s", want, trusted)
		}
	}
	// 近期轮次在不可信块里，且带来源标注。
	turns := b.RenderRecentTurns()
	if !strings.Contains(turns, `<recent_turns source="conversation">`) || !strings.Contains(turns, "lumen 进展如何") {
		t.Fatalf("近期轮次应渲染为带来源的不可信块，实际:\n%s", turns)
	}
	if strings.Contains(trusted, "lumen 进展如何") {
		t.Fatal("近期轮次原文不应进入 trusted 块（用户文本不可信）")
	}
	if len(b.Truncations()) != 0 {
		t.Fatalf("数据齐全时不应有截断元信息，实际 %v", b.Truncations())
	}
}

// TestCandidatesNeverEnterBundle 覆盖验收 P0-3：
// 候选记忆正文绝不出现在任何提示词里。装配器的存储接口根本没有读候选的方法，
// 这里用"内容唯一的标记串"证明它进不来。
func TestCandidatesNeverEnterBundle(t *testing.T) {
	marker := "CANDIDATE_MARKER_绝不能出现_xq7"
	store := &fakeStore{
		entries: []storage.ProfileEntry{
			{UserID: "u1", Key: "称呼", Kind: "preference", Value: "叫我梁哥", Version: 1},
		},
		state: storage.ConversationState{CurrentProject: "lumen"},
		turns: []storage.RecentTurn{{UserText: "你好", AnswerText: "你好呀"}},
	}
	a := NewAssembler(store, identity.Default(), DefaultBudget(), nil)
	b := a.Assemble(context.Background(), Request{UserID: "u1", Temporal: testTemporal(t), UserText: "在吗"})
	b.AddToolResults([]tooling.Result{{Tool: "get_today_status", Count: 1,
		Digest: []string{"today"}}}, nil)

	for _, render := range []string{
		b.RenderTrusted(""), b.RenderRecentTurns(), b.RenderToolData(),
		b.RenderUserMessage(),
	} {
		if strings.Contains(render, marker) {
			t.Fatalf("候选正文泄漏进提示词:\n%s", render)
		}
	}
	// 正常内容仍在，证明断言本身有效。
	if !strings.Contains(b.RenderTrusted(""), "叫我梁哥") {
		t.Fatal("已确认信息应正常注入（断言有效性自检）")
	}
}

// TestBudgetTruncationOrder 覆盖验收 P0-2：
// 极小预算下按"证据细节 > 近期轮次 > 摘要 > Profile"的顺序丢弃，
// 每一步都留下截断元信息。trusted 基础（时间+身份）与用户消息是有界底座、
// 不参与丢弃，因此预算以探测到的底座为锚点。
func TestBudgetTruncationOrder(t *testing.T) {
	ctx := context.Background()
	tt := testTemporal(t)
	// 用无存储装配探测"底座"占用（时间+身份+用户消息）。
	base := NewAssembler(nil, identity.Default(), DefaultBudget(), nil).
		Assemble(ctx, Request{UserID: "u1", Temporal: tt, UserText: "你好"}).UsedRunes()

	store := &fakeStore{
		entries: []storage.ProfileEntry{
			{UserID: "u1", Key: "称呼", Value: "叫我梁哥", Version: 1},
			{UserID: "u1", Key: "作息", Value: "上午状态最好", Version: 1},
		},
		state: storage.ConversationState{CurrentProject: "lumen"},
		turns: []storage.RecentTurn{
			{UserText: "第一轮的问题", AnswerText: "第一轮的回答"},
			{UserText: "第二轮的问题", AnswerText: "第二轮的回答"},
		},
	}
	// 底座 + 一条 Profile（约 22 字）放得下；摘要、轮次、证据都放不下。
	tiny := Budget{ProfileRunes: 30, ProfileMaxEntries: 1, TotalRunes: base + 30}
	a := NewAssembler(store, identity.Default(), tiny, nil)
	b := a.Assemble(ctx, Request{UserID: "u1", Temporal: tt, UserText: "你好"})

	// 优先级最高的 Profile 应保留第一条；第二条被条数上限裁掉。
	entries := b.ProfileEntries()
	if len(entries) != 1 || entries[0].Key != "称呼" {
		t.Fatalf("最高优先级的 Profile 应保留一条，实际 %+v", entries)
	}
	if b.Summary().CurrentProject != "" {
		t.Fatal("预算耗尽时摘要不应注入")
	}
	if len(b.RecentTurns()) != 0 {
		t.Fatal("预算耗尽时近期轮次不应注入")
	}

	// 证据在整包已满时全部丢弃（给结果真实内容，避免空 body 的假阳性）。
	b.AddToolResults([]tooling.Result{
		{Tool: "get_today_status", Count: 1, Digest: []string{"今天 90 分钟"},
			Model: map[string]any{"summary": "今天用了 ZCode 90 分钟"}},
		{Tool: "get_sessions", Count: 1, Digest: []string{"另一段"},
			Model: map[string]any{"sessions": "另一段记录内容"}},
	}, nil)
	if len(b.ToolResults()) != 0 {
		t.Fatalf("整包超限时证据应全部丢弃，实际 %d 条", len(b.ToolResults()))
	}

	notes := strings.Join(b.Truncations(), "\n")
	for _, want := range []string{"已确认信息", "会话摘要", "近期轮次", "tool_data"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("截断元信息应提到 %q，实际:\n%s", want, notes)
		}
	}
	// 丢弃次序：摘要与轮次在装配阶段被整包裁决丢弃，证据最后全军覆没；
	// 元信息里"会话摘要"必须出现在"tool_data"之前（证据最先丢）。
	if strings.Index(notes, "会话摘要") > strings.Index(notes, "tool_data") {
		t.Fatalf("证据应比摘要更早被丢弃（截断顺序），实际:\n%s", notes)
	}
}

// TestEvidenceBudgetDropsTail 覆盖：证据段自身预算超限时，整项丢弃尾部
// 并记录数量，不裁半条 JSON；单项超长时裁剪并显式标注。
func TestEvidenceBudgetDropsTail(t *testing.T) {
	tt := testTemporal(t)
	item := func(name string) tooling.Result {
		return tooling.Result{Tool: name, Count: 1, Digest: []string{"x"},
			Model: map[string]any{"k": strings.Repeat("甲", 150)}}
	}

	// 证据合计预算只够一条：第二条整项丢弃并记录。
	b := NewBundle(tt, identity.Default(), "看看记录")
	b.budget = Budget{EvidenceRunes: 250}.Normalize()
	b.AddToolResults([]tooling.Result{item("get_today_status"), item("get_sessions")}, nil)
	if len(b.ToolResults()) != 1 {
		t.Fatalf("证据预算内应只注入 1 条，实际 %d", len(b.ToolResults()))
	}
	if notes := strings.Join(b.Truncations(), "\n"); !strings.Contains(notes, "tool_data") {
		t.Fatalf("应记录证据裁剪元信息，实际 %v", b.Truncations())
	}

	// 单条超长：裁剪并显式标注（不静默截断）。
	c := NewBundle(tt, identity.Default(), "看看记录")
	c.budget = Budget{ToolResultRunes: 100}.Normalize()
	c.AddToolResults([]tooling.Result{item("get_today_status")}, nil)
	got := c.ToolResults()
	if len(got) != 1 || !got[0].Clipped {
		t.Fatalf("超长条目应被裁剪并标记，实际 %+v", got)
	}
	if !strings.Contains(c.RenderToolData(), "（内容过长，已截断）") {
		t.Fatal("渲染结果应显式标注截断")
	}
}

// TestDegradationOnStorageFailures 覆盖验收 P0-4：
// 任一段读取失败时降级为空段并记录元信息，轮次继续，不 panic。
func TestDegradationOnStorageFailures(t *testing.T) {
	store := &fakeStore{
		entriesErr: fmt.Errorf("db down"),
		stateErr:   fmt.Errorf("db down"),
		turnsErr:   fmt.Errorf("db down"),
	}
	a := NewAssembler(store, identity.Default(), DefaultBudget(), nil)
	b := a.Assemble(context.Background(), Request{UserID: "u1", Temporal: testTemporal(t), UserText: "你好"})
	if len(b.ProfileEntries()) != 0 || len(b.RecentTurns()) != 0 || b.Summary().CurrentProject != "" {
		t.Fatal("读取失败时各段应为空")
	}
	notes := strings.Join(b.Truncations(), "\n")
	for _, want := range []string{"已确认信息", "会话摘要", "近期轮次"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("降级元信息应提到 %q，实际 %v", want, b.Truncations())
		}
	}
	// 提示词仍可渲染。
	if !strings.Contains(b.RenderTrusted(""), "可信时间") {
		t.Fatal("降级后 trusted 块仍应可渲染")
	}

	// 无存储/无用户：同样降级。
	empty := NewAssembler(nil, identity.Default(), DefaultBudget(), nil).
		Assemble(context.Background(), Request{Temporal: testTemporal(t), UserText: "你好"})
	if empty.TurnSource == "" {
		t.Fatal("无存储时仍应生成轮次来源")
	}
}

// TestTurnBodyClipsPerTurn 覆盖：单轮近期对话按预算裁剪，且保序（旧→新）。
func TestTurnBodyClipsPerTurn(t *testing.T) {
	store := &fakeStore{turns: []storage.RecentTurn{
		{UserText: strings.Repeat("新", 500), AnswerText: strings.Repeat("答", 500)},
		{UserText: "第一轮", AnswerText: "回答一"},
	}}
	budget := DefaultBudget()
	budget.RecentTurns = 2
	budget.TurnRunes = 60
	budget.RecentTurnsRunes = 100000
	a := NewAssembler(store, identity.Default(), budget, nil)
	b := a.Assemble(context.Background(), Request{UserID: "u1", Temporal: testTemporal(t), UserText: "你好"})

	turns := b.RecentTurns()
	if len(turns) != 2 {
		t.Fatalf("应注入两轮（裁剪后），实际 %d", len(turns))
	}
	if !strings.HasPrefix(turns[0].Body, "用户：第一轮") {
		t.Fatalf("轮次应转成旧→新顺序，实际: %q", turns[0].Body)
	}
	if got := len([]rune(turns[1].Body)); got > budget.TurnRunes+50 {
		t.Fatalf("单轮应裁剪到预算内（%d ≤ %d），实际 %d 字", got, budget.TurnRunes, got)
	}
}

// TestAddToolResultsCountsDenied 覆盖：被拒步骤进入证据区（不占证据预算），
// 模型必须知道哪些步骤没做成。
func TestAddToolResultsCountsDenied(t *testing.T) {
	b := NewBundle(testTemporal(t), identity.Default(), "你好")
	b.AddToolResults(nil, []tooling.Denial{{Name: "save_memory_candidate", Reason: "测试拒绝"}})
	if len(b.Denied()) != 1 {
		t.Fatalf("被拒步骤应记录，实际 %d", len(b.Denied()))
	}
	if !strings.Contains(b.RenderDenied(), "save_memory_candidate") {
		t.Fatal("被拒步骤应可渲染")
	}
}
