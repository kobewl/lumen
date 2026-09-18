package sessions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"lumen/server/internal/storage"
	"lumen/server/internal/ulid"
)

// insertAway 写入一条 idle.state 事件。
//
// version >= 2 表示新语义（timestamp 是区间开始，duration 是区间长度）；
// version == 0 表示旧语义（timestamp 是区间结束时刻，duration 是区间长度），
// 也就是 2026-09-17 真机上产生的那批历史数据。
func insertAway(t *testing.T, store *storage.Store, at time.Time, state string, durSec, version int) {
	t.Helper()
	data := map[string]any{"state": state}
	if durSec > 0 {
		data["duration_seconds"] = durSec
	}
	if version > 0 {
		data["schema_version"] = version
	}
	insertEvent(t, store, "idle.state", at, "P0", map[string]any{}, data)
}

// sessionEnds 返回某天的 Session 列表，附带断言用的辅助。
func sessionEnds(t *testing.T, store *storage.Store, date string) []storage.Session {
	t.Helper()
	list, err := store.SessionsByDate(context.Background(), date)
	if err != nil {
		t.Fatalf("查询 Session 失败: %v", err)
	}
	return list
}

// TestLockedIntervalCutsSessionAtStart 是核心回归测试。
//
// 真机证据：用户 20:42 锁屏，服务端却把锁屏后的时间继续算作工作。
// 原因之一是切断点方向错误——锁屏区间 [10:00, 12:00] 的切断点必须落在
// **区间开始**（10:00，用户真正离开的时刻），而不是区间结束（12:00）。
func TestLockedIntervalCutsSessionAtStart(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	insertWindow(t, store, day.Add(9*time.Hour), 3600, "VS Code", "lumen")
	// 10:00 锁屏，12:00 解锁（新语义）。窗口段在锁屏时刻被采集端关闭，
	// 所以最后一条活动正好结束在 10:00。
	insertAway(t, store, day.Add(10*time.Hour), "locked", 7200, 2)
	insertWindow(t, store, day.Add(13*time.Hour), 1800, "VS Code", "lumen")

	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list := sessionEnds(t, store, "2026-09-17")
	if len(list) != 2 {
		t.Fatalf("锁屏应切断为 2 个 Session，实际 %d 个", len(list))
	}
	lockAt := day.Add(10 * time.Hour).UTC()
	if list[0].EndAt.After(lockAt) {
		t.Fatalf("第一个 Session 不得越过锁屏时刻（10:00），实际结束于 %s", list[0].EndAt.In(shanghai))
	}
	if !list[0].EndAt.Equal(lockAt) {
		t.Fatalf("第一个 Session 应在锁屏时刻结束（10:00），实际 %s", list[0].EndAt.In(shanghai))
	}
	// 第二个 Session 必须从解锁之后开始，不能回吞锁屏期间。
	if list[1].StartAt.Before(day.Add(12 * time.Hour).UTC()) {
		t.Fatalf("第二个 Session 不应早于解锁时刻，实际 %s", list[1].StartAt.In(shanghai))
	}
}

// TestIdleIntervalCutsAtStart 验证 idle 切断点同样落在 idle 开始处。
func TestIdleIntervalCutsAtStart(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	insertWindow(t, store, day.Add(14*time.Hour), 1800, "ZCode", "lumen")
	// 14:30 起 idle 20 分钟（超过 8 分钟阈值）。
	insertAway(t, store, day.Add(14*time.Hour+30*time.Minute), "idle", 1200, 2)
	insertWindow(t, store, day.Add(15*time.Hour+30*time.Minute), 1800, "ZCode", "lumen")

	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list := sessionEnds(t, store, "2026-09-17")
	if len(list) != 2 {
		t.Fatalf("idle 超过阈值应切断为 2 个 Session，实际 %d 个", len(list))
	}
	want := day.Add(14*time.Hour + 30*time.Minute).UTC()
	if !list[0].EndAt.Equal(want) {
		t.Fatalf("第一个 Session 应在 idle 开始时结束，实际 %s", list[0].EndAt.In(shanghai))
	}
}

