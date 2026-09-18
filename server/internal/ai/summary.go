package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"lumen/server/internal/storage"
)

// PromptVersion 是总结 prompt 的版本号，随 prompt 语义变化递增。
const PromptVersion = "summary-v1"

// SummarySystemPrompt 是总结的系统提示词。
// 约束重点：不确定就写进 uncertainties，禁止编造未提供的 commit、文件或完成状态。
const SummarySystemPrompt = `你是个人工作日志助手。用户会给你当天在电脑上的聚合工作数据（应用使用时长、Git 提交元数据）。

请输出严格 JSON，字段如下：
{
  "date": "YYYY-MM-DD",
  "headline": "一句话概括当天最主要的工作（不超过 40 字）",
  "projects": [
    {
      "name": "项目名",
      "duration_minutes": 数字,
      "activities": ["具体做了什么的短句，最多 4 条"],
      "evidence_session_ids": ["对应的 session id"]
    }
  ],
  "uncertainties": ["无法确定或证据不足的地方，最多 3 条"]
}

硬性规则：
1. 只使用提供的数据，禁止编造 commit、文件、会议、完成状态或未出现的项目；
2. 每个项目的 evidence_session_ids 必须来自输入中真实存在的 session id；
3. duration_minutes 必须与输入数据一致，不要自行估算；
4. 如果数据不足以判断某个项目在做什么，把这一点写进 uncertainties，而不是猜测；
5. activities 只能描述数据里有的东西：应用名、时长、Git 提交信息。
   应用名与时长**不等于**任务内容——数据里没有提交或明确线索时，
   写「使用 XX 约 N 分钟」这类陈述，不要把它包装成「完成了某项功能」；
6. 输出必须是合法 JSON，不要输出 markdown 代码块或额外解释。`

// ProjectDisplayName 把内部项目标记转成用户可读文案。
//
// unclassified 是内部存储与 API 的标记，不该出现在模型输入、总结正文
// 或任何用户可见位置：用户看到「暂未识别项目」才明白这是「不知道」，
// 看到 unclassified 只会以为系统出错了。
func ProjectDisplayName(project string) string {
	if project == SessionsUnclassified || project == "" {
		return "暂未识别项目"
	}
	return project
}

// SessionsUnclassified 与 sessions.Unclassified 保持一致。
//
// 用字面量而不是 import sessions 包：sessions 依赖 storage 与 events，
// 而 ai 只需要一个稳定的字符串标记。两端不一致由测试兜底。
const SessionsUnclassified = "unclassified"

// ProjectContext 是发送给模型的项目级聚合数据。
type ProjectContext struct {
	Name            string     `json:"name"`
	DurationMinutes float64    `json:"duration_minutes"`
	Sessions        []SessionC `json:"sessions"`
	GitHighlights   []string   `json:"git_highlights,omitempty"`
}

// SessionC 是发送给模型的单个 Session 摘要。
type SessionC struct {
	ID              string   `json:"id"`
	Start           string   `json:"start"`
	End             string   `json:"end"`
	DurationMinutes float64  `json:"duration_minutes"`
	Apps            []string `json:"apps"`
	GitMessages     []string `json:"git_messages,omitempty"`
}

// SummaryInput 是当天的完整上下文。
type SummaryInput struct {
	Date      string           `json:"date"`
	Timezone  string           `json:"timezone"`
	Projects  []ProjectContext `json:"projects"`
	TotalMins float64          `json:"total_minutes"`
	Note      string           `json:"note,omitempty"`
}

// StructuredSummary 是模型输出经校验后的结构。
type StructuredSummary struct {
	Date          string           `json:"date"`
	Headline      string           `json:"headline"`
	Projects      []SummaryProject `json:"projects"`
	Uncertainties []string         `json:"uncertainties"`
}

// SummaryProject 是输出中的单个项目。
type SummaryProject struct {
	Name               string   `json:"name"`
	DurationMinutes    float64  `json:"duration_minutes"`
	Activities         []string `json:"activities"`
	EvidenceSessionIDs []string `json:"evidence_session_ids"`
}

