package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/storage"
	"lumen/server/internal/ulid"
)

// 意图集合。
//
// 前四个是闲聊类，回答必须是确定性的（不检索数据、不调用模型）：
// 它们每天都会被问到，答错或答得千篇一律比答得短更让人难受。
const (
	IntentToday       = "today"
	IntentYesterday   = "yesterday"
	IntentProject     = "project"
	IntentGreeting    = "greeting"
	IntentIdentity    = "identity"
	IntentCapability  = "capability"
	IntentSignoff     = "signoff"
	IntentUnsupported = "unsupported"
)

// UnclassifiedProject 与 sessions.Unclassified 保持一致。
//
// 用字面量而不是 import sessions 包：sessions 已经依赖 storage 与 events，
// 这里再引入会让 feishu 包与聚合引擎互相纠缠，而这里需要的只是一个字符串。
const UnclassifiedProject = "unclassified"

// ProjectDisplayName 把内部项目标记转成用户可读文案。
//
// 内部标记（unclassified）只用于存储、日志与 API，绝不能直接出现在飞书消息里。
func ProjectDisplayName(project string) string {
	if project == UnclassifiedProject || project == "" {
		return "暂未识别项目"
	}
	return project
}

// SignoffTextTemplate 是「我下班了」的回复模板；%s 处填入总结时间。
//
// V0.1 的取舍：这里**不**即时生成总结。
// 即时生成会用当天 22:30 之前的残缺数据跑一次模型，晚间的自动总结又会
// 因为数据变化再生成一次，结果是同一天推送两条、且第一条是残缺版本。
// 因此这里只做明确确认并告知总结时间，真正的总结仍交给每日定时任务。
const SignoffTextTemplate = "收到，今天的工作记录就到这里。\n" +
	"今晚 %s 我会把今天的总结发给你。"

var (
	todayRe     = regexp.MustCompile(`(今天|今日|today)`)
	yesterdayRe = regexp.MustCompile(`(昨天|昨日|yesterday)`)
	projectRe   = regexp.MustCompile(`(?:项目|project)\s*([^\s的做了什么最近推进入]*)|([A-Za-z0-9_\-\.]+)\s*(?:项目|最近做了什么|进展)`)
	greetingRe  = regexp.MustCompile(`^(你好|您好|hi|hello|hey|在吗|在么|早上好|下午好|晚上好|早|哈喽)[呀啊哈!！。~\s]*$`)
	identityRe  = regexp.MustCompile(`(你叫什么|你是谁|你的名字|你是什么|介绍一下你|who are you|what are you)`)
	abilityRe   = regexp.MustCompile(`(你能做什么|你会做什么|你会什么|你能干什么|你能干嘛|你会干嘛|有什么功能|能帮我做什么|使用说明|帮助|help)`)
	signoffRe   = regexp.MustCompile(`(下班了|收工|今天先到这|今天就到这|不干了|我走了|结束今天)`)
)

// Query 是一次解析后的问答请求。
type Query struct {
	Intent  string `json:"intent"`
	Date    string `json:"date,omitempty"`
	Project string `json:"project,omitempty"`
	Raw     string `json:"raw,omitempty"`
}

// Answer 是一次问答的结果。
type Answer struct {
	Text             string
	Intent           string
	SourceSessionIDs []string
	Status           string
	UsedAI           bool
}