// TestActivityInsideLockIsClipped 验证落在锁屏区间内的窗口活动被裁剪掉。
//
// 真机上采集端曾把 loginwindow 当成前台应用连续记录，历史数据里
// 有整夜的假活动。重算时必须忽略它们，但**不删除原始事件**。
func TestActivityInsideLockIsClipped(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	// 真实工作：09:00 ~ 10:00。
	insertWindow(t, store, day.Add(9*time.Hour), 3600, "ZCode", "lumen")
	// 10:00 锁屏到 12:00。
	insertAway(t, store, day.Add(10*time.Hour), "locked", 7200, 2)
	// 脏数据：锁屏期间还在记录 loginwindow。
	for i := 0; i < 120; i++ {
		insertWindow(t, store, day.Add(10*time.Hour+time.Duration(i)*time.Minute), 60,
			"loginwindow", "")
	}
	// 解锁后继续工作。
	insertWindow(t, store, day.Add(12*time.Hour+10*time.Minute), 1800, "ZCode", "lumen")

	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list := sessionEnds(t, store, "2026-09-17")
	if len(list) != 2 {
		t.Fatalf("锁屏前后应是 2 个 Session，实际 %d 个", len(list))
	}
	for _, s := range list {
		if s.StartAt.Before(day.Add(10*time.Hour)) && s.EndAt.After(day.Add(10*time.Hour)) {
			t.Fatalf("Session 不得跨越锁屏区间: %s ~ %s", s.StartAt.In(shanghai), s.EndAt.In(shanghai))
		}
		var apps []struct {
			App string `json:"app"`
		}
		_ = json.Unmarshal([]byte(s.AppsJSON), &apps)
		for _, a := range apps {
			if a.App == "loginwindow" {
				t.Fatalf("锁屏期间的脏活动不得计入 Session: %s", s.AppsJSON)
			}
		}
	}
	// 总时长 = 1 小时 + 30 分钟，绝不能被整夜假活动放大。
	var total float64
	for _, s := range list {
		var stats struct {
			DurationMinutes float64 `json:"duration_minutes"`
		}
		_ = json.Unmarshal([]byte(s.StatsJSON), &stats)
		total += stats.DurationMinutes
	}
	if total > 95 {
		t.Fatalf("总时长不应超过真实工作时间，实际 %.1f 分钟", total)
	}
}

// TestLegacyIdleEventUsesEndTimestamp 是历史脏数据的回归测试。
//
// 2026-09-17 真机数据：{"state":"locked","duration_seconds":45094}，
// timestamp 是**解锁时刻** 2026-09-18T01:14:04Z，区间实际是
// 2026-09-17T12:42:30Z ~ 2026-09-18T01:14:04Z（本地 20:42 ~ 次日 09:14）。
//
// 如果按新语义误读成「从 09:14 起锁屏 12.5 小时」，那么当天上午的工作
// 会被整段抹掉；正确做法是按旧语义把区间还原到解锁之前。
func TestLegacyIdleEventUsesEndTimestamp(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)

	unlock := time.Date(2026, 9, 18, 1, 14, 4, 0, time.UTC) // 本地 09:14
	durSec := 45094
	insertAway(t, store, unlock, "locked", durSec, 0)

	// 解锁后的真实工作：本地 09:20 起 40 分钟。
	workStart := time.Date(2026, 9, 18, 9, 20, 0, 0, shanghai)
	insertWindow(t, store, workStart, 2400, "ZCode", "lumen")

	// 脏数据：解锁前的整夜 loginwindow 假活动（前一天的记录）。
	dirtyStart := time.Date(2026, 9, 17, 12, 42, 30, 0, time.UTC)
	for i := 0; i < 200; i++ {
		insertWindow(t, store, dirtyStart.Add(time.Duration(i)*time.Minute), 60, "loginwindow", "")
	}

	day := time.Date(2026, 9, 18, 0, 0, 0, 0, shanghai)
	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list := sessionEnds(t, store, "2026-09-18")
	if len(list) != 1 {
		t.Fatalf("解锁后应只有 1 个工作 Session，实际 %d 个", len(list))
	}
	if !list[0].StartAt.Equal(workStart.UTC()) {
		t.Fatalf("Session 应从解锁后的工作时刻开始，实际 %s", list[0].StartAt.In(shanghai))
	}
}

