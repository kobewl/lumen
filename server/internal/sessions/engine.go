// Package sessions 实现确定性规则 Session 聚合。
//
// 规则（与产品文档一致，AI 不参与切分）：
//  1. 同一项目且相邻活动间隔小于 8 分钟时合并为同一 Session；
//  2. 离开状态（锁屏，或超过 8 分钟的 idle）切断当前 Session，切断点是**离开时刻**；
//  3. 离开期间产生的活动一律裁剪掉，绝不计入工作时长；
//  4. 项目无法可靠识别时标为 unclassified，不猜测；
//  5. 迟到事件只重算所属日期的相邻窗口。
//
// 事件分类来源：
//   - window.activity：应用与时长，是 Session 的主体；
//   - idle.state：判断会话边界；
//   - git.activity：提供项目归属与提交证据。
//
// idle.state 的区间语义见 events.IdleSchemaVersionV2。历史数据里既有旧语义
// （timestamp 是区间结束），也有系统伪应用（loginwindow）产生的假活动，
// 这里统一在重算阶段消化，不要求删除原始事件。
package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"lumen/server/internal/events"
	"lumen/server/internal/storage"
)

// GapThreshold 是会话合并的最大间隔：8 分钟。
const GapThreshold = 8 * time.Minute

// MinSessionDuration 是 Session 的最小长度，过滤纯噪声片段。
const MinSessionDuration = 1 * time.Minute

// GitAttributionWindow 是用 git 事件为窗口活动补项目归属时允许的时间偏差。
//
// 这个窗口刻意设得较小：归因本质上是一种推测，窗口太宽会把无关活动
// （比如提交前后刚好在浏览器上看网页）错误地算进项目里。
// 产品原则是"无法可靠识别就标 unclassified"，因此宁可少归因也不乱归因。
const GitAttributionWindow = 15 * time.Minute

// Unclassified 是无法识别项目时的归属标记。
//
// 它只是内部标记，不能直接展示给用户：用户可见文案由 feishu.ProjectDisplayName
// 转成「暂未识别项目」。
const Unclassified = "unclassified"

// AlgorithmVersion 是当前聚合算法版本，写入 sessions 表用于追溯。
//
// rules-v1 → rules-v2 的变更：idle.state 改用区间语义（timestamp = 区间开始），
// 切断点落在离开时刻，并忽略系统伪应用与离开期间的活动。版本号变化会让
// Session ID 重新派生，从而在重算时自动清掉旧的脏 Session。
const AlgorithmVersion = "rules-v2"

// awayLookback 是重算某天时向前多看的时间。
//
// 跨夜锁屏的区间起点在前一天晚上（真机数据：22:42 锁屏、次日 09:14 解锁），
// 只看当天事件会漏掉这个区间，导致第二天清晨的假活动无法被裁剪。
const awayLookback = 24 * time.Hour

// Engine 依据事件重算某日 Session。
type Engine struct {
	store *storage.Store
	loc   *time.Location
}

// NewEngine 创建 Session 引擎。
func NewEngine(store *storage.Store, loc *time.Location) *Engine {
	if loc == nil {
		loc = time.UTC
	}
	return &Engine{store: store, loc: loc}
}

// activity 是聚合过程中使用的一段活动。
type activity struct {
	start   time.Time
	end     time.Time
	app     string
	project string
}

// awayInterval 表示一段「用户不在」的区间。
//
// end 为零值表示区间尚未闭合。未闭合的区间不能用「一直延续到永久」处理：
// 用户可能只是进程重启，随后就回来工作了；此时区间在 mergeAway 里按
// 下一条真实活动的开始时刻收口。
type awayInterval struct {
	start  time.Time
	end    time.Time
	locked bool
}

// closed 返回区间是否已经闭合。
func (a awayInterval) closed() bool { return !a.end.IsZero() }

// effectiveEnd 返回区间实际生效的结束时刻；未闭合时返回零值表示「延续」。
func (a awayInterval) effectiveEnd() time.Time { return a.end }

