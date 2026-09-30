package tools

import (
	"context"
	"fmt"
	"time"

	"lumen/server/internal/storage"
	"lumen/server/internal/tooling"
)

// knownProjectsTool 列出最近出现过的项目名。
//
// 用途：用户说"那个项目"或名字记错时，模型先看看有哪些项目可对齐，
// 而不是凭空猜一个项目名去查询。
type knownProjectsTool struct {
	store Store
	// days 是默认回看天数。
	days int
}

func (t *knownProjectsTool) Spec() tooling.Spec {
	return tooling.Spec{
		Name: "get_known_projects",
		Summary: "列出最近出现过的项目名（默认最近 14 天）。用户提到的项目名不确定、或想知道" +
			"一共有哪些项目时先用它对齐，不要凭印象猜一个项目名去查询。",
		Parameters: []tooling.Param{
			{Name: "days", Type: tooling.ParamInteger,
				Description: "往回看多少天，默认 14，范围 1~90。",
				HasRange:    true, Min: 1, Max: 90},
		},
		ResultSchema: `{"projects":["lumen","linguaforge"],"days":14,"count":2,"note":""}`,
		Kind:         tooling.KindReference,
		Risk:         tooling.RiskRead,
	}
}

// KnownProjectsResult 是 get_known_projects 的返回结果。
type KnownProjectsResult struct {
	Projects []string `json:"projects"`
	Days     int      `json:"days"`
	Count    int      `json:"count"`
	Note     string   `json:"note,omitempty"`
}

func (t *knownProjectsTool) Execute(ctx context.Context, inv tooling.Invocation) (tooling.Result, error) {
	if t.store == nil {
		return tooling.Result{}, fmt.Errorf("缺少存储")
	}
	tc, err := temporalOf(inv)
	if err != nil {
		return tooling.Result{}, err
	}
	days := t.days
	if days <= 0 {
		days = 14
	}
	if v := inv.Args.Int("days"); v > 0 {
		days = v
	}

	now := tc.Now
	from := now.AddDate(0, 0, -days)
	to := now.AddDate(0, 0, 1)
	projects, err := t.store.ProjectsWithActivity(ctx, from, to)
	if err != nil {
		return tooling.Result{}, err
	}

	display := make([]string, 0, len(projects))
	for _, p := range projects {
		display = append(display, DisplayProject(p))
	}
	display = sortedUnique(display)

	out := KnownProjectsResult{Projects: display, Days: days, Count: len(display)}
	if len(display) == 0 {
		out.Note = fmt.Sprintf("最近 %d 天没有可归类的项目记录。", days)
	}

	var digest string
	if len(display) == 0 {
		digest = fmt.Sprintf("最近 %d 天没有可归类的项目记录。", days)
	} else {
		digest = fmt.Sprintf("最近 %d 天出现过的项目：%s。", days, joinLimited(display, 10))
	}
	return tooling.Result{Model: out, Digest: []string{digest}, Count: len(display)}, nil
}

// taskSummariesTool 读取专业 Agent 汇报的任务摘要。
//
// 与活动记录的关系不是替代而是互补：
//   - 活动记录回答"用了什么、多久"——只能推断；
//   - 本工具回答"Agent 报告做完了什么"——这是结论本身。
//
// 因此合成回答时必须区分两者：前者是 inferred，后者才是 supported。
// 这条区分是这类工具存在的全部意义，不能在上层被抹平。
type taskSummariesTool struct {
	store Store
	loc   *time.Location
	// maxLimit 限制单次返回条数，防止一次拉出整表。
	maxLimit int
}

func (t *taskSummariesTool) Spec() tooling.Spec {
	return tooling.Spec{
		Name: "get_task_summaries",
		Summary: "查询专业 Agent（如 ZCode）主动汇报的任务摘要：任务标题、状态（done/partial/blocked/abandoned/unknown）、" +
			"已产出结果、未完成事项。这是唯一带「结论」的数据源——" +
			"问“完成了什么/做完哪些/还有什么没做完/卡在哪”时用它。" +
			"date 与 project 至少给一个。",
		Parameters: []tooling.Param{
			{Name: "date", Type: tooling.ParamString, MaxLength: 10,
				Description: "日期，格式 YYYY-MM-DD。不知道今天是几号就先调用 get_current_time。"},
			{Name: "project", Type: tooling.ParamString, MaxLength: 64,
				Description: "项目名。不确定名字时先用 get_known_projects 对齐。"},
			{Name: "limit", Type: tooling.ParamInteger,
				Description: "返回条数上限，默认 20，最大 50。",
				HasRange:    true, Min: 1, Max: 50},
		},
		ResultSchema: `{"query":"date=2026-09-18","task_summaries":[{"task_id":"...","project":"lumen",` +
			`"app":"ZCode","title":"...","status":"done","outcomes":["..."],"open_loops":["..."],` +
			`"source_agent":"zcode-cli","occurred_at":"2026-09-18 15:42"}],"count":1,` +
			`"status_counts":{"done":1},"note":""}`,
		Kind: tooling.KindReportedTasks,
		Risk: tooling.RiskRead,
	}
}

