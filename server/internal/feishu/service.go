package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/assistant"
	"lumen/server/internal/storage"
	"lumen/server/internal/ulid"
)

// QAService 是飞书问答与只读调试接口共用的入口。
//
// 它现在是 assistant.Agent 的薄适配层：语义判断全部交给 Agent
// （模型出计划 → Policy Gate 审批 → 执行受限能力 → 合成回答），
// 这里只负责"把飞书的输入转成一次轮次、把结果记进 conversations 表"。
//
// 保留这个类型名是因为 bot.go 与 api/server.go 都以它为依赖点，
// 改名会扩散到不影响功能的调用方；语义上它已经是 Orchestrator 的封装。
type QAService struct {
	agent *assistant.Agent
	store *storage.Store
	loc   *time.Location
	// fallback 只在模型完全不可用时兜底，不是默认入口。
	fallback *assistant.MinimalFallback
	logger   *slog.Logger
}

// QAServiceOptions 是构造问答服务的依赖。
type QAServiceOptions struct {
	Store  *storage.Store
	Client *ai.Client
	Loc    *time.Location
	// Planner 允许测试注入假模型；生产环境留空即使用 DeepSeek。
	//
	// 存在的意义：验证"模型给出的计划"导致的代码行为（工具选择、权限拦截、
	// 来源校验），而不是把固定回复文案当成 AI 质量断言。
	Planner assistant.Planner
	Profile assistant.Profile
	// QueryDailyLimit 是问答每天可调用模型的次数上限（plan 与 answer 各计一次）。
	QueryDailyLimit int
	// SummaryHour / SummaryMinute 由 Profile 的主动性描述引用，用于提示词里
	// 说明助手什么时候会主动说话。
	SummaryHour   int
	SummaryMinute int
	Logger        *slog.Logger
}

