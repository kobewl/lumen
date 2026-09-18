package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"lumen/server/internal/storage"
)

// Capability 是一次受限的、只读的数据访问。
//
// 设计原则：模型永远拿不到 SQL、Shell 或任意查询能力，只能用这里声明的
// 固定能力。每个能力自己声明参数 schema 与结果形状，Policy Gate 据此校验，
// 模型构造的额外参数一律拒绝。
type Capability interface {
	// Name 是能力名，模型在 plan.tool_calls[].name 里引用它。
	Name() string
	// Description 给模型看的一句话说明（含参数用法）。
	Description() string
	// Parameters 声明允许的参数名与类型，用于拒绝越界参数。
	Parameters() map[string]ParamSpec
	// ReadOnly 标记能力是否只读。V0.1 只允许只读能力。
	ReadOnly() bool
	// Run 执行能力；args 已通过参数校验。
	Run(ctx context.Context, args map[string]any) (any, error)
}

// ParamSpec 描述一个参数的类型与取值范围。
type ParamSpec struct {
	Type     string // string | int
	Required bool
	// Enum 非空时，取值必须落在其中。
	Enum []string
	// MaxLength 限制字符串长度，防止把超长内容塞进数据库查询。
	MaxLength int
}

// CapabilityRegistry 持有允许被调用的全部能力。
type CapabilityRegistry struct {
	order  []string
	byName map[string]Capability
}

// NewRegistry 用给定能力构建注册表。
func NewRegistry(caps ...Capability) *CapabilityRegistry {
	r := &CapabilityRegistry{byName: make(map[string]Capability, len(caps))}
	for _, c := range caps {
		if c == nil {
			continue
		}
		name := c.Name()
		if _, dup := r.byName[name]; dup {
			// 重名会让"模型调用了哪个能力"变得不确定，直接忽略后者并让它
			// 在构造期就暴露出来（测试会覆盖）。
			continue
		}
		r.byName[name] = c
		r.order = append(r.order, name)
	}
	return r
}

// Get 按名字取能力。
//
// 空注册表返回"未命中"而不是 panic：注册表缺失是装配问题，
// 调用方的正确反应是拒绝这次调用，不是把服务弄崩。
func (r *CapabilityRegistry) Get(name string) (Capability, bool) {
	if r == nil {
		return nil, false
	}
	c, ok := r.byName[name]
	return c, ok
}

// Names 返回全部能力名（稳定顺序，便于测试与提示词生成）。
func (r *CapabilityRegistry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Catalog 渲染给模型看的能力目录；注册表为空时返回空串。
func (r *CapabilityRegistry) Catalog() string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, name := range r.order {
		c := r.byName[name]
		fmt.Fprintf(&b, "- %s：%s\n", name, c.Description())
	}
	return b.String()
}

// ---- 具体能力 ----

// ProfileCapability 返回助手身份配置。
//
// 存在的意义：身份相关信息必须来自配置，模型据此组织回答，
// 而不是代码里写死「你叫 Lumen」。
type ProfileCapability struct {
	Profile Profile
}

func (c *ProfileCapability) Name() string { return "get_assistant_profile" }
func (c *ProfileCapability) Description() string {
	return "读取助手自己的身份配置（名字、定位、称呼、语言、语气、主动性）。回答“你是谁/你叫什么/你能做什么”这类问题时先调用它。无参数。"
}
func (c *ProfileCapability) Parameters() map[string]ParamSpec { return nil }
func (c *ProfileCapability) ReadOnly() bool                   { return true }

func (c *ProfileCapability) Run(_ context.Context, _ map[string]any) (any, error) {
	return c.Profile.Normalize().AsResult(), nil
}

// SessionsCapability 按日期或项目检索历史时段。
//
// 这是模型唯一能读到"用户做过什么"的入口，只读、有上限、返回聚合结果，
// 不含原始事件、窗口标题或路径。
type SessionsCapability struct {
	Store *storage.Store
	Loc   *time.Location
	// MaxLimit 限制单次返回的时段数，防止一次拉出整库。
	MaxLimit int
}

func (c *SessionsCapability) Name() string { return "get_sessions" }
func (c *SessionsCapability) Description() string {
	return "查询指定日期或项目的已聚合工作时段（项目、起止时间、应用与时长、git 提交信息）。" +
		"参数：date=YYYY-MM-DD（可选）、project=项目名（可选）、limit=返回条数上限（可选，默认 10）。" +
		"date 与 project 至少给一个。"
}
func (c *SessionsCapability) Parameters() map[string]ParamSpec {
	return map[string]ParamSpec{
		"date":    {Type: "string", MaxLength: 10},
		"project": {Type: "string", MaxLength: 64},
		"limit":   {Type: "int"},
	}
}
func (c *SessionsCapability) ReadOnly() bool { return true }

