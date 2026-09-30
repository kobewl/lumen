package temporal

import (
	"testing"
	"time"
)

var shanghai = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// at 构造上海时区的固定时刻。
func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, shanghai)
	if err != nil {
		t.Fatalf("解析时间 %q 失败: %v", value, err)
	}
	return parsed
}

// TestBuildProducesSameAnswerEverywhere 覆盖核心约定：
// 同一 Clock/时区构建的 Context，其派生字段（日期/星期/时段）彼此一致，
// 下游谁都不需要再自己推导。
func TestBuildProducesSameAnswerEverywhere(t *testing.T) {
	clock := FixedClock(at(t, "2026-09-18 13:19:00")) // 周五
	tc := Build(clock, shanghai)

	if !tc.Valid() {
		t.Fatal("Build 出的 Context 必须可用")
	}
	if tc.Date != "2026-09-18" {
		t.Fatalf("日期应为 2026-09-18，实际 %q", tc.Date)
	}
	if tc.Weekday != "周五" {
		t.Fatalf("星期应为周五，实际 %q", tc.Weekday)
	}
	if tc.DayPart != "下午" {
		t.Fatalf("13:19 应为下午，实际 %q", tc.DayPart)
	}
	if tc.ClockText() != "2026-09-18 13:19" {
		t.Fatalf("ClockText 不符: %q", tc.ClockText())
	}
	if tc.Now.Location() != shanghai {
		t.Fatal("Now 应已转换到本地时区")
	}
}

// TestDayPartBoundaries 覆盖时段边界，含验收指定的三档：
// 13:19（下午）、18:30（晚上）、00:30（深夜）。
func TestDayPartBoundaries(t *testing.T) {
	cases := map[string]string{
		"2026-09-18 00:30:00": "深夜",
		"2026-09-18 04:59:00": "深夜",
		"2026-09-18 05:00:00": "早上",
		"2026-09-18 08:59:00": "早上",
		"2026-09-18 09:00:00": "上午",
		"2026-09-18 11:59:00": "上午",
		"2026-09-18 12:00:00": "中午",
		"2026-09-18 12:59:00": "中午",
		"2026-09-18 13:19:00": "下午", // 验收指定：口语"下午一点"从 13 点起
		"2026-09-18 13:59:00": "下午",
		"2026-09-18 14:00:00": "下午",
		"2026-09-18 17:59:00": "下午",
		"2026-09-18 18:30:00": "晚上", // 验收指定
		"2026-09-18 22:59:00": "晚上",
		"2026-09-18 23:00:00": "深夜",
	}
	for value, want := range cases {
		if got := DayPart(at(t, value)); got != want {
			t.Fatalf("%s 应属于 %q，实际 %q", value, want, got)
		}
	}
}

// TestFixedClockIsStable 覆盖固定时钟的稳定性：多次读取同一时刻。
func TestFixedClockIsStable(t *testing.T) {
	fixed := at(t, "2026-09-19 00:30:00")
	clock := FixedClock(fixed)
	for i := 0; i < 3; i++ {
		if got := clock.Now(); !got.Equal(fixed) {
			t.Fatalf("FixedClock 第 %d 次读取应返回同一时刻，实际 %v", i+1, got)
		}
	}
	// 00:30 是验收指定档：深夜。
	if got := Build(clock, shanghai); got.DayPart != "深夜" {
		t.Fatalf("00:30 应为深夜，实际 %q", got.DayPart)
	}
}

// TestNilClockAndLoc 覆盖装配缺失时的回退（不 panic，但标记可用）。
func TestNilClockAndLoc(t *testing.T) {
	tc := Build(nil, nil)
	if !tc.Valid() {
		t.Fatal("nil 回退到系统时钟与 UTC 后仍应可用")
	}
	if tc.Loc != time.UTC {
		t.Fatal("nil 时区应回退 UTC")
	}

	var zero Context
	if zero.Valid() {
		t.Fatal("零值 Context 不可用（执行器据此 fail-closed）")
	}
	if zero.ClockText() == "" {
		// 零值也要能安全渲染，不能 panic。
		_ = zero.ClockText()
	}
}