// TaskSummaryView 是给模型看的任务摘要视图。
//
// 刻意不含 source_session_id：那是来源 Agent 的内部会话 ID，只用于追溯，
// 既不该出现在模型的上下文里，也不该出现在用户可见文本里。
type TaskSummaryView struct {
	TaskID      string   `json:"task_id"`
	Project     string   `json:"project,omitempty"`
	App         string   `json:"app,omitempty"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	Outcomes    []string `json:"outcomes"`
	OpenLoops   []string `json:"open_loops"`
	SourceAgent string   `json:"source_agent"`
	OccurredAt  string   `json:"occurred_at"`
}

// TaskSummariesResult 是 get_task_summaries 的返回结果。
type TaskSummariesResult struct {
	Query         string            `json:"query"`
	TaskSummaries []TaskSummaryView `json:"task_summaries"`
	Count         int               `json:"count"`
	// StatusCounts 按状态汇总，方便模型直接说"3 个完成、1 个受阻"。
	StatusCounts map[string]int `json:"status_counts"`
	Note         string         `json:"note,omitempty"`
}

func (t *taskSummariesTool) Execute(ctx context.Context, inv tooling.Invocation) (tooling.Result, error) {
	if t.store == nil {
		return tooling.Result{}, fmt.Errorf("缺少存储")
	}
	loc := safeLocation(t.loc)

	date := inv.Args.String("date")
	project := inv.Args.String("project")
	if date == "" && project == "" {
		return tooling.Result{}, tooling.Deny("需要提供 date 或 project 之一")
	}

	limit := t.maxLimit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if v := inv.Args.Int("limit"); v > 0 && v < limit {
		limit = v
	}

	var list []storage.TaskSummary
	var query string
	var err error
	if date != "" {
		day, perr := time.ParseInLocation("2006-01-02", date, loc)
		if perr != nil {
			return tooling.Result{}, tooling.Deny("date 必须是 YYYY-MM-DD 格式")
		}
		list, err = t.store.TaskSummariesBetween(ctx, day, day.AddDate(0, 0, 1), limit)
		query = "date=" + date
	} else {
		list, err = t.store.TaskSummariesByProject(ctx, project, limit)
		query = "project=" + project
	}
	if err != nil {
		return tooling.Result{}, err
	}

	views := make([]TaskSummaryView, 0, len(list))
	ids := make([]string, 0, len(list))
	for _, item := range list {
		views = append(views, taskSummaryView(item, loc))
		if item.TaskID != "" {
			ids = append(ids, item.TaskID)
		}
	}

	out := TaskSummariesResult{
		Query: query, TaskSummaries: views, Count: len(views),
		StatusCounts: statusCounts(list),
	}
	if len(views) == 0 {
		out.Note = "这个范围内没有收到 Agent 汇报的任务摘要（这不等于没做事，只说明没有 Agent 报过结果）。"
	}

	return tooling.Result{
		Model:    out,
		Digest:   taskDigest(out),
		Evidence: dedupeStrings(ids),
		Count:    len(views),
	}, nil
}

func taskSummaryView(t storage.TaskSummary, loc *time.Location) TaskSummaryView {
	return TaskSummaryView{
		TaskID:      t.TaskID,
		Project:     DisplayProject(t.Project),
		App:         t.App,
		Title:       truncateRunes(t.Title, 120),
		Status:      t.Status,
		Outcomes:    clipItems(t.Outcomes),
		OpenLoops:   clipItems(t.OpenLoops),
		SourceAgent: t.SourceAgent,
		OccurredAt:  t.OccurredAt.In(loc).Format("2006-01-02 15:04"),
	}
}

// taskDigest 渲染任务摘要的确定性事实行。
//
// 明确写"Agent 报告"而不是"你完成了"：结论来自 Agent 的自我汇报，
// 不是 Lumen 观察到的，用户有权知道这个区别。
func taskDigest(out TaskSummariesResult) []string {
	if len(out.TaskSummaries) == 0 {
		return []string{fmt.Sprintf("（%s）没有查到 Agent 汇报的任务摘要。", out.Query)}
	}
	lines := []string{fmt.Sprintf("（%s）Agent 报告了 %d 个任务：", out.Query, len(out.TaskSummaries))}
	for _, item := range out.TaskSummaries {
		lines = append(lines, fmt.Sprintf("▍%s（%s）", item.Title, taskStatusLabel(item.Status)))
		if item.Project != "" {
			lines = append(lines, "  · 项目："+item.Project)
		}
		for _, text := range item.Outcomes {
			lines = append(lines, "  · 完成："+text)
		}
		for _, text := range item.OpenLoops {
			lines = append(lines, "  · 未完成："+text)
		}
		if item.SourceAgent != "" {
			lines = append(lines, "  · 来源："+item.SourceAgent+" 报告")
		}
	}
	return lines
}

// taskStatusLabel 把状态转成用户可读文案。
func taskStatusLabel(status string) string {
	switch status {
	case "done":
		return "已完成"
	case "partial":
		return "部分完成"
	case "blocked":
		return "受阻"
	case "abandoned":
		return "已放弃"
	default:
		return "状态未说明"
	}
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// joinLimited 拼接列表，超过 limit 项时用"等"收尾。
func joinLimited(items []string, limit int) string {
	if len(items) <= limit {
		return joinCN(items)
	}
	return joinCN(items[:limit]) + " 等"
}

func joinCN(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += "、"
		}
		out += item
	}
	return out
}