// TestPseudoAppsAreNeverWork 验证系统伪应用在重算时被忽略。
func TestPseudoAppsAreNeverWork(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	for _, app := range []string{"loginwindow", "ScreenSaverEngine", "SecurityAgent"} {
		insertWindow(t, store, day.Add(10*time.Hour), 600, app, "")
	}
	insertWindow(t, store, day.Add(11*time.Hour), 600, "ZCode", "lumen")

	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list := sessionEnds(t, store, "2026-09-17")
	if len(list) != 1 {
		t.Fatalf("只应保留真实应用的 1 个 Session，实际 %d 个", len(list))
	}
	if list[0].StartAt.Equal(day.Add(10 * time.Hour).UTC()) {
		t.Fatal("系统伪应用不应出现在 Session 里")
	}
}

// TestCrossDayLockDoesNotPolluteNextDay 验证跨日锁屏不污染次日重建。
//
// 场景：22:42 锁屏，次日 09:41 解锁。次日重建时：
//   - 锁屏期间的假活动（07:00 的 loginwindow）必须被忽略；
//   - 解锁后的真实工作必须出现，且不能延展回解锁之前。
func TestCrossDayLockDoesNotPolluteNextDay(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)

	lockAt := time.Date(2026, 9, 17, 22, 42, 30, 0, shanghai)
	insertAway(t, store, lockAt, "locked", int(12*time.Hour/time.Second), 2)

	// 次日凌晨的假活动与次日上午的真实工作。
	dirty := time.Date(2026, 9, 18, 7, 0, 0, 0, shanghai)
	for i := 0; i < 60; i++ {
		insertWindow(t, store, dirty.Add(time.Duration(i)*time.Minute), 60, "loginwindow", "")
	}
	unlock := lockAt.Add(12 * time.Hour) // 次日 10:42
	insertWindow(t, store, unlock.Add(20*time.Minute), 1800, "ZCode", "lumen")

	day := time.Date(2026, 9, 18, 0, 0, 0, 0, shanghai)
	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list := sessionEnds(t, store, "2026-09-18")
	if len(list) != 1 {
		t.Fatalf("次日应只有 1 个 Session，实际 %d 个", len(list))
	}
	if !list[0].StartAt.Equal(unlock.Add(20 * time.Minute).UTC()) {
		t.Fatalf("Session 应从解锁后的工作时刻开始，实际 %s", list[0].StartAt.In(shanghai))
	}
}

// TestOpenAwayIntervalDoesNotBlockResumedWork 验证「未闭合区间」不会吃掉后续工作。
//
// 采集端重启或进程被杀时，可能只留下一个未闭合的 locked 区间。
// 此时只要之后出现了真实窗口活动，就说明用户已经回来了，
// 区间应按「下一条活动的开始时刻」收口，而不能一直延续下去。
func TestOpenAwayIntervalDoesNotBlockResumedWork(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)

	lockAt := time.Date(2026, 9, 18, 8, 0, 0, 0, shanghai)
	insertAway(t, store, lockAt, "locked", 0, 2) // 未闭合区间，没有时长
	// 两段活动相隔 5 分钟（小于 8 分钟阈值），正常应合并。
	insertWindow(t, store, time.Date(2026, 9, 18, 9, 0, 0, 0, shanghai), 1800, "ZCode", "lumen")
	insertWindow(t, store, time.Date(2026, 9, 18, 9, 35, 0, 0, shanghai), 1500, "ZCode", "lumen")

	day := time.Date(2026, 9, 18, 0, 0, 0, 0, shanghai)
	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list := sessionEnds(t, store, "2026-09-18")
	if len(list) != 1 {
		t.Fatalf("用户已经回来工作，应保留 1 个 Session，实际 %d 个", len(list))
	}
	want := time.Date(2026, 9, 18, 9, 0, 0, 0, shanghai).UTC()
	if !list[0].StartAt.Equal(want) {
		t.Fatalf("Session 应从恢复工作时刻开始，实际 %s", list[0].StartAt.In(shanghai))
	}
	var stats struct {
		DurationMinutes float64 `json:"duration_minutes"`
	}
	_ = json.Unmarshal([]byte(list[0].StatsJSON), &stats)
	if stats.DurationMinutes < 54 || stats.DurationMinutes > 56 {
		t.Fatalf("应保留两段真实工作共 55 分钟，实际 %.1f", stats.DurationMinutes)
	}
}

