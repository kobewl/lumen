package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/assistant"
	"lumen/server/internal/contextassembler"
	"lumen/server/internal/identity"
	"lumen/server/internal/storage"
	"lumen/server/internal/temporal"
	"lumen/server/internal/tooling"
	"lumen/server/internal/tools"
	"lumen/server/internal/ulid"
)

// QAService 是飞书问答与只读调试接口共用的入口。
//
// 它是 assistant.Agent 的薄适配层：语义判断全部交给 Agent
// （模型出计划 → 工具执行器（策略 + 审计）→ 合成回答），
// 这里只负责"把飞书的输入转成一次轮次、把结果记进 conversations 表"，
// 以及**显式装配**工具层的依赖。
//
// 保留这个类型名是因为 bot.go 与 api/server.go 都以它为依赖点，
// 改名会扩散到不影响功能的调用方；语义上它已经是 Orchestrator 的封装。
type QAService struct {
	agent *assistant.Agent
	store *storage.Store
	loc   *time.Location
	// clock 是装配根注入的可信时间来源；Handle 每轮用它生成唯一快照。
	clock temporal.Clock
	// fallback 只在模型完全不可用时兜底，不是默认入口。
	// 它同样走工具执行器，因此照样过策略、照样留审计。
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
	Profile identity.Profile
	// Clock 是可信时间的来源：每轮 Handle 用它生成**一个** TemporalContext，
	// 传给 Planner、Synthesizer、事实兜底与所有工具。留空即系统时钟；
	// 测试注入 FixedClock 可以让"13:19 说早上好"这类时段问题可复现。
	Clock temporal.Clock
	// QueryDailyLimit 是问答每天可调用模型的次数上限（plan 与 answer 各计一次）。
	QueryDailyLimit int
	// SummaryHour / SummaryMinute 由 Profile 的主动性描述引用，用于提示词里
	// 说明助手什么时候会主动说话。
	SummaryHour   int
	SummaryMinute int
	Logger        *slog.Logger
	// Tools 允许测试或上层替换工具集合；留空则装配默认的第一批工具。
	Tools []tooling.Tool
	// Audit 是工具调用审计的写入点；留空表示不审计（生产装配应显式传入存储实现）。
	Audit tooling.AuditSink
	// Executor 允许上层注入**已装配好**的执行器（生产装配用：主动关怀需要
	// 与问答共用同一个执行器，事实才会走同一套策略与审计）。
	// 留空则按 Tools/Audit 在内部装配（测试与旧行为）。
	Executor *tooling.Executor
	// ContextBudget 是上下文装配的预算；零值用 contextassembler.DefaultBudget。
	ContextBudget contextassembler.Budget
}

// NewQAService 创建问答服务。
//
// 装配顺序刻意保持"显式"：工具 → 执行器（策略 + 审计）→ 编排器。
// 每一层都只依赖下一层的接口，因此换工具、换策略、换审计写入点都不需要改这里。
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

	executor := opts.Executor
	if executor == nil {
		toolSet := opts.Tools
		if len(toolSet) == 0 {
			toolSet = tools.All(tools.Options{
				Store:    opts.Store,
				Profile:  profile,
				Location: loc,
			})
		}
		registry, err := tooling.NewRegistry(toolSet...)
		if err != nil {
			// 装配错误必须尽早暴露：工具声明不合法说明代码有问题，
			// 静默降级成一个"什么工具都没有"的助手会让问题被埋掉。
			// 这里用 panic 是因为构造函数没有 error 返回（调用方紧接着会 Validate）。
			panic("工具注册表构造失败: " + err.Error())
		}
		var err2 error
		executor, err2 = tooling.NewExecutor(tooling.ExecutorOptions{
			Registry: registry,
			Audit:    opts.Audit,
			Logger:   logger,
		})
		if err2 != nil {
			panic("工具执行器构造失败: " + err2.Error())
		}
	}

	planner := opts.Planner
	if planner == nil {
		planner = &assistant.LLMPlanner{Client: opts.Client}
	}

	agent := assistant.NewAgent(assistant.Options{
		Store:         opts.Store,
		Planner:       planner,
		Executor:      executor,
		Profile:       profile,
		Location:      loc,
		Budget:        &agentBudget{store: opts.Store, loc: loc, dailyLimit: opts.QueryDailyLimit},
		Logger:        logger,
		ContextBudget: opts.ContextBudget,
	})

	clock := opts.Clock
	if clock == nil {
		clock = temporal.SystemClock{}
	}
	return &QAService{
		agent:    agent,
		store:    opts.Store,
		loc:      loc,
		clock:    clock,
		fallback: &assistant.MinimalFallback{Executor: executor, Loc: loc},
		logger:   logger,
	}
}

// Agent 暴露编排器，供调试接口与测试直接使用。
func (s *QAService) Agent() *assistant.Agent { return s.agent }

// Profile 返回当前生效的身份配置。
func (s *QAService) Profile() identity.Profile { return s.agent.Profile() }

// ToolCatalog 返回工具目录（给只读调试接口看"模型能选什么"）。
func (s *QAService) ToolCatalog() string { return s.agent.ToolCatalog() }

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
	// 一轮一个可信时间快照：它是本轮唯一的时间事实，
	// 由这里生成后传给 Agent（进而到 Planner/Synthesizer/工具）与兜底。
	tc := temporal.Build(s.clock, s.loc)

	reply, err := s.agent.Handle(ctx, assistant.Turn{UserID: userID, Text: text, Temporal: tc})
	if err != nil {
		return assistant.Reply{}, err
	}

	// 模型不可用时，只对最明确的数据请求做确定性兜底（见 MinimalFallback 注释）。
	// 兜底拿同一份快照，不自己取时间。
	if reply.Status == "fallback_no_model" && s.fallback.CanHandle(text) {
		if body, ferr := s.fallback.Handle(ctx, tc); ferr == nil && body != "" {
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
	// 候选记忆里的"用户直接表达"来源就是 reply.TurnSource；用它作 conversations
	// 行 ID，让这个来源**可回溯**——拿来源 ID 能在 conversations 表里找到原话。
	id := reply.TurnSource
	if id == "" {
		id = ulid.New()
	}
	return s.store.RecordConversation(ctx, id, messageID, userID, reply.Mode,
		queryJSON, reply.Text, sourceIDs, status)
}

// AlreadyHandled 判断消息是否已处理过，保证飞书重复投递不重复处理。
func (s *QAService) AlreadyHandled(ctx context.Context, messageID string) (bool, error) {
	return s.store.ConversationExists(ctx, messageID)
}

// marshalQuery 把本轮的结构信息压成 conversations.query_json。
//
// 只记用户原话与模式、工具名，不记模型提示词与工具返回内容：
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
		payload["denied_tools"] = reply.DeniedTools
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// marshalIDs 序列化审计用的来源 ID，失败时返回空数组而不是让写入失败。
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
