package sessions

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"lumen/server/internal/storage"
	"lumen/server/internal/ulid"
)

var shanghai = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// newTestStore 创建一个临时 SQLite 数据库用于 Session 测试。
func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// insertWindow 写入一条窗口活动事件。
func insertWindow(t *testing.T, store *storage.Store, at time.Time, durSec int, app, project string) {
	t.Helper()
	ctx := map[string]any{"app": app}
	if project != "" {
		ctx["project"] = project
	}
	insertEvent(t, store, "window.activity", at, "P0", ctx, map[string]any{"duration_seconds": durSec})
}

// insertIdle 写入一条空闲事件。
//
// 语义与采集端一致：timestamp 是区间**开始**时刻，duration_seconds 是区间长度。
// 也就是说 insertIdle(at, state, dur) 表示「从 at 开始、持续 dur 秒的 state 区间」。
func insertIdle(t *testing.T, store *storage.Store, at time.Time, state string, durSec int) {
	t.Helper()
	data := map[string]any{"state": state, "schema_version": 2}
	if durSec > 0 {
		data["duration_seconds"] = durSec
	}
	insertEvent(t, store, "idle.state", at, "P0", map[string]any{}, data)
}

// insertGit 写入一条 Git 事件。
func insertGit(t *testing.T, store *storage.Store, at time.Time, repo, message string) {
	t.Helper()
	insertEvent(t, store, "git.activity", at, "P1",
		map[string]any{"repo": repo, "project": repo},
		map[string]any{"kind": "commit", "branch": "main", "commit_message": message})
}

func insertEvent(t *testing.T, store *storage.Store, typ string, at time.Time, privacy string,
	ctxMap, dataMap map[string]any) {
	t.Helper()
	ctxJSON, _ := json.Marshal(ctxMap)
	dataJSON, _ := json.Marshal(dataMap)
	// 使用 ULID 生成唯一 id，避免测试之间互相覆盖。
	id := ulid.New()
	if _, err := store.InsertEvent(context.Background(), storage.Event{
		ID: id, DeviceID: "desktop-mac-01", Type: typ,
		Timestamp: at, ReceivedAt: at, PrivacyLevel: privacy,
		ContextJSON: string(ctxJSON), DataJSON: string(dataJSON), BatchID: "test-batch",
	}); err != nil {
		t.Fatalf("写入事件失败: %v", err)
	}
}

func TestMergeSameProjectWithinGap(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	// 两段活动间隔 3 分钟（小于 8 分钟阈值）且同项目，应合并。
	insertWindow(t, store, day.Add(9*time.Hour), 600, "VS Code", "lumen")
	insertWindow(t, store, day.Add(9*time.Hour+10*time.Minute+3*time.Minute), 600, "VS Code", "lumen")

	ctx := context.Background()
	if _, err := eng.RebuildDay(ctx, day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list, err := store.SessionsByDate(ctx, "2026-09-17")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("间隔 3 分钟应合并为 1 个 Session，实际 %d 个", len(list))
	}
	if list[0].Project != "lumen" {
		t.Fatalf("项目应为 lumen，实际 %s", list[0].Project)
	}
}