// TestAwayIntervalIsNotAffectedByIngestOrder 验证重算结果与事件到达顺序无关。
func TestAwayIntervalIsNotAffectedByIngestOrder(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	// 先写入较晚的事件，再写入较早的事件（模拟补传迟到数据）。
	insertWindow(t, store, day.Add(13*time.Hour), 1800, "ZCode", "lumen")
	insertAway(t, store, day.Add(10*time.Hour), "locked", 7200, 2)
	insertWindow(t, store, day.Add(9*time.Hour), 1800, "ZCode", "lumen")

	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list := sessionEnds(t, store, "2026-09-17")
	if len(list) != 2 {
		t.Fatalf("应切断为 2 个 Session，实际 %d 个", len(list))
	}
}

// TestLegacyWindowActivityStillWorks 验证窗口活动仍是「timestamp = 开始时刻」，
// 语义变化只影响 idle.state。
func TestLegacyWindowActivityStillWorks(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	insertWindow(t, store, day.Add(9*time.Hour), 600, "ZCode", "lumen")
	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list := sessionEnds(t, store, "2026-09-17")
	if len(list) != 1 {
		t.Fatalf("期望 1 个 Session，实际 %d 个", len(list))
	}
	if !list[0].StartAt.Equal(day.Add(9 * time.Hour).UTC()) {
		t.Fatalf("窗口活动起点错误: %s", list[0].StartAt.In(shanghai))
	}
}

// TestIdleStateWithoutState 忽略缺字段或非法状态的 idle 事件，不 panic。
func TestIdleStateWithoutState(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	insertEvent(t, store, "idle.state", day.Add(9*time.Hour), "P0", map[string]any{}, map[string]any{})
	insertWindow(t, store, day.Add(9*time.Hour), 600, "ZCode", "lumen")

	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	if got := len(sessionEnds(t, store, "2026-09-17")); got != 1 {
		t.Fatalf("期望 1 个 Session，实际 %d 个", got)
	}
}

// TestAwayIntervalHelpers 覆盖区间构建的边界情况。
func TestAwayIntervalHelpers(t *testing.T) {
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	// 新语义：没有时长的 idle 事件是开放区间，起点即 timestamp。
	open := storage.Event{
		ID: ulid.New(), Type: "idle.state", Timestamp: day.Add(9 * time.Hour),
		DataJSON: `{"state":"idle","schema_version":2}`,
	}
	iv, ok := awayIntervalOf(open)
	if !ok || !iv.start.Equal(day.Add(9*time.Hour).UTC()) || !iv.end.IsZero() {
		t.Fatalf("开放区间解析错误: %+v ok=%v", iv, ok)
	}

	// 旧语义：timestamp 是区间结束。
	legacy := storage.Event{
		ID: ulid.New(), Type: "idle.state", Timestamp: day.Add(10 * time.Hour),
		DataJSON: `{"state":"locked","duration_seconds":3600}`,
	}
	iv, ok = awayIntervalOf(legacy)
	if !ok || !iv.start.Equal(day.Add(9*time.Hour).UTC()) || !iv.end.Equal(day.Add(10*time.Hour).UTC()) {
		t.Fatalf("旧语义区间解析错误: %+v ok=%v", iv, ok)
	}

	// active 不是「不在」状态。
	active := storage.Event{
		ID: ulid.New(), Type: "idle.state", Timestamp: day.Add(11 * time.Hour),
		DataJSON: `{"state":"active","schema_version":2}`,
	}
	if _, ok := awayIntervalOf(active); ok {
		t.Fatal("active 不应被当作离开区间")
	}
}