// SessionView 是给模型看的时段视图：只有项目、时间、应用与时长、提交信息。
//
// ID 字段带 json:"-"，因此序列化给模型时不会出现；代码内部仍能用它做审计，
// 保证"回答里的证据"可以回溯到真实记录，而用户与模型都看不到内部 ID。
type SessionView struct {
	ID           string   `json:"-"`
	Date         string   `json:"date"`
	Project      string   `json:"project"`
	Start        string   `json:"start"`
	End          string   `json:"end"`
	DurationMins float64  `json:"duration_minutes"`
	Apps         []string `json:"apps"`
	GitMessages  []string `json:"git_messages,omitempty"`
}

// SessionsResult 是 get_sessions 的返回结果。
type SessionsResult struct {
	Query    string        `json:"query"`
	Sessions []SessionView `json:"sessions"`
	// TotalMinutes 是本次返回时段的时长合计，避免模型自己算错。
	TotalMinutes float64 `json:"total_minutes"`
}

func (c *SessionsCapability) Run(ctx context.Context, args map[string]any) (any, error) {
	loc := c.Loc
	if loc == nil {
		loc = time.UTC
	}
	limit := c.MaxLimit
	if limit <= 0 {
		limit = 30
	}
	if v, ok := args["limit"].(int); ok && v > 0 && v < limit {
		limit = v
	}

	date, _ := args["date"].(string)
	project, _ := args["project"].(string)
	if date == "" && project == "" {
		return nil, fmt.Errorf("date 与 project 至少需要一个")
	}

	var sessions []storage.Session
	var err error
	query := ""
	if date != "" {
		if _, perr := time.ParseInLocation("2006-01-02", date, loc); perr != nil {
			return nil, fmt.Errorf("date 必须是 YYYY-MM-DD")
		}
		sessions, err = c.Store.SessionsByDate(ctx, date)
		query = "date=" + date
	} else {
		sessions, err = c.Store.SessionsByProject(ctx, project, limit)
		query = "project=" + project
	}
	if err != nil {
		return nil, err
	}
	if date != "" && len(sessions) > limit {
		sessions = sessions[:limit]
	}

	views := make([]SessionView, 0, len(sessions))
	var total float64
	for _, s := range sessions {
		v := sessionToView(s, loc)
		total += v.DurationMins
		views = append(views, v)
	}
	return SessionsResult{Query: query, Sessions: views, TotalMinutes: round1(total)}, nil
}

// sessionToView 把存储结构转成模型可见的最小视图。
func sessionToView(s storage.Session, loc *time.Location) SessionView {
	type appEntry struct {
		App             string  `json:"app"`
		DurationMinutes float64 `json:"duration_minutes"`
	}
	var apps []appEntry
	_ = json.Unmarshal([]byte(s.AppsJSON), &apps)
	appNames := make([]string, 0, len(apps))
	for _, a := range apps {
		if strings.TrimSpace(a.App) == "" {
			continue
		}
		appNames = append(appNames, fmt.Sprintf("%s %.0f 分钟", a.App, a.DurationMinutes))
	}

	var gits []struct {
		Branch  string `json:"branch"`
		Message string `json:"commit_message"`
	}
	_ = json.Unmarshal([]byte(s.GitJSON), &gits)
	msgs := make([]string, 0, len(gits))
	for _, g := range gits {
		if msg := firstLine(g.Message); msg != "" {
			msgs = append(msgs, truncateRunes(msg, 120))
		}
	}

	var stats struct {
		DurationMinutes float64 `json:"duration_minutes"`
	}
	_ = json.Unmarshal([]byte(s.StatsJSON), &stats)

	return SessionView{
		ID:           s.ID,
		Date:         s.Date,
		Project:      DisplayProject(s.Project),
		Start:        s.StartAt.In(loc).Format("2006-01-02 15:04"),
		End:          s.EndAt.In(loc).Format("15:04"),
		DurationMins: stats.DurationMinutes,
		Apps:         appNames,
		GitMessages:  msgs,
	}
}

// KnownProjectsCapability 列出最近出现过的项目名。
//
// 用途：用户说"那个项目"或名字记错时，模型先看看有哪些项目可对齐，
// 而不是凭空猜一个项目名去查询。
type KnownProjectsCapability struct {
	Store *storage.Store
	Loc   *time.Location
	// Days 往回看的天数。
	Days int
}

