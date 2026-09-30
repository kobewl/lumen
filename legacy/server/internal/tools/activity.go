package tools

import (
	"context"
	"fmt"
	"time"

	"lumen/server/internal/storage"
	"lumen/server/internal/tooling"
)

// todayStatusTool 返回"今天到目前为止"的时段概览。
//
// "今天"来自调用携带的可信时间快照（与 Planner/Synthesizer 同一份），
// 不是工具自己取的 time.Now——这样回答"今天怎么样"与提示词里的可信时间
// 永远是同一天、同一个时段。
type todayStatusTool struct {
	store Store
}

func (t *todayStatusTool) Spec() tooling.Spec {
	return tooling.Spec{
		Name: "get_today_status",
		Summary: "读取今天的活动摘要：日期、当天已聚合的工作时段（项目、起止时间、应用与各应用时长、git 提交信息）与合计分钟数。" +
			"问“今天怎么样/今天做了什么/今天用了哪些应用”时用它。无参数。",
		ResultSchema: `{"date":"2026-09-18","sessions":[{"date":"...","project":"lumen","start":"2026-09-18 09:10",` +
			`"end":"10:40","duration_minutes":90,"apps":["ZCode 90 分钟"],"git_messages":["feat: ..."]}],` +
			`"total_minutes":90,"count":1,"note":""}`,
		Kind: tooling.KindActivity,
		Risk: tooling.RiskRead,
	}
}

// ActivityResult 是活动类工具的返回结果（今天与指定日期/项目共用同一形状）。
type ActivityResult struct {
	Date         string        `json:"date,omitempty"`
	Query        string        `json:"query,omitempty"`
	Sessions     []SessionView `json:"sessions"`
	TotalMinutes float64       `json:"total_minutes"`
	Count        int           `json:"count"`
	// Note 说明"为什么没有记录"，避免模型把空结果当成"这天没工作"。
	Note string `json:"note,omitempty"`
}

func (t *todayStatusTool) Execute(ctx context.Context, inv tooling.Invocation) (tooling.Result, error) {
	if t.store == nil {
		return tooling.Result{}, fmt.Errorf("缺少存储")
	}
	tc, err := temporalOf(inv)
	if err != nil {
		return tooling.Result{}, err
	}

	sessions, err := t.store.SessionsByDate(ctx, tc.Date)
	if err != nil {
		return tooling.Result{}, err
	}
	return activityResult("今天 "+tc.Date, tc.Date, "", sessions, tc.Loc)
}

// sessionsTool 按日期或项目检索历史时段。
//
// 这是"用户做过什么"的主要入口：只读、有上限、返回聚合结果，
// 不含原始事件、窗口标题或路径。
type sessionsTool struct {
	store Store
	// maxLimit 限制单次返回的时段数，防止一次拉出整库。
	maxLimit int
}

func (t *sessionsTool) Spec() tooling.Spec {
	return tooling.Spec{
		Name: "get_sessions",
		Summary: "按日期或项目查询已聚合的工作时段（项目、起止时间、应用与各应用时长、git 提交信息）。" +
			"用户问“昨天/上周做了什么”“某个项目最近怎么样”时用它。" +
			"date 与 project 至少给一个；只给 project 时返回该项目最近的记录。" +
			"具体日期从可信上下文的当前日期推算（例如“昨天”），不要凭印象猜。",
		Parameters: []tooling.Param{
			{Name: "date", Type: tooling.ParamString, MaxLength: 10,
				Description: "日期，格式 YYYY-MM-DD。从可信上下文的当前日期推算，不要凭空猜。"},
			{Name: "project", Type: tooling.ParamString, MaxLength: 64,
				Description: "项目名。不确定名字时先用 get_known_projects 对齐，不要猜。"},
			{Name: "limit", Type: tooling.ParamInteger, MaxLength: 0,
				Description: "返回条数上限，默认 10，最大 30。", HasRange: true, Min: 1, Max: 30},
		},
		ResultSchema: `{"query":"date=2026-09-17","sessions":[{"date":"2026-09-17","project":"lumen",` +
			`"start":"2026-09-17 09:00","end":"10:30","duration_minutes":90,` +
			`"apps":["ZCode 90 分钟"],"git_messages":["feat: ..."]}],` +
			`"total_minutes":90,"count":1,"note":""}`,
		Kind: tooling.KindActivity,
		Risk: tooling.RiskRead,
	}
}

func (t *sessionsTool) Execute(ctx context.Context, inv tooling.Invocation) (tooling.Result, error) {
	if t.store == nil {
		return tooling.Result{}, fmt.Errorf("缺少存储")
	}
	tc, err := temporalOf(inv)
	if err != nil {
		return tooling.Result{}, err
	}
	loc := safeLocation(tc.Loc)

	date := inv.Args.String("date")
	project := inv.Args.String("project")
	if date == "" && project == "" {
		// 不给范围就等于拉全库，这是工具内约束（策略之外的第二道）。
		return tooling.Result{}, tooling.Deny("需要提供 date 或 project 之一")
	}

	limit := t.maxLimit
	if limit <= 0 || limit > maxSessionsPerView {
		limit = 10
	}
	if v := inv.Args.Int("limit"); v > 0 && v < limit {
		limit = v
	}

	var sessions []storage.Session
	var query string
	var err2 error
	if date != "" {
		if _, perr := time.ParseInLocation("2006-01-02", date, loc); perr != nil {
			return tooling.Result{}, tooling.Deny("date 必须是 YYYY-MM-DD 格式")
		}
		sessions, err2 = t.store.SessionsByDate(ctx, date)
		query = "date=" + date
		if err2 == nil && len(sessions) > limit {
			sessions = sessions[:limit]
		}
	} else {
		sessions, err2 = t.store.SessionsByProject(ctx, project, limit)
		query = "project=" + project
	}
	if err2 != nil {
		return tooling.Result{}, err2
	}
	return activityResult(query, "", query, sessions, loc)
}

// activityResult 把时段记录转成统一的活动结果（模型视图 + 事实行 + 证据）。
func activityResult(label, date, query string, sessions []storage.Session, loc *time.Location) (tooling.Result, error) {
	views := make([]SessionView, 0, len(sessions))
	total := 0.0
	for _, s := range sessions {
		v := sessionView(s, loc)
		total += v.DurationMinutes
		views = append(views, v)
	}
	ids, span := sessionEvidence(sessions)

	out := ActivityResult{
		Date:         date,
		Query:        query,
		Sessions:     views,
		TotalMinutes: round1(total),
		Count:        len(views),
	}
	if len(views) == 0 {
		out.Note = "这个范围内没有任何已聚合的工作时段。"
	}

	return tooling.Result{
		Model:    out,
		Digest:   sessionDigest("（"+label+"）", sessions, loc),
		Evidence: ids,
		Count:    len(views),
		Span:     span,
	}, nil
}