// TestRebuildDoesNotLeakPreviousDaySessions 验证重算某天不会把前一天的活动
// 写成当天的 Session。
//
// 为了让跨夜的离开区间可见，重算会向前多看 24 小时；如果不按「开始时刻所在
// 的本地日期」过滤合并结果，前一天下午的工作会被写成今天的 Session
// （真机复现：查 09-14 的第一个时段起点是 09-13 14:00）。
func TestRebuildDoesNotLeakPreviousDaySessions(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)

	prevDay := time.Date(2026, 9, 13, 14, 0, 0, 0, shanghai)
	insertWindow(t, store, prevDay, 3600, "ZCode", "lumen")
	// 次日：跨夜的锁屏区间（起点在前一天晚上，次日 09:00 解锁）。
	insertAway(t, store, time.Date(2026, 9, 13, 22, 0, 0, 0, shanghai), "locked",
		int(11*time.Hour/time.Second), 2)
	insertWindow(t, store, time.Date(2026, 9, 14, 9, 30, 0, 0, shanghai), 1800, "ZCode", "lumen")

	day := time.Date(2026, 9, 14, 0, 0, 0, 0, shanghai)
	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}

	for _, s := range sessionEnds(t, store, "2026-09-14") {
		if got := s.StartAt.In(shanghai).Format("2006-01-02"); got != "2026-09-14" {
			t.Fatalf("9-14 的 Session 不应包含 9-13 的活动，实际起点 %s", s.StartAt.In(shanghai))
		}
	}
	if len(sessionEnds(t, store, "2026-09-14")) != 1 {
		t.Fatalf("9-14 应只有 1 个 Session，实际 %d 个", len(sessionEnds(t, store, "2026-09-14")))
	}
	// 前一天自己的重算结果不受影响。
	if _, err := eng.RebuildDay(context.Background(), time.Date(2026, 9, 13, 0, 0, 0, 0, shanghai)); err != nil {
		t.Fatalf("重建 9-13 失败: %v", err)
	}
	if len(sessionEnds(t, store, "2026-09-13")) != 1 {
		t.Fatalf("9-13 应保留自己的 1 个 Session，实际 %d 个", len(sessionEnds(t, store, "2026-09-13")))
	}
}

// TestRebuildClearsStaleSessionsOfSameDate 验证重算会清掉同一天的旧版本结果。
//
// 规则升级（rules-v1 → rules-v2）后，旧 Session 的起点可能落在别处；
// 重算必须让当天只剩新结果，否则总结里会同时出现新旧两套账。
func TestRebuildClearsStaleSessionsOfSameDate(t *testing.T) {
	store := newTestStore(t)
	eng := NewEngine(store, shanghai)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, shanghai)

	// 手工写入一条「旧版本规则」留下的、起点在别的日期的脏 Session。
	if err := store.ReplaceSessions(context.Background(), "2026-09-17",
		day.UTC(), day.AddDate(0, 0, 1).UTC(), []storage.Session{{
			ID: "s_legacy_stale_0001", Date: "2026-09-17", Project: "lumen",
			StartAt:  day.AddDate(0, 0, -1).Add(14 * time.Hour).UTC(),
			EndAt:    day.AddDate(0, 0, -1).Add(15 * time.Hour).UTC(),
			AppsJSON: `[{"app":"ZCode","duration_minutes":60}]`,
			GitJSON:  "[]", StatsJSON: `{"duration_minutes":60,"app_count":1,"git_event_count":0}`,
			AlgorithmVersion: "rules-v1",
			SourceStartAt:    day.UTC(), SourceEndAt: day.AddDate(0, 0, 1).UTC(),
			UpdatedAt: time.Now().UTC(),
		}}); err != nil {
		t.Fatalf("写入脏 Session 失败: %v", err)
	}

	insertWindow(t, store, day.Add(9*time.Hour), 1800, "ZCode", "lumen")
	if _, err := eng.RebuildDay(context.Background(), day); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	list := sessionEnds(t, store, "2026-09-17")
	if len(list) != 1 {
		t.Fatalf("重算后当天应只剩 1 个 Session，实际 %d 个", len(list))
	}
	if list[0].AlgorithmVersion != AlgorithmVersion {
		t.Fatalf("应使用新算法版本，实际 %s", list[0].AlgorithmVersion)
	}
}
