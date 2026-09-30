// Package tools 是 Agent 的**第一批具体工具**（application 层）。
//
// 每个工具只做一件事：把 storage 里的记录（或身份、时间、对话状态）取出来，
// 转成"给模型看的最小视图 + 给用户看的确定性事实行 + 可核实的证据 ID"。
// 权限、参数校验、执行顺序、审计都不在这里——它们在 internal/tooling。
//
// 设计约束（刻意如此，不要放宽）：
//   - 没有任何工具接受 SQL、Shell、文件路径或网络地址；
//   - 没有任何工具能读到窗口标题、网页内容、剪贴板、代码或对话原文；
//   - 模型不能通过参数指定"读谁的数据"：请求者身份由代码注入（Invocation.Actor）；
//   - 视图里不出现内部 ID：记录 ID 只走 Result.Evidence，供审计，不进模型上下文。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"lumen/server/internal/storage"
	"lumen/server/internal/tooling"
)

// UnclassifiedProject 是"没能归类到项目"的内部标记。
//
// 它与 storage/sessions 里的取值保持一致；用字面量而不是 import 该包，
// 避免工具层与聚合引擎互相纠缠。
const UnclassifiedProject = "unclassified"

// DisplayProject 把内部项目标记转成用户可读名称。
//
// unclassified 是内部标记，绝不能进入模型上下文或用户可见文本。
func DisplayProject(project string) string {
	if project == "" || project == UnclassifiedProject {
		return "暂未识别项目"
	}
	return project
}

// SessionView 是给模型看的时段视图：只有项目、时间、应用与时长、提交信息。
type SessionView struct {
	Date            string   `json:"date"`
	Project         string   `json:"project"`
	Start           string   `json:"start"`
	End             string   `json:"end"`
	DurationMinutes float64  `json:"duration_minutes"`
	Apps            []string `json:"apps"`
	GitMessages     []string `json:"git_messages,omitempty"`
}

// sessionView 把存储结构转成模型可见的最小视图。
func sessionView(s storage.Session, loc *time.Location) SessionView {
	apps := appLines(s.AppsJSON)
	msgs := gitMessages(s.GitJSON)
	return SessionView{
		Date:            s.Date,
		Project:         DisplayProject(s.Project),
		Start:           s.StartAt.In(loc).Format("2006-01-02 15:04"),
		End:             s.EndAt.In(loc).Format("15:04"),
		DurationMinutes: sessionMinutes(s),
		Apps:            apps,
		GitMessages:     msgs,
	}
}

// sessionEvidence 把一批时段转成可核实的证据 ID 与覆盖区间。
//
// 区间取"最早开始 ~ 最晚结束"：按项目查是倒序、按日期查是正序，
// 取首尾元素会写出一个根本不存在的区间（用户核对时就会发现对不上）。
func sessionEvidence(sessions []storage.Session) (ids []string, span tooling.Span) {
	ids = make([]string, 0, len(sessions))
	spans := make([]tooling.Span, 0, len(sessions))
	for _, s := range sessions {
		if s.ID != "" {
			ids = append(ids, s.ID)
		}
		spans = append(spans, tooling.Span{Start: s.StartAt, End: s.EndAt})
	}
	return ids, tooling.MergeSpans(spans...)
}

// sessionDigest 渲染一批时段的确定性事实行（合成失败时直接陈述给用户）。
//
// 只陈述记录里确有的东西：项目、时长、应用、提交信息。
// 刻意不写"完成了什么"——活动记录推不出结论。
func sessionDigest(label string, sessions []storage.Session, loc *time.Location) []string {
	if len(sessions) == 0 {
		return []string{fmt.Sprintf("%s我这边没有查到记录。", label)}
	}
	total := 0.0
	for _, s := range sessions {
		total += sessionMinutes(s)
	}
	lines := []string{fmt.Sprintf("%s共 %d 段记录，合计约 %.0f 分钟：", label, len(sessions), total)}
	for _, s := range sessions {
		v := sessionView(s, loc)
		lines = append(lines, fmt.Sprintf("▍%s · 约 %.0f 分钟（%s ~ %s）",
			v.Project, v.DurationMinutes, shortTime(v.Start), v.End))
		for _, app := range v.Apps {
			lines = append(lines, "  · "+app)
		}
		for _, msg := range v.GitMessages {
			lines = append(lines, "  · 提交："+msg)
		}
		if len(v.GitMessages) == 0 {
			lines = append(lines, "  · 只有应用与时长记录，看不出具体做了什么")
		}
	}
	return lines
}