// BuildInput 把某天的 Session 转换成最小化的模型输入。
// 只包含项目、时间、应用名、Git 提交信息摘要，不含任何路径、标题或凭证。
func BuildInput(date string, loc *time.Location, sessions []storage.Session) SummaryInput {
	if loc == nil {
		loc = time.UTC
	}

	type appEntry struct {
		App             string  `json:"app"`
		DurationMinutes float64 `json:"duration_minutes"`
	}
	type gitEntry struct {
		Repo    string `json:"repo"`
		Branch  string `json:"branch"`
		Commit  string `json:"head_commit"`
		Message string `json:"commit_message"`
	}

	byProject := map[string]*ProjectContext{}
	var total float64
	order := []string{}

	for _, s := range sessions {
		// 分组仍按内部项目标识，但发给模型的名字用可读文案，
		// 否则模型会把 unclassified 原样写进总结正文。
		pc, ok := byProject[s.Project]
		if !ok {
			pc = &ProjectContext{Name: ProjectDisplayName(s.Project)}
			byProject[s.Project] = pc
			order = append(order, s.Project)
		}

		var apps []appEntry
		_ = json.Unmarshal([]byte(s.AppsJSON), &apps)
		var stats struct {
			DurationMinutes float64 `json:"duration_minutes"`
		}
		_ = json.Unmarshal([]byte(s.StatsJSON), &stats)

		appNames := make([]string, 0, len(apps))
		for _, a := range apps {
			if a.App != "" {
				appNames = append(appNames, a.App)
			}
		}

		var gits []gitEntry
		_ = json.Unmarshal([]byte(s.GitJSON), &gits)
		var gitMsgs []string
		for _, g := range gits {
			if strings.TrimSpace(g.Message) == "" {
				continue
			}
			// 只保留提交信息首行，避免超长正文。
			msg := firstLine(g.Message)
			if len(msg) > 120 {
				msg = msg[:120]
			}
			gitMsgs = append(gitMsgs, fmt.Sprintf("%s(%s): %s", g.Branch, shortCommit(g.Commit), msg))
		}

		dur := stats.DurationMinutes
		pc.DurationMinutes += dur
		total += dur
		pc.Sessions = append(pc.Sessions, SessionC{
			ID:              s.ID,
			Start:           s.StartAt.In(loc).Format("15:04"),
			End:             s.EndAt.In(loc).Format("15:04"),
			DurationMinutes: dur,
			Apps:            appNames,
			GitMessages:     gitMsgs,
		})
		pc.GitHighlights = append(pc.GitHighlights, gitMsgs...)
	}

	projects := make([]ProjectContext, 0, len(order))
	for _, name := range order {
		pc := byProject[name]
		pc.DurationMinutes = round1(pc.DurationMinutes)
		projects = append(projects, *pc)
	}
	// 按时长降序，让模型先看到主要工作。
	sort.SliceStable(projects, func(i, j int) bool { return projects[i].DurationMinutes > projects[j].DurationMinutes })

	return SummaryInput{
		Date:      date,
		Timezone:  loc.String(),
		Projects:  projects,
		TotalMins: round1(total),
	}
}

// InputHash 计算输入指纹，用于“同输入不重复生成”的幂等控制。
func InputHash(in SummaryInput) string {
	// 只对影响内容的字段计算哈希，序列化顺序固定。
	payload, _ := json.Marshal(in)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])[:32]
}