// NewQAService 创建问答服务。
func NewQAService(opts QAServiceOptions) *QAService {
	loc := opts.Loc
	if loc == nil {
		loc = time.UTC
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	profile := opts.Profile.Normalize()

	registry := assistant.NewRegistry(
		&assistant.ProfileCapability{Profile: profile},
		&assistant.TodayStatusCapability{Store: opts.Store, Loc: loc},
		&assistant.SessionsCapability{Store: opts.Store, Loc: loc},
		&assistant.KnownProjectsCapability{Store: opts.Store, Loc: loc, Days: 14},
		// 任务摘要是唯一带结论的数据源，必须能被独立检索：
		// 混进 get_sessions 会让"Agent 报告的结论"和"应用时长的推断"分不开。
		&assistant.TaskSummariesCapability{Store: opts.Store, Loc: loc},
	)

	planner := opts.Planner
	if planner == nil {
		planner = &assistant.LLMPlanner{Client: opts.Client}
	}

	agent := assistant.NewAgent(assistant.Options{
		Store:    opts.Store,
		Planner:  planner,
		Registry: registry,
		Profile:  profile,
		Location: loc,
		Budget:   &agentBudget{store: opts.Store, loc: loc, dailyLimit: opts.QueryDailyLimit},
		Logger:   logger,
	})

	return &QAService{
		agent:    agent,
		store:    opts.Store,
		loc:      loc,
		fallback: &assistant.MinimalFallback{Store: opts.Store, Loc: loc},
		logger:   logger,
	}
}

// Agent 暴露编排器，供调试接口与测试直接使用。
func (s *QAService) Agent() *assistant.Agent { return s.agent }

// Profile 返回当前生效的身份配置。
func (s *QAService) Profile() assistant.Profile { return s.agent.Profile() }

// Validate 检查依赖是否齐全，供启动期快速失败。
func (s *QAService) Validate() error {
	if s == nil || s.agent == nil {
		return errors.New("问答服务未初始化")
	}
	if s.store == nil {
		return errors.New("问答服务缺少存储")
	}
	return s.agent.Validate()
}

// Handle 处理一条用户消息。
//
// 默认路径是 AI-first 的 Agent；只有在模型完全不可用（未配置/预算耗尽/调用失败）
// 且这句话属于最明确的数据请求时，才退回最小确定性兜底。
func (s *QAService) Handle(ctx context.Context, userID, text string) (assistant.Reply, error) {
	reply, err := s.agent.Handle(ctx, assistant.Turn{UserID: userID, Text: text})
	if err != nil {
		return assistant.Reply{}, err
	}

	// 模型不可用时，只对最明确的数据请求做确定性兜底（见 MinimalFallback 注释）。
	if reply.Status == "fallback_no_model" && s.fallback.CanHandle(text) {
		if body, ferr := s.fallback.Handle(ctx); ferr == nil && body != "" {
			reply.Text = body
			reply.Mode = assistant.ModeRecall
			reply.SupportLevel = assistant.SupportSupported
			reply.Status = "fallback_no_model_with_facts"
		}
	}
	return reply, nil
}

// RecordConversation 保存问答记录的最小字段。
//
// query_json 只记模式与工具名，不记用户原话全文之外的额外内容；
// 用户原话本身是必要的（用于排障与幂等），保存时不额外扩散。
func (s *QAService) RecordConversation(ctx context.Context, messageID, userID string,
	text string, reply assistant.Reply) error {
	queryJSON := marshalQuery(text, reply)
	// 时段与任务两种来源合并落库：conversations 表只有一个来源字段，
	// 而排障时更需要"这条回答的依据能不能回溯"，而不是"依据属于哪一类"。
	sourceIDs := marshalIDs(append(append([]string{}, reply.SourceSessionIDs...), reply.SourceTaskIDs...))
	status := reply.Status
	if status == "" {
		status = "ok"
	}
	return s.store.RecordConversation(ctx, ulid.New(), messageID, userID, reply.Mode,
		queryJSON, reply.Text, sourceIDs, status)
}

// AlreadyHandled 判断消息是否已处理过，保证飞书重复投递不重复处理。
func (s *QAService) AlreadyHandled(ctx context.Context, messageID string) (bool, error) {
	return s.store.ConversationExists(ctx, messageID)
}

// marshalQuery 把本轮的结构信息压成 conversations.query_json。
//
// 只记用户原话与模式、工具名，不记模型提示词与能力返回内容：
// 前者含大量内部细节，后者是用户数据，落库只会扩大泄露面。
func marshalQuery(text string, reply assistant.Reply) string {
	payload := map[string]any{
		"raw":        text,
		"mode":       reply.Mode,
		"tool_calls": reply.ToolCalls,
		// 支持等级一并留档：事后排查"模型当时是不是把推断说成了结论"，
		// 只看回答文本是看不出来的，得靠这个标记。
		"support_level": reply.SupportLevel,
	}
	if len(reply.DeniedTools) > 0 {
		denied := make([]map[string]string, 0, len(reply.DeniedTools))
		for _, d := range reply.DeniedTools {
			denied = append(denied, map[string]string{"name": d.Name, "reason": d.Reason})
		}
		payload["denied_tools"] = denied
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// marshalIDs 序列化审计用的时段 ID，失败时返回空数组而不是让写入失败。
func marshalIDs(ids []string) string {
	if len(ids) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// aiUsageKindQuery 是 ai_usage 表里的预算桶名。
//
// 问答链路的 plan 与 answer 共用这一个桶：一轮对话最多两次模型调用，
// 如果拆成两个桶，用户看到的额度会是实际可问轮数的两倍。
const aiUsageKindQuery = "query"

// agentBudget 把 storage 的 ai_usage 表接到 Agent 的预算接口上。
type agentBudget struct {
	store      *storage.Store
	loc        *time.Location
	dailyLimit int
}

// Exhausted 判断当日预算是否用尽。
//
// plan 与 answer 共用一个上限：一轮对话最多两次调用，若分开计数，
// 用户看到的是"还能问 20 次"但实际只够 10 轮。
func (b *agentBudget) Exhausted(ctx context.Context, _ string) bool {
	if b.store == nil {
		return false
	}
	limit := b.dailyLimit
	if limit <= 0 {
		limit = 20
	}
	loc := b.loc
	if loc == nil {
		loc = time.UTC
	}
	date := time.Now().In(loc).Format("2006-01-02")
	usage, err := b.store.AIUsageToday(ctx, date, aiUsageKindQuery)
	if err != nil {
		// 读不到用量时放行：宁可多花一次调用，也不要让记录查询不可用。
		return false
	}
	return usage.Calls >= limit
}

// Record 记录一次调用的 token 用量。
//
// 未启用的模型客户端会返回空 Response：这种情况不计入调用次数，
// 否则"模型不可用"会被预算统计误判成"已经用过"。
func (b *agentBudget) Record(ctx context.Context, _ string, resp ai.Response) error {
	if b.store == nil || resp.Usage.TotalTokens == 0 {
		return nil
	}
	loc := b.loc
	if loc == nil {
		loc = time.UTC
	}
	date := time.Now().In(loc).Format("2006-01-02")
	return b.store.AddAIUsage(ctx, date, aiUsageKindQuery,
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
}