// appLines 解析应用与时长，逐条限长限量后渲染成"应用 N 分钟"。
func appLines(appsJSON string) []string {
	var apps []struct {
		App             string  `json:"app"`
		DurationMinutes float64 `json:"duration_minutes"`
	}
	_ = json.Unmarshal([]byte(appsJSON), &apps)
	out := make([]string, 0, len(apps))
	for _, a := range apps {
		if strings.TrimSpace(a.App) == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%s %.0f 分钟", a.App, a.DurationMinutes))
		if len(out) >= maxAppsPerView {
			break
		}
	}
	return out
}

// gitMessages 解析提交信息，只取首行并逐条限长限量。
func gitMessages(gitJSON string) []string {
	var gits []struct {
		Message string `json:"commit_message"`
	}
	_ = json.Unmarshal([]byte(gitJSON), &gits)
	out := make([]string, 0, len(gits))
	for _, g := range gits {
		if msg := firstLine(g.Message); msg != "" {
			out = append(out, truncateRunes(msg, maxGitMessageRunes))
		}
		if len(out) >= maxGitMessagesPerView {
			break
		}
	}
	return out
}

// sessionMinutes 读取聚合出的时长；缺失时返回 0 而不是猜测。
func sessionMinutes(s storage.Session) float64 {
	var stats struct {
		DurationMinutes float64 `json:"duration_minutes"`
	}
	_ = json.Unmarshal([]byte(s.StatsJSON), &stats)
	return round1(stats.DurationMinutes)
}

// StatusCounts 汇总任务状态，方便模型直接说"3 个完成、1 个受阻"。
func statusCounts(list []storage.TaskSummary) map[string]int {
	counts := map[string]int{}
	for _, t := range list {
		counts[t.Status]++
	}
	return counts
}

// clipItems 逐条限长并限量，避免历史数据里的超长内容进入上下文。
//
// 入库时已经校验过，这里是防御性的第二道：库里可能存在早期版本或人工写入
// 的数据，不能假设它们一定符合当前约束。
func clipItems(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if text := strings.TrimSpace(s); text != "" {
			out = append(out, truncateRunes(text, maxItemRunes))
		}
		if len(out) >= maxItemsPerView {
			break
		}
	}
	return out
}

// sortedUnique 去重并排序，保证同样的输入渲染出同样的目录文本。
func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
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

// shortTime 把 "2006-01-02 15:04" 压成 "01-02 15:04"，让事实摘要更紧凑。
func shortTime(s string) string {
	if len(s) >= 16 {
		return s[5:]
	}
	return s
}

// 视图限额：进入提示词的内容必须有上限，否则一次检索就可能超长。
const (
	maxAppsPerView        = 6
	maxGitMessagesPerView = 4
	maxGitMessageRunes    = 120
	maxItemsPerView       = 8
	maxItemRunes          = 120
	maxSessionsPerView    = 30
)

// 本包对 storage 的最小依赖。用窄接口而不是具体类型：
// 工具只声明自己真正用到的方法，测试也可以注入假实现。
type Store interface {
	SessionsByDate(ctx context.Context, date string) ([]storage.Session, error)
	SessionsByProject(ctx context.Context, project string, limit int) ([]storage.Session, error)
	ProjectsWithActivity(ctx context.Context, from, to time.Time) ([]string, error)
	TaskSummariesBetween(ctx context.Context, from, to time.Time, limit int) ([]storage.TaskSummary, error)
	TaskSummariesByProject(ctx context.Context, project string, limit int) ([]storage.TaskSummary, error)
	ConversationStateByUser(ctx context.Context, userID string) (storage.ConversationState, error)
	SaveMemoryCandidate(ctx context.Context, c storage.MemoryCandidate) error
}