// SourceSessionIDs 返回输入中全部 session id，写入 summaries 表用于追溯。
func SourceSessionIDs(in SummaryInput) []string {
	var ids []string
	for _, p := range in.Projects {
		for _, s := range p.Sessions {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// UserPrompt 构造用户消息。
func UserPrompt(in SummaryInput) string {
	payload, _ := json.MarshalIndent(in, "", "  ")
	return "以下是当天的工作数据（JSON），请按系统提示的要求生成总结：\n" + string(payload)
}

// ParseSummary 解析并校验模型输出。
// 校验包括：JSON 合法性、必填字段、日期一致性、证据 session id 是否真实存在。
func ParseSummary(content, expectDate string, validSessionIDs map[string]bool) (StructuredSummary, error) {
	raw, err := ExtractJSON(content)
	if err != nil {
		return StructuredSummary{}, err
	}

	var out StructuredSummary
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return StructuredSummary{}, fmt.Errorf("总结 JSON 结构不符: %w", err)
	}

	if strings.TrimSpace(out.Date) != expectDate {
		// 日期不符说明模型理解错了范围，直接失败，避免把错误内容当成当天总结。
		return StructuredSummary{}, fmt.Errorf("总结日期不符: 期望 %s 实际 %s", expectDate, out.Date)
	}
	if strings.TrimSpace(out.Headline) == "" {
		return StructuredSummary{}, errors.New("总结缺少 headline")
	}
	if len(out.Headline) > 200 {
		out.Headline = out.Headline[:200]
	}

	cleaned := make([]SummaryProject, 0, len(out.Projects))
	for _, p := range out.Projects {
		if strings.TrimSpace(p.Name) == "" {
			continue
		}
		// 过滤模型臆造的 session id，保证证据可追溯。
		ids := make([]string, 0, len(p.EvidenceSessionIDs))
		for _, id := range p.EvidenceSessionIDs {
			if validSessionIDs[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			// 没有任何有效证据的项目不作为主要结论保留。
			continue
		}
		activities := make([]string, 0, len(p.Activities))
		for _, a := range p.Activities {
			if a = strings.TrimSpace(a); a != "" {
				if len(a) > 160 {
					a = a[:160]
				}
				activities = append(activities, a)
			}
			if len(activities) >= 4 {
				break
			}
		}
		cleaned = append(cleaned, SummaryProject{
			Name:               truncateRunes(p.Name, 64),
			DurationMinutes:    p.DurationMinutes,
			Activities:         activities,
			EvidenceSessionIDs: ids,
		})
	}
	out.Projects = cleaned

	if len(out.Uncertainties) > 3 {
		out.Uncertainties = out.Uncertainties[:3]
	}
	for i, u := range out.Uncertainties {
		out.Uncertainties[i] = truncateRunes(strings.TrimSpace(u), 160)
	}
	return out, nil
}

// Render 把结构化总结渲染成人类可读文本（飞书与查询接口共用）。
//
// 渲染时不再输出 Session ID：用户要看的是「今天做了什么」，
// 一长串内部 ID 只会让人以为系统出错。ID 仍保存在 structured_output 与
// source_session_ids 里，供 API 与审计追溯。
func Render(s StructuredSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📅 %s 工作总结\n\n", s.Date)
	fmt.Fprintf(&b, "%s\n", s.Headline)

	if len(s.Projects) > 0 {
		b.WriteString("\n")
		for _, p := range s.Projects {
			fmt.Fprintf(&b, "▍%s · 约 %s\n", ProjectDisplayName(p.Name), formatMinutes(p.DurationMinutes))
			for _, a := range p.Activities {
				fmt.Fprintf(&b, "  · %s\n", a)
			}
		}
	}
	if len(s.Uncertainties) > 0 {
		b.WriteString("\n不确定项：\n")
		for _, u := range s.Uncertainties {
			if u == "" {
				continue
			}
			fmt.Fprintf(&b, "  · %s\n", u)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// GenerateSummary 完成一次完整的总结生成：调用模型、解析、校验、渲染。
func GenerateSummary(ctx context.Context, client *Client, in SummaryInput) (StructuredSummary, string, Response, error) {
	valid := map[string]bool{}
	for _, id := range SourceSessionIDs(in) {
		valid[id] = true
	}

	resp, err := client.CompleteJSON(ctx, SummarySystemPrompt, UserPrompt(in), 2000)
	if err != nil {
		return StructuredSummary{}, "", Response{}, err
	}

	parsed, err := ParseSummary(resp.Content, in.Date, valid)
	if err != nil {
		return StructuredSummary{}, "", resp, err
	}
	return parsed, Render(parsed), resp, nil
}

// MarshalStructured 序列化结构化结果用于存储。
func MarshalStructured(s StructuredSummary) string {
	b, err := json.Marshal(s)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// MarshalUsage 序列化 token 用量用于存储。
func MarshalUsage(resp Response) string {
	b, err := json.Marshal(map[string]any{
		"prompt_tokens":     resp.Usage.PromptTokens,
		"completion_tokens": resp.Usage.CompletionTokens,
		"total_tokens":      resp.Usage.TotalTokens,
		"elapsed_ms":        resp.Elapsed.Milliseconds(),
	})
	if err != nil {
		return "{}"
	}
	return string(b)
}

// UnmarshalStructured 读取结构化结果，用于查询接口渲染。
func UnmarshalStructured(raw string) (StructuredSummary, error) {
	var out StructuredSummary
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return StructuredSummary{}, err
	}
	return out, nil
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func shortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

func formatMinutes(m float64) string {
	if m < 60 {
		return fmt.Sprintf("%.0f 分钟", m)
	}
	h := int(m) / 60
	min := int(m) % 60
	if min == 0 {
		return fmt.Sprintf("%d 小时", h)
	}
	return fmt.Sprintf("%d 小时 %d 分钟", h, min)
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