// gitEvidence 是一条挂到 Session 上的 Git 证据。
type gitEvidence struct {
	At time.Time `json:"at"`
	// Kind 区分 commit 与 workspace。
	//
	// commit 是用户真实做出的动作，可用于推断"当时在哪个项目工作"；
	// workspace 只是扫描时发现的仓库基线状态（首次启动会对每个仓库各产生一条），
	// 不代表用户此刻在该仓库工作，因此**不参与**项目归因。
	Kind    string `json:"kind,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Commit  string `json:"head_commit,omitempty"`
	Message string `json:"commit_message,omitempty"`
	Files   int    `json:"changed_files_count,omitempty"`
}

// builtSession 是合并结果，随后转换成 storage.Session。
type builtSession struct {
	project string
	start   time.Time
	end     time.Time
	apps    map[string]float64
	git     []gitEvidence
}

// RebuildDay 重新构建指定本地日期的全部 Session。
// 返回该日期，便于调用方了解重算范围。重复执行结果一致。
func (e *Engine) RebuildDay(ctx context.Context, day time.Time) (string, error) {
	date := day.In(e.loc).Format("2006-01-02")
	from, to := e.dayRange(day)

	// 向前多看一段：跨夜离开区间的起点可能在昨天，只查当天会漏掉，
	// 导致清晨的假活动无法被裁剪。
	rawEvents, err := e.store.EventsBetween(ctx, from.Add(-awayLookback), to)
	if err != nil {
		return date, err
	}

	acts, aways, gits := e.extract(rawEvents)
	built := e.merge(acts, aways, gits)

	sessions := make([]storage.Session, 0, len(built))
	now := time.Now().UTC()
	for _, b := range built {
		// 只看回一天是为了拿到跨夜离开区间，但合并结果里也会出现属于
		// 前一天的活动。Session 归属按「开始时刻所在的本地日期」决定，
		// 否则重算某天会把昨天的 Session 一起写进今天。
		if b.start.In(e.loc).Format("2006-01-02") != date {
			continue
		}
		sessions = append(sessions, storage.Session{
			// ID 由内容确定性派生，保证同一天数据不变时重复重算得到相同 ID。
			// 这样总结记录的 evidence_session_ids 不会因为重算而失效，
			// input_hash 也才具备幂等意义。
			ID:               stableID(date, b.project, b.start, b.end),
			Date:             date,
			Project:          b.project,
			StartAt:          b.start,
			EndAt:            b.end,
			AppsJSON:         b.appsJSON(),
			GitJSON:          b.gitJSON(),
			StatsJSON:        b.statsJSON(),
			AlgorithmVersion: AlgorithmVersion,
			SourceStartAt:    from,
			SourceEndAt:      to,
			UpdatedAt:        now,
		})
	}

	// 即使结果为空也要执行替换，保证重算的幂等性和旧数据的清理。
	if err := e.store.ReplaceSessions(ctx, date, from, to, sessions); err != nil {
		return date, err
	}
	return date, nil
}

// RebuildRange 重算一段日期范围，用于迟到事件补偿。
func (e *Engine) RebuildRange(ctx context.Context, from, to time.Time) ([]string, error) {
	var days []string
	cur := from.In(e.loc)
	last := to.In(e.loc)
	for !cur.After(last) {
		date, err := e.RebuildDay(ctx, cur)
		if err != nil {
			return days, err
		}
		days = append(days, date)
		cur = cur.AddDate(0, 0, 1)
	}
	return days, nil
}

// DayRange 返回本地日期对应的 UTC 时间区间 [start, end)。
func (e *Engine) DayRange(day time.Time) (time.Time, time.Time) {
	return e.dayRange(day)
}

// TodayRange 返回“今天”在本地时区的 UTC 区间。
func (e *Engine) TodayRange(now time.Time) (time.Time, time.Time) {
	return e.dayRange(now)
}

func (e *Engine) dayRange(day time.Time) (time.Time, time.Time) {
	local := day.In(e.loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, e.loc)
	return start.UTC(), start.AddDate(0, 0, 1).UTC()
}

// extract 把原始事件拆成活动片段、离开区间和 Git 证据。
func (e *Engine) extract(rawEvents []storage.Event) ([]activity, []awayInterval, []gitEvidence) {
	var acts []activity
	var aways []awayInterval
	var gits []gitEvidence

	for _, ev := range rawEvents {
		var ctx map[string]any
		var data map[string]any
		_ = json.Unmarshal([]byte(ev.ContextJSON), &ctx)
		_ = json.Unmarshal([]byte(ev.DataJSON), &data)

		switch ev.Type {
		case "idle.state":
			if iv, ok := awayIntervalOf(ev); ok {
				aways = append(aways, iv)
			}

		case "git.activity":
			project, _ := ctx["project"].(string)
			repo, _ := ctx["repo"].(string)
			if project == "" {
				project = repo
			}
			branch, _ := data["branch"].(string)
			commit, _ := data["head_commit"].(string)
			message, _ := data["commit_message"].(string)
			kind, _ := data["kind"].(string)
			gits = append(gits, gitEvidence{
				At: ev.Timestamp, Kind: kind, Repo: repo, Branch: branch,
				Commit: commit, Message: message, Files: int(toFloat(data["changed_files_count"])),
			})

		case "window.activity":
			app, _ := ctx["app"].(string)
			project, _ := ctx["project"].(string)
			bundleID, _ := ctx["bundle_id"].(string)
			dur := toFloat(data["duration_seconds"])
			if dur <= 0 || app == "" {
				continue
			}
			// 第一层防御在采集端（不再产生伪应用事件）；这里是第二层，
			// 用于让已经入库的历史假活动在重算时被忽略，而不是删掉原始事件。
			if events.IsSystemPseudoApp(app, bundleID) {
				continue
			}
			acts = append(acts, activity{
				start:   ev.Timestamp,
				end:     ev.Timestamp.Add(time.Duration(dur) * time.Second),
				app:     app,
				project: project,
			})
		}
	}

	// 用 Git 事件补项目归属：缺少 project 的窗口活动，若附近有 Git **提交**，才归属到该仓库。
	//
	// 只认 commit 事件的三个原因：
	//  1. workspace 只是扫描时的基线快照，首次启动会对所有仓库同时各产生一条，
	//     用它归因会把所有活动都算到时间上最近的那个仓库上（实际踩过这个坑）；
	//  2. 提交是用户真实做出的动作，时间上贴近正在做的事；
	//  3. 一个时间段内可能有多个仓库提交，此时按时间最近的那个归属，且只取一个，
	//     避免把同一段活动算进多个项目。
	for i := range acts {
		if acts[i].project != "" {
			continue
		}

		bestRepo := ""
		bestDelta := GitAttributionWindow
		for _, g := range gits {
			if g.Kind != "commit" || g.Repo == "" {
				continue
			}
			delta := acts[i].start.Sub(g.At)
			if delta < 0 {
				delta = -delta
			}
			if delta < bestDelta {
				bestDelta = delta
				bestRepo = g.Repo
			}
		}
		if bestRepo != "" {
			acts[i].project = bestRepo
		}
	}

	sort.SliceStable(acts, func(i, j int) bool { return acts[i].start.Before(acts[j].start) })
	sort.Slice(aways, func(i, j int) bool { return aways[i].start.Before(aways[j].start) })
	sort.Slice(gits, func(i, j int) bool { return gits[i].At.Before(gits[j].At) })

	aways = mergeAwayIntervals(aways, acts)
	acts = clipActivities(acts, aways)
	return acts, aways, gits
}

// awayIntervalOf 把一条 idle.state 事件解析成「用户不在」的区间。
//
// 返回 false 表示这条事件不代表用户离开（例如 active 状态）。
func awayIntervalOf(ev storage.Event) (awayInterval, bool) {
	var data map[string]any
	if err := json.Unmarshal([]byte(ev.DataJSON), &data); err != nil {
		return awayInterval{}, false
	}
	state, _ := data["state"].(string)
	if state != "idle" && state != "locked" {
		return awayInterval{}, false
	}
	locked := state == "locked"

	dur, hasDur := toFloatOK(data["duration_seconds"])
	version, _ := toFloatOK(data["schema_version"])

	// 新语义：timestamp 是区间开始，duration 是区间长度（缺省表示未闭合）。
	if version >= events.IdleSchemaVersionV2 {
		iv := awayInterval{start: ev.Timestamp, locked: locked}
		if hasDur && dur > 0 {
			iv.end = ev.Timestamp.Add(time.Duration(dur) * time.Second)
		}
		return iv, true
	}

	// 旧语义：timestamp 是区间结束，区间为 [timestamp - duration, timestamp]。
	//
	// 真机数据：{"state":"locked","duration_seconds":45094}，timestamp 是解锁时刻
	// 2026-09-18T01:14:04Z，区间实际是前一天的 20:42:30 ~ 次日 09:14:04。
	// 按新语义误读会把区间搬到解锁之后，抹掉当天上午的工作。
	if !hasDur || dur <= 0 {
		return awayInterval{}, false
	}
	return awayInterval{
		start:  ev.Timestamp.Add(-time.Duration(dur) * time.Second),
		end:    ev.Timestamp,
		locked: locked,
	}, true
}

// mergeAwayIntervals 合并重叠的离开区间，并给未闭合的区间收口。
//
// 收口规则：未闭合区间延续到「下一条真实活动的开始时刻」。它的含义是
// 「用户不在」这段状态没有观测到结束，但随后的活动证明人已经回来了，
// 所以区间实际结束在活动开始之处。
func mergeAwayIntervals(in []awayInterval, acts []activity) []awayInterval {
	if len(in) == 0 {
		return nil
	}
	sorted := make([]awayInterval, len(in))
	copy(sorted, in)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].start.Before(sorted[j].start) })

	out := make([]awayInterval, 0, len(sorted))
	for _, iv := range sorted {
		if iv.closed() && !iv.end.After(iv.start) {
			// 零长度区间没有信息量，忽略。
			continue
		}
		if iv.end.IsZero() {
			for _, a := range acts {
				if a.start.After(iv.start) {
					iv.end = a.start
					break
				}
			}
		}
		if n := len(out); n > 0 {
			last := &out[n-1]
			// 后一个区间已经在上一个（未闭合）区间之内：忽略。
			if last.end.IsZero() {
				continue
			}
			if !iv.start.After(last.end) {
				if iv.end.After(last.end) {
					last.end = iv.end
				}
				last.locked = last.locked || iv.locked
				continue
			}
		}
		out = append(out, iv)
	}
	return out
}

// effectiveAway 返回真正构成「离开」的区间。
//
// 判定标准：锁屏一律成立；idle 需要足够长（>= GapThreshold），
// 短暂 idle（比如看视频时没有输入）不构成离开，不能裁剪工作记录。
// 未闭合的 idle 无法判定时长，忽略。
func effectiveAway(aways []awayInterval) []awayInterval {
	out := make([]awayInterval, 0, len(aways))
	for _, iv := range aways {
		if iv.locked {
			out = append(out, iv)
			continue
		}
		if !iv.closed() {
			continue
		}
		if iv.end.Sub(iv.start) >= GapThreshold {
			out = append(out, iv)
		}
	}
	return out
}

// clipActivities 裁掉落在离开区间内的活动片段。
//
// 这是「数据真实性」的核心：离开期间不可能有工作，无论事件是真是假。
func clipActivities(acts []activity, aways []awayInterval) []activity {
	effective := effectiveAway(aways)
	if len(effective) == 0 {
		return acts
	}
	out := make([]activity, 0, len(acts))
	for _, a := range acts {
		parts := []activity{a}
		for _, iv := range effective {
			next := make([]activity, 0, len(parts))
			for _, part := range parts {
				next = append(next, subtractActivity(part, iv)...)
			}
			parts = next
			if len(parts) == 0 {
				break
			}
		}
		for _, part := range parts {
			if part.end.Sub(part.start) >= time.Second {
				out = append(out, part)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return out
}

// subtractActivity 从一段活动里减掉一个离开区间，返回剩余片段。
func subtractActivity(a activity, iv awayInterval) []activity {
	end := iv.effectiveEnd()
	// 无重叠（闭合区间且活动在其之后）。
	if !end.IsZero() && !a.start.Before(end) {
		return []activity{a}
	}
	// 活动在区间开始之前结束。
	if !a.end.After(iv.start) {
		return []activity{a}
	}
	// 区间从活动头部开始。
	if !iv.start.After(a.start) {
		if end.IsZero() || !end.Before(a.end) {
			return nil
		}
		return []activity{{start: end, end: a.end, app: a.app, project: a.project}}
	}
	left := activity{start: a.start, end: iv.start, app: a.app, project: a.project}
	if end.IsZero() || !end.Before(a.end) {
		return []activity{left}
	}
	return []activity{left, {start: end, end: a.end, app: a.app, project: a.project}}
}

// merge 按规则把活动片段合并成 Session，并把 Git 证据挂到覆盖它的 Session 上。
func (e *Engine) merge(acts []activity, aways []awayInterval, gits []gitEvidence) []builtSession {
	var out []builtSession
	var cur *builtSession

	cuts := cutPoints(aways)

	flush := func() {
		if cur == nil {
			return
		}
		if cur.end.Sub(cur.start) >= MinSessionDuration {
			out = append(out, *cur)
		}
		cur = nil
	}

	newSession := func(a activity, project string) *builtSession {
		s := &builtSession{project: project, start: a.start, end: a.end, apps: map[string]float64{}}
		s.apps[a.app] += a.end.Sub(a.start).Seconds()
		return s
	}

	for _, a := range acts {
		project := a.project
		if project == "" {
			project = Unclassified
		}

		if cur == nil {
			cur = newSession(a, project)
			continue
		}

		// 只有同项目、间隔小于阈值且中间没有离开切断点时才合并。
		if cur.project == project && a.start.Sub(cur.end) < GapThreshold &&
			!cutBetween(cuts, cur.end, a.start) {
			if a.end.After(cur.end) {
				cur.end = a.end
			}
			cur.apps[a.app] += a.end.Sub(a.start).Seconds()
			continue
		}

		flush()
		cur = newSession(a, project)
	}
	flush()

	// 把 Git 证据挂到时间范围覆盖它的 Session。
	for i := range out {
		for _, g := range gits {
			if !g.At.Before(out[i].start) && !g.At.After(out[i].end) {
				out[i].git = append(out[i].git, g)
			}
		}
	}
	return out
}

// cutPoints 返回需要在 Session 里切断的时刻：离开区间的起点。
//
// 方向很关键：切断点是用户最后活动的时刻（区间开始），不是发现离开、
// 更不是回来的时刻。曾经用 timestamp + duration 取到区间结尾，
// 结果是「锁屏 12 小时」的那段被算成了工作。
func cutPoints(aways []awayInterval) []time.Time {
	effective := effectiveAway(aways)
	cuts := make([]time.Time, 0, len(effective))
	for _, iv := range effective {
		cuts = append(cuts, iv.start)
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].Before(cuts[j]) })
	return cuts
}

// stableID 由日期、项目与时间边界派生确定性的 Session ID。
//
// 为什么不直接用随机 ULID：每次重算都换 ID 会让总结里的证据 ID 失效，
// 也会让“输入未变化时不重复生成”的幂等判断永远失效。用内容派生即可保证
// 同一段工作得到同一个 ID，而一旦边界变化（补传迟到事件）自然会得到新 ID。
func stableID(date, project string, start, end time.Time) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		date, project, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), AlgorithmVersion,
	}, "|")))
	return "s_" + hex.EncodeToString(sum[:])[:24]
}

// cutBetween 判断 [from, to) 之间是否存在离开切断点。
func cutBetween(cuts []time.Time, from, to time.Time) bool {
	for _, c := range cuts {
		// 切断点正好落在边界上时按「切断」处理：宁可分开，也不要把
		// 两段被离开隔开的工作错误地合并成一段。
		if !c.Before(from) && c.Before(to) {
			return true
		}
	}
	return false
}

func (b builtSession) appsJSON() string {
	type appEntry struct {
		App             string  `json:"app"`
		DurationMinutes float64 `json:"duration_minutes"`
	}
	entries := make([]appEntry, 0, len(b.apps))
	for name, secs := range b.apps {
		entries = append(entries, appEntry{App: name, DurationMinutes: round1(secs / 60)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].DurationMinutes > entries[j].DurationMinutes })
	return marshalJSON(entries)
}

func (b builtSession) gitJSON() string {
	if len(b.git) == 0 {
		return "[]"
	}
	return marshalJSON(b.git)
}

func (b builtSession) statsJSON() string {
	var total float64
	for _, secs := range b.apps {
		total += secs
	}
	return marshalJSON(map[string]any{
		"duration_minutes": round1(total / 60),
		"app_count":        len(b.apps),
		"git_event_count":  len(b.git),
	})
}

func toFloat(v any) float64 {
	f, _ := toFloatOK(v)
	return f
}

func toFloatOK(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