func (c *KnownProjectsCapability) Name() string { return "get_known_projects" }
func (c *KnownProjectsCapability) Description() string {
	return "列出最近出现过的项目名（可选参数 days，默认 14）。用户提到的项目名不确定时先用它对齐。"
}
func (c *KnownProjectsCapability) Parameters() map[string]ParamSpec {
	return map[string]ParamSpec{"days": {Type: "int"}}
}
func (c *KnownProjectsCapability) ReadOnly() bool { return true }

// KnownProjectsResult 是 get_known_projects 的返回结果。
type KnownProjectsResult struct {
	Projects []string `json:"projects"`
	Days     int      `json:"days"`
}

func (c *KnownProjectsCapability) Run(ctx context.Context, args map[string]any) (any, error) {
	loc := c.Loc
	if loc == nil {
		loc = time.UTC
	}
	days := c.Days
	if days <= 0 {
		days = 14
	}
	if v, ok := args["days"].(int); ok && v > 0 && v <= 90 {
		days = v
	}

	now := time.Now().In(loc)
	from := now.AddDate(0, 0, -days)
	to := now.AddDate(0, 0, 1)
	projects, err := c.Store.ProjectsWithActivity(ctx, from, to)
	if err != nil {
		return nil, err
	}
	display := make([]string, 0, len(projects))
	for _, p := range projects {
		display = append(display, DisplayProject(p))
	}
	sort.Strings(display)
	return KnownProjectsResult{Projects: display, Days: days}, nil
}

// TodayStatusCapability 返回"现在这一刻"的状态概览。
//
// 与 get_sessions 的区别：这个回答"今天到目前为止怎么样"，
// 不需要模型先算日期，减少模型算错日期的机会。
type TodayStatusCapability struct {
	Store *storage.Store
	Loc   *time.Location
}

func (c *TodayStatusCapability) Name() string { return "get_today_status" }
func (c *TodayStatusCapability) Description() string {
	return "返回今天的日期与今天的时段概览（相当于 date=今天的 get_sessions）。问“今天怎么样/今天做了什么”时用它。无参数。"
}
func (c *TodayStatusCapability) Parameters() map[string]ParamSpec { return nil }
func (c *TodayStatusCapability) ReadOnly() bool                   { return true }

// TodayStatusResult 是 get_today_status 的返回结果。
type TodayStatusResult struct {
	Date         string        `json:"date"`
	Sessions     []SessionView `json:"sessions"`
	TotalMinutes float64       `json:"total_minutes"`
}

func (c *TodayStatusCapability) Run(ctx context.Context, _ map[string]any) (any, error) {
	loc := c.Loc
	if loc == nil {
		loc = time.UTC
	}
	date := time.Now().In(loc).Format("2006-01-02")
	sessions, err := c.Store.SessionsByDate(ctx, date)
	if err != nil {
		return nil, err
	}
	views := make([]SessionView, 0, len(sessions))
	var total float64
	for _, s := range sessions {
		v := sessionToView(s, loc)
		total += v.DurationMins
		views = append(views, v)
	}
	return TodayStatusResult{Date: date, Sessions: views, TotalMinutes: round1(total)}, nil
}

// TaskSummariesCapability 读取专业 Agent 汇报的任务摘要。
//
// 与 get_sessions 的关系不是替代而是互补：
//   - get_sessions 回答"用了什么、多久"——只能推断；
//   - 本能力回答"Agent 报告做完了什么"——这是结论本身。
//
// 因此合成回答时必须区分两者：前者是 inferred，后者才是 supported。
// 这条区分是本能力存在的全部意义，不能在上层被抹平。
type TaskSummariesCapability struct {
	Store *storage.Store
	Loc   *time.Location
	// MaxLimit 限制单次返回条数，防止一次拉出整表。
	MaxLimit int
}

func (c *TaskSummariesCapability) Name() string { return "get_task_summaries" }
func (c *TaskSummariesCapability) Description() string {
	return "查询专业 Agent（如 ZCode）汇报的任务摘要：任务标题、状态（done/partial/blocked/abandoned/unknown）、" +
		"已产出结果、未完成事项。这是唯一带「结论」的数据源——" +
		"问「完成了什么/做完什么/还有什么没做完」时用它。" +
		"参数：date=YYYY-MM-DD（可选）、project=项目名（可选）、limit=条数上限（可选，默认 20）。" +
		"date 与 project 至少给一个。"
}
func (c *TaskSummariesCapability) Parameters() map[string]ParamSpec {
	return map[string]ParamSpec{
		"date":    {Type: "string", MaxLength: 10},
		"project": {Type: "string", MaxLength: 64},
		"limit":   {Type: "int"},
	}
}
func (c *TaskSummariesCapability) ReadOnly() bool { return true }