// ParseQuery 解析用户消息。
//
// 解析顺序刻意如此：具体意图优先于泛化意图。例如「今天先到这」同时命中
// signoff 与 today 关键词，必须先按 signoff 处理，否则会被当成查询今天的记录。
func ParseQuery(text string) Query {
	t := strings.TrimSpace(text)
	lower := strings.ToLower(t)

	if m := projectRe.FindStringSubmatch(t); m != nil {
		name := strings.TrimSpace(m[1])
		if name == "" {
			name = strings.TrimSpace(m[2])
		}
		if name != "" {
			if isEngineeringTerm(name) {
				// 内部术语不是项目名，没必要去检索后把术语回显给用户。
				return Query{Intent: IntentUnsupported, Raw: t}
			}
			return Query{Intent: IntentProject, Project: name, Raw: t}
		}
	}
	// 闲聊类意图：确定性的固定回复，不检索数据、不调用模型。
	if signoffRe.MatchString(t) {
		return Query{Intent: IntentSignoff, Raw: t}
	}
	if identityRe.MatchString(lower) {
		return Query{Intent: IntentIdentity, Raw: t}
	}
	if abilityRe.MatchString(lower) {
		return Query{Intent: IntentCapability, Raw: t}
	}
	if greetingRe.MatchString(lower) {
		return Query{Intent: IntentGreeting, Raw: t}
	}
	if yesterdayRe.MatchString(lower) {
		return Query{Intent: IntentYesterday, Raw: t}
	}
	if todayRe.MatchString(lower) {
		return Query{Intent: IntentToday, Raw: t}
	}
	// “做了什么”这类问法默认按今天处理。
	if strings.Contains(t, "做了什么") || strings.Contains(t, "干了什么") {
		return Query{Intent: IntentToday, Raw: t}
	}
	return Query{Intent: IntentUnsupported, Raw: t}
}

// isEngineeringTerm 判断查询词是否是内部术语。
//
// 用户在飞书上不会用 unclassified 提问，但调试接口可能这样传。
// 与其把它当成项目名去检索并回显，不如按无法识别处理。
func isEngineeringTerm(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	return lower == UnclassifiedProject || lower == "session" || lower == "sessions"
}

// QAService 负责检索 Session 并组织回答。
type QAService struct {
	store  *storage.Store
	client *ai.Client
	loc    *time.Location
	// queryDailyLimit 是问答每天可调用 DeepSeek 的次数上限。
	queryDailyLimit int
	// summaryHour / summaryMinute 是每日总结的触发时间。
	//
	// 必须与调度器使用同一个配置值：「我下班了」的回复要告诉用户总结什么时候来，
	// 写死时间会在配置被改动后骗人。
	summaryHour   int
	summaryMinute int
}

// NewQAService 创建问答服务。
func NewQAService(store *storage.Store, client *ai.Client, loc *time.Location,
	queryDailyLimit, summaryHour, summaryMinute int) *QAService {
	if loc == nil {
		loc = time.UTC
	}
	if queryDailyLimit <= 0 {
		queryDailyLimit = 20
	}
	if summaryHour < 0 || summaryHour > 23 {
		summaryHour = 22
	}
	if summaryMinute < 0 || summaryMinute > 59 {
		summaryMinute = 30
	}
	return &QAService{
		store: store, client: client, loc: loc, queryDailyLimit: queryDailyLimit,
		summaryHour: summaryHour, summaryMinute: summaryMinute,
	}
}

// summaryTimeLabel 返回形如 22:30 的总结时间，用于用户可见文案。
func (s *QAService) summaryTimeLabel() string {
	return fmt.Sprintf("%02d:%02d", s.summaryHour, s.summaryMinute)
}

// HelpText 是无法识别意图时的帮助文本。
const HelpText = "你好，我是 Lumen。目前支持三类查询：\n" +
	"1. 今天做了什么\n" +
	"2. 昨天做了什么\n" +
	"3. 项目 <名称> 最近做了什么"

// GreetingText 是问候的回复：短、自报身份，但不倾倒功能菜单。
const GreetingText = "你好，我是 Lumen，在记录你今天在电脑上的工作。" +
	"想问点什么直接说，比如「今天做了什么」。"

// IdentityText 是身份询问的回复。
const IdentityText = "我是 Lumen，你自己的工作时间记录助手。" +
	"数据存在你自己的服务器上，我只看得到你用了哪些应用、用了多久。"

// CapabilityText 是能力询问的回复。
//
// 必须说明边界：V0.1 只记录应用名与时长，看不到窗口标题、网页内容或代码，
// 所以做不到「总结你写了什么功能」。
const CapabilityText = "我能回答三类问题：\n" +
	"1. 今天做了什么\n" +
	"2. 昨天做了什么\n" +
	"3. 项目 <名称> 最近做了什么\n\n" +
	"记录范围只有应用名和使用时长，看不到窗口标题、网页内容或代码，\n" +
	"所以我能告诉你用了哪些应用、各多久，但不能替你判断具体写了什么。"

