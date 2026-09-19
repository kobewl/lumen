// Package temporal 是"可信时间"这一领域概念的唯一落点。
//
// 为什么需要它：此前当前时间分散在三处——agent.go 直接 time.Now、
// get_current_time 是模型的可选调用（不调就没有）、Synthesizer 的提示词里
// 根本没有时间。结果就是"中午说早呀"：模型靠运气猜时段。
//
// 这里的约定是：一轮对话只有一个 Context 对象，由注入的 Clock 在装配根
// 生成一次，然后传给 Planner、Synthesizer、事实兜底和所有需要当前时间的
// 工具。任何一层都不允许自行调用 time.Now——执行器对"缺少可信时间"
// fail-closed，这让约定变成结构保证。
package temporal

import "time"

// Clock 提供当前时刻。可注入：生产用 SystemClock，测试与本地验收用 FixedClock。
type Clock interface {
	Now() time.Time
}

// SystemClock 用真实系统时钟。
type SystemClock struct{}

// Now 实现 Clock。
func (SystemClock) Now() time.Time { return time.Now() }

// FixedClock 恒定返回同一时刻（测试与本地验收用）。
type FixedClock time.Time

// Now 实现 Clock。
func (f FixedClock) Now() time.Time { return time.Time(f) }

// Context 是一轮对话的可信时间快照。
//
// 字段在 Build 时一次性算好（含中文星期与时段），下游只读不再推导，
// 因此"13:19 到底算什么时段"只有一个答案，TemporalGuard 与提示词用的是同一个。
type Context struct {
	// Now 是本地时区下的当前时刻。
	Now time.Time
	// Loc 是展示与查询用的时区。
	Loc *time.Location
	// Date 是本地日期 YYYY-MM-DD。
	Date string
	// Weekday 是中文星期（周一…周日）。
	Weekday string
	// DayPart 是中文时段（早上/上午/中午/下午/晚上/深夜）。
	DayPart string
}

// Build 用 Clock 与时区生成一个可信时间快照。
//
// loc 为 nil 时回退 UTC：调用方装配缺失不应该 panic，
// 但 Context 会带上 UTC 时区名，提示词里看得出来。
func Build(c Clock, loc *time.Location) Context {
	if loc == nil {
		loc = time.UTC
	}
	var now time.Time
	if c == nil {
		now = time.Now().In(loc)
	} else {
		now = c.Now().In(loc)
	}
	return Context{
		Now:     now,
		Loc:     loc,
		Date:    now.Format("2006-01-02"),
		Weekday: WeekdayCN(now),
		DayPart: DayPart(now),
	}
}

// Valid 判断 Context 是否可用。零值不可用：执行器据此 fail-closed。
func (c Context) Valid() bool {
	return c.Loc != nil && !c.Now.IsZero() && c.Date != ""
}

// ClockText 渲染成给模型与日志看的"日期 时刻"。
func (c Context) ClockText() string {
	return c.Now.Format("2006-01-02 15:04")
}

// WeekdayCN 返回中文星期。
func WeekdayCN(t time.Time) string {
	switch t.Weekday() {
	case time.Monday:
		return "周一"
	case time.Tuesday:
		return "周二"
	case time.Wednesday:
		return "周三"
	case time.Thursday:
		return "周四"
	case time.Friday:
		return "周五"
	case time.Saturday:
		return "周六"
	default:
		return "周日"
	}
}

// DayPart 把一天切成中文时段。边界是产品口径，写在一处：
// TemporalGuard、提示词、get_current_time 工具全部用它，不会有第二份答案。
//
//	5:00-8:59 早上 / 9:00-11:59 上午 / 12:00-12:59 中午
//	13:00-17:59 下午（口语"下午一点"从 13 点起）/ 18:00-22:59 晚上
//	其余（23:00-4:59）深夜
//
// 因此 13:19 是下午——"早上好"在 13:19 是明显冲突（TemporalGuard 的验收样例）。
func DayPart(t time.Time) string {
	switch h := t.Hour(); {
	case h >= 5 && h < 9:
		return "早上"
	case h >= 9 && h < 12:
		return "上午"
	case h >= 12 && h < 13:
		return "中午"
	case h >= 13 && h < 18:
		return "下午"
	case h >= 18 && h < 23:
		return "晚上"
	default:
		return "深夜"
	}
}