// TaskSummaryView 是给模型看的任务摘要视图。
//
// 刻意不含 SourceSessionID：那是来源 Agent 的内部会话 ID，只用于追溯，
// 既不该出现在模型的上下文里，也不该出现在用户可见文本里。
type TaskSummaryView struct {
	TaskID    string   `json:"task_id"`
	Project   string   `json:"project,omitempty"`
	App       string   `json:"app,omitempty"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Outcomes  []string `json:"outcomes"`
	OpenLoops []string `json:"open_loops"`
	// SourceAgent 说明这份结论是谁报告的（如 zcode-cli）。
	// 用户有权知道"这是 Agent 说的"而不是 Lumen 自己观察到的。
	SourceAgent string `json:"source_agent"`
	OccurredAt  string `json:"occurred_at"`
}

// TaskSummariesResult 是 get_task_summaries 的返回结果。
type TaskSummariesResult struct {
	Query         string            `json:"query"`
	TaskSummaries []TaskSummaryView `json:"task_summaries"`
	// Count 是本次返回的条数，避免模型自己数错。
	Count int `json:"count"`
	// StatusCounts 按状态汇总，方便模型直接说"3 个完成、1 个受阻"。
	StatusCounts map[string]int `json:"status_counts"`
}

func (c *TaskSummariesCapability) Run(ctx context.Context, args map[string]any) (any, error) {
	loc := c.Loc
	if loc == nil {
		loc = time.UTC
	}
	limit := c.MaxLimit
	if limit <= 0 {
		limit = 20
	}
	if v, ok := args["limit"].(int); ok && v > 0 && v < limit {
		limit = v
	}

	date, _ := args["date"].(string)
	project, _ := args["project"].(string)
	if date == "" && project == "" {
		return nil, fmt.Errorf("date 与 project 至少需要一个")
	}

	var list []storage.TaskSummary
	var err error
	query := ""
	if date != "" {
		day, perr := time.ParseInLocation("2006-01-02", date, loc)
		if perr != nil {
			return nil, fmt.Errorf("date 必须是 YYYY-MM-DD")
		}
		list, err = c.Store.TaskSummariesBetween(ctx, day, day.AddDate(0, 0, 1), limit)
		query = "date=" + date
	} else {
		list, err = c.Store.TaskSummariesByProject(ctx, project, limit)
		query = "project=" + project
	}
	if err != nil {
		return nil, err
	}

	views := make([]TaskSummaryView, 0, len(list))
	counts := map[string]int{}
	for _, t := range list {
		views = append(views, taskSummaryToView(t, loc))
		counts[t.Status]++
	}
	return TaskSummariesResult{
		Query: query, TaskSummaries: views, Count: len(views), StatusCounts: counts,
	}, nil
}

// taskSummaryToView 把存储结构转成模型可见的最小视图。
func taskSummaryToView(t storage.TaskSummary, loc *time.Location) TaskSummaryView {
	return TaskSummaryView{
		TaskID:      t.TaskID,
		Project:     DisplayProject(t.Project),
		App:         t.App,
		Title:       truncateRunes(t.Title, 120),
		Status:      t.Status,
		Outcomes:    clippedItems(t.Outcomes),
		OpenLoops:   clippedItems(t.OpenLoops),
		SourceAgent: t.SourceAgent,
		OccurredAt:  t.OccurredAt.In(loc).Format("2006-01-02 15:04"),
	}
}

// clippedItems 逐条限长并限量，避免历史数据里的超长内容进入上下文。
//
// 服务端入库时已经校验过，这里是防御性的第二道：数据库里可能存在
// 早期版本或人工写入的数据，不能假设它们一定符合当前约束。
func clippedItems(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if text := strings.TrimSpace(s); text != "" {
			out = append(out, truncateRunes(text, 120))
		}
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// DisplayProject 把内部项目标记转成用户可读名称。
//
// 与 feishu.ProjectDisplayName / ai.ProjectDisplayName 同一目的：
// unclassified 是内部标记，绝不能进入模型上下文或用户可见文本。
func DisplayProject(project string) string {
	if project == "" || project == "unclassified" {
		return "暂未识别项目"
	}
	return project
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}