// Handle 处理一条用户消息，返回回答文本与证据 Session。
func (s *QAService) Handle(ctx context.Context, q Query) (Answer, error) {
	now := time.Now().In(s.loc)
	switch q.Intent {
	case IntentToday:
		return s.answerForDate(ctx, now, "今天", IntentToday)
	case IntentYesterday:
		return s.answerForDate(ctx, now.AddDate(0, 0, -1), "昨天", IntentYesterday)
	case IntentProject:
		return s.answerForProject(ctx, q.Project)
	case IntentGreeting:
		return Answer{Text: GreetingText, Intent: IntentGreeting, Status: "small_talk"}, nil
	case IntentIdentity:
		return Answer{Text: IdentityText, Intent: IntentIdentity, Status: "small_talk"}, nil
	case IntentCapability:
		return Answer{Text: CapabilityText, Intent: IntentCapability, Status: "small_talk"}, nil
	case IntentSignoff:
		text := fmt.Sprintf(SignoffTextTemplate, s.summaryTimeLabel())
		return Answer{Text: text, Intent: IntentSignoff, Status: "small_talk"}, nil
	default:
		return Answer{Text: HelpText, Intent: IntentUnsupported, Status: "help"}, nil
	}
}

func (s *QAService) answerForDate(ctx context.Context, day time.Time, label, intent string) (Answer, error) {
	date := day.Format("2006-01-02")
	sessions, err := s.store.SessionsByDate(ctx, date)
	if err != nil {
		return Answer{}, err
	}
	if len(sessions) == 0 {
		// 没有数据时明确说明，不调用模型编造内容。
		return Answer{
			Text:   fmt.Sprintf("%s（%s）没有记录到可识别的工作时段。如果刚恢复网络，可以稍后再试。", label, date),
			Intent: intent, Status: "no_data",
		}, nil
	}
	return s.answerWithSessions(ctx, sessions, fmt.Sprintf("%s（%s）", label, date), intent)
}

func (s *QAService) answerForProject(ctx context.Context, project string) (Answer, error) {
	from := time.Now().In(s.loc).AddDate(0, 0, -14)
	to := time.Now().In(s.loc).AddDate(0, 0, 1)
	projects, err := s.store.ProjectsWithActivity(ctx, from, to)
	if err != nil {
		return Answer{}, err
	}

	matched := matchProject(project, projects)
	if matched == "" {
		names := make([]string, 0, len(projects))
		for _, p := range projects {
			names = append(names, ProjectDisplayName(p))
		}
		return Answer{
			Text: fmt.Sprintf("最近 14 天没有找到项目「%s」的记录。已知项目：%s\n可以试试上面列出的准确名称。",
				ProjectDisplayName(project), strings.Join(names, "、")),
			Intent: IntentProject, Status: "no_data",
		}, nil
	}

	sessions, err := s.store.SessionsByProject(ctx, matched, 30)
	if err != nil {
		return Answer{}, err
	}
	if len(sessions) == 0 {
		return Answer{
			Text:   fmt.Sprintf("项目「%s」最近没有记录。", ProjectDisplayName(matched)),
			Intent: IntentProject, Status: "no_data",
		}, nil
	}
	return s.answerWithSessions(ctx, sessions,
		fmt.Sprintf("项目「%s」", ProjectDisplayName(matched)), IntentProject)
}