func TestIdleBreaksSession(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	insertWindow(t, store, day.Add(9*time.Hour), 600, "VS Code", "lumen")
	// 10 分钟 idle，超过 8 分钟阈值，必须切断。
	insertIdle(t, store, day.Add(9*time.Hour+10*time.Minute), "idle", 600)
	insertWindow(t, store, day.Add(9*time.Hour+25*time.Minute), 600, "VS Code", "lumen")

	ctx := context.Background()
	if _, err := eng.RebuildDay(ctx, day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list, _ := store.SessionsByDate(ctx, "2026-09-17")
	if len(list) != 2 {
		t.Fatalf("idle 10 分钟应切断为 2 个 Session，实际 %d 个", len(list))
	}
}

func TestIdleBelowThresholdDoesNotBreak(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	insertWindow(t, store, day.Add(9*time.Hour), 600, "VS Code", "lumen")
	// idle 只持续 5 分钟，不足 8 分钟，不构成切断；且两段间隔也小于阈值。
	insertIdle(t, store, day.Add(9*time.Hour+10*time.Minute), "idle", 300)
	insertWindow(t, store, day.Add(9*time.Hour+16*time.Minute), 600, "VS Code", "lumen")

	ctx := context.Background()
	if _, err := eng.RebuildDay(ctx, day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list, _ := store.SessionsByDate(ctx, "2026-09-17")
	if len(list) != 1 {
		t.Fatalf("idle 5 分钟不应切断，期望 1 个 Session，实际 %d 个", len(list))
	}
}

func TestProjectSwitchCreatesSeparateSessions(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	insertWindow(t, store, day.Add(9*time.Hour), 600, "VS Code", "lumen")
	insertWindow(t, store, day.Add(9*time.Hour+11*time.Minute), 600, "VS Code", "clipmaster")

	ctx := context.Background()
	_, _ = eng.RebuildDay(ctx, day)
	list, _ := store.SessionsByDate(ctx, "2026-09-17")
	if len(list) != 2 {
		t.Fatalf("项目切换应产生 2 个 Session，实际 %d 个", len(list))
	}
	projects := map[string]bool{}
	for _, s := range list {
		projects[s.Project] = true
	}
	if !projects["lumen"] || !projects["clipmaster"] {
		t.Fatalf("项目归属错误: %+v", projects)
	}
}

func TestUnclassifiedWhenProjectUnknown(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	// 没有 git 事件，也没有 project 字段，应标为 unclassified 而不是猜测。
	insertWindow(t, store, day.Add(9*time.Hour), 900, "Safari", "")

	ctx := context.Background()
	_, _ = eng.RebuildDay(ctx, day)
	list, _ := store.SessionsByDate(ctx, "2026-09-17")
	if len(list) != 1 {
		t.Fatalf("期望 1 个 Session，实际 %d", len(list))
	}
	if list[0].Project != Unclassified {
		t.Fatalf("项目不明时应为 %s，实际 %s", Unclassified, list[0].Project)
	}
}

// insertGitWorkspace 写入一条 kind=workspace 的 Git 事件（首次扫描时的基线状态）。
func insertGitWorkspace(t *testing.T, store *storage.Store, at time.Time, repo string) {
	t.Helper()
	insertEvent(t, store, "git.activity", at, "P1",
		map[string]any{"repo": repo, "project": repo},
		map[string]any{"kind": "workspace", "branch": "main"})
}

// TestWorkspaceBaselineDoesNotAttributeProject 是回归测试。
//
// 首次启动时采集端会对每个仓库各产生一条 workspace 基线事件（时间几乎相同）。
// 如果把它们当作归因证据，那么同一时刻的窗口活动会被算到"时间上最近的那个仓库"，
// 而完全不管用户实际在做什么。真实踩到过这个 bug：用户在 ZCode 里工作，
// 却被归到了另一个仓库的项目上。
//
// workspace 是扫描快照，不是用户动作，因此不参与归因。
func TestWorkspaceBaselineDoesNotAttributeProject(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	at := day.Add(20*time.Hour + 17*time.Minute)
	// 模拟首次扫描：四个仓库同一时刻各产生一条 workspace 基线。
	for _, repo := range []string{"ClipMaster-Pro", "LinguaForge", "ai-picture-editor", "loglens"} {
		insertGitWorkspace(t, store, at, repo)
	}
	// 同一时刻用户实际在一个没有项目标识的应用里工作。
	insertWindow(t, store, at, 60, "ZCode", "")

	ctx := context.Background()
	_, _ = eng.RebuildDay(ctx, day)
	list, _ := store.SessionsByDate(ctx, "2026-09-17")
	if len(list) != 1 {
		t.Fatalf("期望 1 个 Session，实际 %d", len(list))
	}
	if list[0].Project != Unclassified {
		t.Fatalf("workspace 基线不应参与项目归因，期望 %s 实际 %s",
			Unclassified, list[0].Project)
	}
}

// TestCommitStillAttributesAfterWorkspaceFiltered 确认过滤 workspace 后 commit 仍能归因。
func TestCommitStillAttributesAfterWorkspaceFiltered(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	at := day.Add(10 * time.Hour)
	// 基线事件（不应归因）与真实提交（应归因）同时存在。
	insertGitWorkspace(t, store, at, "other-repo")
	insertGit(t, store, at.Add(2*time.Minute), "lumen", "feat: real commit")
	insertWindow(t, store, at.Add(time.Minute), 900, "ZCode", "")

	ctx := context.Background()
	_, _ = eng.RebuildDay(ctx, day)
	list, _ := store.SessionsByDate(ctx, "2026-09-17")
	if len(list) != 1 {
		t.Fatalf("期望 1 个 Session，实际 %d", len(list))
	}
	if list[0].Project != "lumen" {
		t.Fatalf("应归因到有真实提交的 lumen，实际 %s", list[0].Project)
	}
}

// TestGitAttributionDoesNotGrabUnrelatedActivity 验证 Git 归因窗口足够窄。
// 提交前后很久的无关活动（例如浏览器）不应被算进该项目。
func TestGitAttributionDoesNotGrabUnrelatedActivity(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	// 上午 9 点有 Git 提交。
	insertGit(t, store, day.Add(9*time.Hour), "lumen", "feat: something")
	// 中午 12 点在浏览器上活动（距离提交 3 小时，远超归因窗口）。
	insertWindow(t, store, day.Add(12*time.Hour), 1200, "Safari", "")

	ctx := context.Background()
	_, _ = eng.RebuildDay(ctx, day)
	list, _ := store.SessionsByDate(ctx, "2026-09-17")
	if len(list) != 1 {
		t.Fatalf("期望 1 个 Session，实际 %d", len(list))
	}
	if list[0].Project != Unclassified {
		t.Fatalf("距离 Git 活动 3 小时的浏览行为不应归入项目，期望 %s 实际 %s",
			Unclassified, list[0].Project)
	}
}

// TestGitAttributionWindowIsNarrow 验证窗口边界：窗口内归因、窗口外不归因。
func TestGitAttributionWindowIsNarrow(t *testing.T) {
	if GitAttributionWindow > 30*time.Minute {
		t.Fatalf("归因窗口过大（%s），会把无关活动错误归入项目", GitAttributionWindow)
	}

	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	// 窗口内：提交前 10 分钟的活动应被归因。
	insertGit(t, store, day.Add(9*time.Hour), "lumen", "feat: x")
	insertWindow(t, store, day.Add(8*time.Hour+50*time.Minute), 600, "Terminal", "")

	ctx := context.Background()
	_, _ = eng.RebuildDay(ctx, day)
	list, _ := store.SessionsByDate(ctx, "2026-09-17")
	if len(list) != 1 {
		t.Fatalf("期望 1 个 Session，实际 %d", len(list))
	}
	if list[0].Project != "lumen" {
		t.Fatalf("提交前 10 分钟的活动应归因到 lumen，实际 %s", list[0].Project)
	}
}

func TestGitEventAttributesProject(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	// 窗口事件没有项目信息，但附近有 git 活动，应归属到仓库。
	insertWindow(t, store, day.Add(9*time.Hour), 900, "Terminal", "")
	insertGit(t, store, day.Add(9*time.Hour+2*time.Minute), "lumen", "feat: session engine")

	ctx := context.Background()
	_, _ = eng.RebuildDay(ctx, day)
	list, _ := store.SessionsByDate(ctx, "2026-09-17")
	if len(list) != 1 {
		t.Fatalf("期望 1 个 Session，实际 %d", len(list))
	}
	if list[0].Project != "lumen" {
		t.Fatalf("应由 git 事件归属到 lumen，实际 %s", list[0].Project)
	}
	if list[0].GitJSON == "[]" {
		t.Fatal("Session 应包含 git 证据")
	}
}

func TestRebuildIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	insertWindow(t, store, day.Add(9*time.Hour), 600, "VS Code", "lumen")
	insertWindow(t, store, day.Add(9*time.Hour+11*time.Minute), 600, "VS Code", "lumen")

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := eng.RebuildDay(ctx, day); err != nil {
			t.Fatalf("第 %d 次重建失败: %v", i+1, err)
		}
	}
	list, _ := store.SessionsByDate(ctx, "2026-09-17")
	if len(list) != 1 {
		t.Fatalf("重复重建不应产生重复 Session，实际 %d 个", len(list))
	}
}

func TestShortFragmentIsFiltered(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	// 30 秒的碎片不足以成为独立 Session。
	insertWindow(t, store, day.Add(9*time.Hour), 30, "Finder", "")

	ctx := context.Background()
	_, _ = eng.RebuildDay(ctx, day)
	list, _ := store.SessionsByDate(ctx, "2026-09-17")
	if len(list) != 0 {
		t.Fatalf("30 秒碎片不应生成 Session，实际 %d 个", len(list))
	}
}

func TestDayRangeUsesLocalTimezone(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 15, 30, 0, 0, shanghai)

	from, to := eng.DayRange(day)
	// 上海 9-17 00:00 对应 UTC 9-16 16:00。
	wantFrom := time.Date(2026, 9, 16, 16, 0, 0, 0, time.UTC)
	if !from.Equal(wantFrom) {
		t.Fatalf("起始时间错误: want %s got %s", wantFrom, from)
	}
	if to.Sub(from) != 24*time.Hour {
		t.Fatalf("区间长度应为 24 小时，实际 %s", to.Sub(from))
	}
}