// answerWithSessions 判断是否需要调用模型。
//
// 设计取舍：证据很少时直接返回确定性摘要，不消耗模型预算，也避免模型在小样本上编故事。
func (s *QAService) answerWithSessions(ctx context.Context, sessions []storage.Session, label, intent string) (Answer, error) {
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.ID)
	}

	deterministic := renderDeterministic(label, sessions, s.loc)
	if !s.client.Enabled() || s.budgetExhausted(ctx) {
		return Answer{Text: deterministic, Intent: intent, SourceSessionIDs: ids, Status: "ok_no_ai"}, nil
	}

	in := ai.BuildInput(sessions[0].Date, s.loc, sessions)
	in.Note = "这是回答用户提问时的检索结果，请用简洁口语总结，并指出不确定的部分。"

	resp, err := s.client.CompleteJSON(ctx, querySystemPrompt, ai.UserPrompt(in), 1200)
	if err != nil {
		// 模型失败时降级为确定性摘要，保证用户总能得到带证据的回答。
		return Answer{Text: deterministic, Intent: intent, SourceSessionIDs: ids, Status: "ai_failed_fallback"}, nil
	}
	if err := s.recordUsage(ctx, resp); err != nil {
		// 用量记录失败不影响回答。
		_ = err
	}

	text, err := parseQueryAnswer(resp.Content)
	if err != nil || strings.TrimSpace(text) == "" {
		return Answer{Text: deterministic, Intent: intent, SourceSessionIDs: ids, Status: "ai_invalid_fallback"}, nil
	}

	// 回答必须附证据范围，这里统一在末尾追加真实来源，避免模型漏写。
	full := text + "\n\n" + evidenceLine(sessions, s.loc)
	return Answer{Text: full, Intent: intent, SourceSessionIDs: ids, Status: "ok", UsedAI: true}, nil
}

// budgetExhausted 检查问答的每日调用预算。
func (s *QAService) budgetExhausted(ctx context.Context) bool {
	date := time.Now().In(s.loc).Format("2006-01-02")
	usage, err := s.store.AIUsageToday(ctx, date, "query")
	if err != nil {
		return false
	}
	return usage.Calls >= s.queryDailyLimit
}

func (s *QAService) recordUsage(ctx context.Context, resp ai.Response) error {
	date := time.Now().In(s.loc).Format("2006-01-02")
	return s.store.AddAIUsage(ctx, date, "query", resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
}

// RecordConversation 保存问答记录的最小字段。
func (s *QAService) RecordConversation(ctx context.Context, messageID, userID string, q Query, a Answer) error {
	queryJSON, _ := json.Marshal(q)
	sourceIDs, _ := json.Marshal(a.SourceSessionIDs)
	status := a.Status
	if status == "" {
		status = "ok"
	}
	return s.store.RecordConversation(ctx, ulid.New(), messageID, userID, a.Intent,
		string(queryJSON), a.Text, string(sourceIDs), status)
}

// AlreadyHandled 判断消息是否已处理过，保证飞书重复投递不重复处理。
func (s *QAService) AlreadyHandled(ctx context.Context, messageID string) (bool, error) {
	return s.store.ConversationExists(ctx, messageID)
}

const querySystemPrompt = `你是个人工作助手。用户询问自己做了什么，你只能依据给出的 Session 数据回答。

输出严格 JSON：{"answer": "回答文本"}

硬性规则：
1. 只使用给定数据，禁止编造未出现的项目、提交或完成状态；
2. 如果数据不足，明确说“记录里看不出来”，不要推测；
3. 回答用简洁口语，控制在 200 字以内；
4. 不要输出 markdown 代码块或额外解释。`

func parseQueryAnswer(content string) (string, error) {
	raw, err := ai.ExtractJSON(content)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	return strings.TrimSpace(parsed.Answer), nil
}

// renderDeterministic 生成不依赖模型的确定性摘要。
//
// 措辞刻意克制：只陈述「用了哪些应用、各多久」，不把应用时长包装成
// 「完成了什么任务」——V0.1 看不到窗口标题与内容，说得出任务就是编造。
func renderDeterministic(label string, sessions []storage.Session, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s的工作记录：\n", label)

	type appEntry struct {
		App             string  `json:"app"`
		DurationMinutes float64 `json:"duration_minutes"`
	}

	type projectAgg struct {
		name      string
		minutes   float64
		apps      []string
		hasGit    bool
		gitMsgs   []string
		appCounts map[string]float64
	}

	order := []string{}
	byProject := map[string]*projectAgg{}

	for _, s := range sessions {
		var stats struct {
			DurationMinutes float64 `json:"duration_minutes"`
		}
		_ = json.Unmarshal([]byte(s.StatsJSON), &stats)

		agg, ok := byProject[s.Project]
		if !ok {
			agg = &projectAgg{name: s.Project, appCounts: map[string]float64{}}
			byProject[s.Project] = agg
			order = append(order, s.Project)
		}
		agg.minutes += stats.DurationMinutes

		var apps []appEntry
		_ = json.Unmarshal([]byte(s.AppsJSON), &apps)
		for _, a := range apps {
			if strings.TrimSpace(a.App) == "" {
				continue
			}
			agg.appCounts[a.App] += a.DurationMinutes
		}

		var gits []struct {
			Branch  string `json:"branch"`
			Message string `json:"commit_message"`
		}
		_ = json.Unmarshal([]byte(s.GitJSON), &gits)
		for _, g := range gits {
			msg := strings.TrimSpace(g.Message)
			if msg == "" {
				continue
			}
			agg.hasGit = true
			if i := strings.IndexAny(msg, "\r\n"); i >= 0 {
				msg = msg[:i]
			}
			if len(msg) > 80 {
				msg = msg[:80]
			}
			agg.gitMsgs = append(agg.gitMsgs, msg)
		}
	}

	for _, key := range order {
		agg := byProject[key]
		fmt.Fprintf(&b, "▍%s · 约 %.0f 分钟\n", ProjectDisplayName(agg.name), agg.minutes)
		for _, app := range topApps(agg.appCounts, 3) {
			fmt.Fprintf(&b, "  · %s\n", app)
		}
		for _, msg := range dedupe(agg.gitMsgs) {
			fmt.Fprintf(&b, "  · 提交：%s\n", msg)
		}
		if !agg.hasGit {
			// 证据只有应用与时长时说明局限，避免用户以为我们知道他在做什么。
			b.WriteString("  · 只有应用与时长记录，看不出具体做了什么\n")
		}
	}
	b.WriteString("\n" + evidenceLine(sessions, loc))
	return strings.TrimRight(b.String(), "\n")
}

// topApps 返回时长最多的前 n 个应用（形如「ZCode 约 23 分钟」）。
func topApps(counts map[string]float64, n int) []string {
	type entry struct {
		app     string
		minutes float64
	}
	list := make([]entry, 0, len(counts))
	for app, minutes := range counts {
		list = append(list, entry{app: app, minutes: minutes})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].minutes > list[j].minutes })
	if len(list) > n {
		list = list[:n]
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, fmt.Sprintf("%s 约 %.0f 分钟", e.app, e.minutes))
	}
	return out
}

// evidenceLine 生成证据时间范围，附在每条回答末尾。
//
// 只给时间范围，不给内部 ID：用户要看的是「这段时间有记录」，
// 内部 ID 只在 API 与审计里出现，写进飞书消息只会增加噪声。
//
// 时区必须用服务配置的 loc，不能取 session 时间自身的 Location()：
// 数据库里存的是 UTC，解析出来的时间也是 UTC，直接格式化会让用户看到
// 比实际早 8 小时的时间段。
func evidenceLine(sessions []storage.Session, loc *time.Location) string {
	if len(sessions) == 0 {
		return ""
	}
	if loc == nil {
		loc = time.UTC
	}

	earliest := sessions[0].StartAt
	latest := sessions[len(sessions)-1].EndAt
	for _, sess := range sessions {
		if sess.StartAt.Before(earliest) {
			earliest = sess.StartAt
		}
		if sess.EndAt.After(latest) {
			latest = sess.EndAt
		}
	}
	start, end := earliest.In(loc), latest.In(loc)
	// 同一天时不重复写日期，跨天才写完整区间。
	span := fmt.Sprintf("%s ~ %s", start.Format("01-02 15:04"), end.Format("15:04"))
	if start.Format("2006-01-02") != end.Format("2006-01-02") {
		span = fmt.Sprintf("%s ~ %s", start.Format("01-02 15:04"), end.Format("01-02 15:04"))
	}
	return fmt.Sprintf("证据：%d 段记录，%s", len(sessions), span)
}

// matchProject 在已知项目名中做不区分大小写的包含匹配。
func matchProject(query string, known []string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return ""
	}
	for _, k := range known {
		if strings.EqualFold(k, q) {
			return k
		}
	}
	for _, k := range known {
		if strings.Contains(strings.ToLower(k), q) {
			return k
		}
	}
	return ""
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
