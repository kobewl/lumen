package assistant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/contextassembler"
	"lumen/server/internal/identity"
	"lumen/server/internal/storage"
	"lumen/server/internal/temporal"
	"lumen/server/internal/tooling"
)

// 答案的支持等级。
//
// 这是模型必须自己标注、但由代码校验的字段：用户有权知道哪些结论是
// 直接来自记录、哪些是推断、哪些其实没证据。
const (
	SupportSupported    = "supported"
	SupportInferred     = "inferred"
	SupportInsufficient = "insufficient"
	SupportConflicted   = "conflicted"
)

// Turn 是一次完整的对话轮次输入。
//
// Temporal 是装配根（QAService）用注入时钟生成的可信时间快照：
// 一轮一个对象，随后传给 Planner、Synthesizer、事实兜底和所有工具。
// 直接构造 Turn（测试等场景）时可以留空，Agent 会用自己的注入时钟补一个；
// 但生产链路上不允许任何层自行取 time.Now。
//
// Bundle 是本轮的完整上下文（由 contextassembler 装配）。测试可以直接注入
// 预先构造好的 Bundle；留空时 Agent 用自己的装配器现场装配
// （Agent 由此不再直接读 storage 拼上下文）。
type Turn struct {
	UserID   string
	Text     string
	Temporal temporal.Context
	Bundle   *contextassembler.Bundle
}

// Reply 是一次对话轮次的结果。
type Reply struct {
	Text         string
	Mode         string
	SupportLevel string
	// SourceSessionIDs 是本轮回答依据中的"活动记录"类 ID（时段），用于审计。
	SourceSessionIDs []string
	// SourceTaskIDs 是本轮回答依据中的"Agent 报告"类 ID，用于审计。
	// 与 SourceSessionIDs 分开：两者的可信度不同，混在一起就分不清
	// 回答里哪些是 Agent 报告的、哪些是从活动记录推断的。
	SourceTaskIDs []string
	// TurnSource 是"本轮用户消息"的代码生成来源 ID（u_ 前缀）。
	// 用户直接表达的偏好候选以它为依据；飞书路径下它同时是
	// conversations 行的 ID，因此这个来源可回溯核对。
	TurnSource  string
	Status      string
	UsedAI      bool
	ToolCalls   []string
	DeniedTools []tooling.Denial
	// MemoryCandidates 是本轮写入的候选记忆数（只写候选，不生效）。
	MemoryCandidates int
	// Truncations 是上下文装配的截断/降级元信息（可解释性：为什么这一轮
	// 模型看到的上下文缺了哪一段）。只进入调试接口与日志，不进用户可见文本。
	Truncations []string
}

// Planner 是生成计划的模型接口。
//
// 抽成接口是为了测试能注入 fake model：验证"模型说了什么"导致的代码行为，
// 而不是把固定文案当 AI 质量断言。
type Planner interface {
	// Plan 生成一轮计划。
	Plan(ctx context.Context, req PlanRequest) (Plan, ai.Response, error)
	// Synthesize 依据事实生成最终回答。
	Synthesize(ctx context.Context, req SynthesizeRequest) (SynthResult, ai.Response, error)
	// Enabled 表示模型是否可用。
	Enabled() bool
}

// PlanRequest 是调用 Planner 的输入。
type PlanRequest struct {
	SystemPrompt string
	UserPrompt   string
}

// SynthesizeRequest 是调用 Synthesize 的输入。
type SynthesizeRequest struct {
	SystemPrompt string
	UserPrompt   string
}

// SynthResult 是合成阶段解析出来的结果。
type SynthResult struct {
	Answer       string
	SupportLevel string
}

// LLMPlanner 用 ai.Client 实现 Planner。
type LLMPlanner struct {
	Client *ai.Client
}

func (p *LLMPlanner) Enabled() bool { return p.Client != nil && p.Client.Enabled() }

func (p *LLMPlanner) Plan(ctx context.Context, req PlanRequest) (Plan, ai.Response, error) {
	resp, err := p.Client.CompleteJSON(ctx, req.SystemPrompt, req.UserPrompt, 900)
	if err != nil {
		return Plan{}, resp, err
	}
	plan, err := ParsePlan(resp.Content)
	if err != nil {
		return Plan{}, resp, err
	}
	return plan, resp, nil
}

func (p *LLMPlanner) Synthesize(ctx context.Context, req SynthesizeRequest) (SynthResult, ai.Response, error) {
	resp, err := p.Client.CompleteJSON(ctx, req.SystemPrompt, req.UserPrompt, 900)
	if err != nil {
		return SynthResult{}, resp, err
	}
	parsed, err := parseSynthResult(resp.Content)
	if err != nil {
		return SynthResult{}, resp, err
	}
	return parsed, resp, nil
}

// Budget 是模型调用的预算控制接口。
type Budget interface {
	// Exhausted 表示当日预算是否用尽。
	Exhausted(ctx context.Context, kind string) bool
	// Record 记录一次调用的 token 用量。
	Record(ctx context.Context, kind string, resp ai.Response) error
}

// Agent 是编排器：上下文装配 → Planner → 工具执行器（含 Policy Gate）
// → Synthesizer → 校验。
//
// 它是默认产品入口。工具目录、策略、执行顺序与审计都在 tooling 层，
// 因此"模型能做什么"这件事不在这份代码里，而在注册表与工具声明里。
// 可信时间同样不在这份代码里：一轮一个 temporal.Context 从入口传进来。
// 每轮送进模型的上下文也不在这里拼：由 contextassembler 按预算装配，
// Agent 只消费同一个 Bundle（Planner 与 Synthesizer 看到同一份）。
type Agent struct {
	store   *storage.Store
	planner Planner
	// executor 是工具调用的唯一入口（策略 + 执行 + 审计）。
	executor *tooling.Executor
	profile  identity.Profile
	loc      *time.Location
	budget   Budget
	logger   *slog.Logger
	// clock 只在 Turn 没有携带可信时间时兜底（直接构造 Turn 的测试等场景）；
	// 生产链路由 QAService 统一注入，Agent 不直接取系统时间。
	clock temporal.Clock
	// assembler 负责把存储数据装配成 ContextBundle。Turn 已带 Bundle 时跳过。
	// 为 nil 时（且 Turn 也没带）退化为最小 Bundle（只有时间与身份）。
	assembler *contextassembler.Assembler
}

// Options 是构造 Agent 的可选参数。
type Options struct {
	Store    *storage.Store
	Planner  Planner
	Executor *tooling.Executor
	Profile  identity.Profile
	Location *time.Location
	Budget   Budget
	Logger   *slog.Logger
	// Clock 兜底时钟（Turn 未携带可信时间时使用）；留空即系统时钟。
	Clock temporal.Clock
	// Assembler 允许上层注入装配器（自定义预算/替换存储来源）；
	// 留空时用 Store + Profile 构造默认装配器（Store 也为 nil 则不装配存储段）。
	Assembler *contextassembler.Assembler
	// ContextBudget 是默认装配器的预算；仅在 Assembler 为空时生效。
	ContextBudget contextassembler.Budget
}

// NewAgent 创建编排器。
func NewAgent(opts Options) *Agent {
	loc := opts.Location
	if loc == nil {
		loc = time.UTC
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := opts.Clock
	if clock == nil {
		clock = temporal.SystemClock{}
	}
	profile := opts.Profile.Normalize()
	// 装配器：显式注入优先；否则由 Store+Profile 构造默认。
	// Agent 自身不读 storage 拼上下文——这是本包的分层边界。
	assembler := opts.Assembler
	if assembler == nil && opts.Store != nil {
		budget := opts.ContextBudget
		if budget.TotalRunes <= 0 {
			budget = contextassembler.DefaultBudget()
		}
		assembler = contextassembler.NewAssembler(opts.Store, profile, budget, logger)
	}
	return &Agent{
		store:     opts.Store,
		planner:   opts.Planner,
		executor:  opts.Executor,
		profile:   profile,
		loc:       loc,
		budget:    opts.Budget,
		logger:    logger,
		clock:     clock,
		assembler: assembler,
	}
}

// Profile 返回当前生效的身份配置。
func (a *Agent) Profile() identity.Profile { return a.profile }

// ToolCatalog 返回模型看到的工具目录（只读调试接口与提示词共用）。
func (a *Agent) ToolCatalog() string {
	if a.executor == nil {
		return ""
	}
	return a.executor.Catalog()
}

// ToolRegistry 暴露工具注册表，供只读调试接口展示能力边界。
func (a *Agent) ToolRegistry() *tooling.Registry {
	if a.executor == nil {
		return nil
	}
	return a.executor.Registry()
}

// Handle 处理一轮用户消息。
//
// 流程与失败点：
//  1. 模型不可用/预算耗尽 → 确定性兜底（不检索、不猜）；
//  2. Planner 失败或计划非法 → 降级兜底，绝不用"解释模型意图"的方式硬猜；
//  3. 工具执行器拦截部分调用 → 继续执行放行的部分，并告知模型哪些没做成；
//  4. Synthesizer 失败 → 用工具返回的原始事实做确定性摘要；
//  5. 合成结果越界（编造能力、内部术语、超长）→ 同样降级；
//  6. 问候与可信时段明显冲突（"中午说早呀"）→ 带纠正事实重写一次，
//     仍失败则降为中性无时段文本（TemporalGuard）。
func (a *Agent) Handle(ctx context.Context, turn Turn) (Reply, error) {
	text := strings.TrimSpace(turn.Text)
	if text == "" {
		return Reply{
			Text:         fmt.Sprintf("我在，%s。想说点什么？", a.profile.Name),
			Mode:         ModeChat,
			SupportLevel: SupportInsufficient,
			Status:       "empty_input",
		}, nil
	}

	// 一轮一个可信时间快照：入口（QAService）传进来的优先；
	// 没有才用 Agent 自己的注入时钟兜底（不经过任何 time.Now 直调）。
	tc := turn.Temporal
	if !tc.Valid() {
		tc = temporal.Build(a.clock, a.loc)
	}

	// 一轮一份上下文：Planner 与 Synthesizer 消费同一个 Bundle。
	// Turn 已带（QAService/测试注入）就用它；否则用装配器现场装配；
	// 连装配器都没有时退化为最小 Bundle（时间+身份，无存储读取）。
	bundle := turn.Bundle
	if bundle == nil {
		if a.assembler != nil {
			bundle = a.assembler.Assemble(ctx, contextassembler.Request{
				UserID:   turn.UserID,
				Temporal: tc,
				UserText: text,
			})
		} else {
			bundle = contextassembler.NewBundle(tc, a.profile, text)
		}
	}

	if !a.plannerAvailable(ctx) {
		return a.fallbackReply(PlannerUnavailableDisabled), nil
	}

	planReq := PlanRequest{
		SystemPrompt: a.plannerSystemPrompt(),
		UserPrompt:   renderPlannerPrompt(bundle),
	}
	plan, planResp, err := a.planner.Plan(ctx, planReq)
	a.recordUsage(ctx, "plan", planResp)
	if err != nil {
		a.logger.Warn("生成计划失败，走确定性兜底",
			"error", err.Error(), "user_id_mask", maskID(turn.UserID))
		return a.fallbackReply(PlannerUnavailableError), nil
	}

	// 工具执行器是唯一入口：策略校验、执行顺序（先读后写）、证据注入与审计
	// 都在里面完成。模型把写入排在读之前也一样安全。
	// 执行完把结果装回同一个 Bundle（预算裁剪在这里发生），
	// Synthesizer 看到的就是本轮最终上下文。
	results, denied := a.executeTools(ctx, turn.UserID, bundle.Temporal, bundle.TurnSource, plan.ToolCalls)
	bundle.AddToolResults(results, denied)

	// 需要澄清时，模型的问题只需要过安全校验，不需要再走一轮合成：
	// 这时候没有任何数据可依据，再调一次模型只是多花钱。
	if plan.NeedsClarification && strings.TrimSpace(plan.ClarificationQuestion) != "" {
		q := strings.TrimSpace(plan.ClarificationQuestion)
		if validAnswerText(q) {
			a.saveState(ctx, turn.UserID, text, plan, q)
			return Reply{
				Text:         q,
				Mode:         ModeClarify,
				SupportLevel: SupportInsufficient,
				TurnSource:   bundle.TurnSource,
				Status:       "clarify",
				UsedAI:       true,
				ToolCalls:    plan.ToolNames(),
				DeniedTools:  denied,
				Truncations:  bundle.Truncations(),
			}, nil
		}
	}

	synth, synthResp, err := a.synthesize(ctx, bundle, plan, "")
	a.recordUsage(ctx, "answer", synthResp)
	if err != nil {
		a.logger.Warn("合成回答失败，降级为事实摘要", "error", err.Error())
		return a.factsFallback(turn, plan, results, denied, "synth_failed"), nil
	}

	// 答案必须过校验：越界的回复宁可降级，也不能发给用户。
	if !validAnswerText(synth.Answer) {
		a.logger.Warn("合成回答越界，降级为事实摘要")
		return a.factsFallback(turn, plan, results, denied, "answer_invalid"), nil
	}

	// TemporalGuard：只拦"问候与可信时段明显冲突"这一种硬冲突。
	// 冲突时带纠正事实重写一次；第二次失败降为中性无时段文本。
	answer := synth.Answer
	if conflict := detectGreetingConflict(answer, bundle.Temporal); conflict != "" {
		a.logger.Warn("问候与可信时段冲突，带纠正事实重写一次",
			"day_part", bundle.Temporal.DayPart, "clock", bundle.Temporal.ClockText())
		answer = a.rewriteAfterTemporalConflict(ctx, bundle, plan, results, denied, conflict, answer)
	}

	if len(results) > 0 {
		// 有事实依据时统一附证据范围，避免模型漏写或写错。
		//
		// 证据行同时标注数据来源类型（Agent 报告 / 活动记录）：
		// 用户因此能自己判断哪部分结论可信、哪部分只是推断，
		// 不必依赖模型是否如实标注。
		if line := evidenceLine(results, a.loc); line != "" {
			answer = strings.TrimRight(answer, "\n") + "\n\n" + line
		}
	}

	a.saveState(ctx, turn.UserID, text, plan, "")

	sessionIDs, taskIDs := splitEvidence(results)
	return Reply{
		Text:             answer,
		Mode:             plan.Mode,
		SupportLevel:     normalizeSupport(synth.SupportLevel, answer, results),
		SourceSessionIDs: sessionIDs,
		SourceTaskIDs:    taskIDs,
		TurnSource:       bundle.TurnSource,
		Status:           "ok",
		UsedAI:           true,
		ToolCalls:        plan.ToolNames(),
		DeniedTools:      denied,
		MemoryCandidates: countMemoryWrites(results),
		Truncations:      bundle.Truncations(),
	}, nil
}

// rewriteAfterTemporalConflict 带纠正事实重写一次；第二次失败降为中性无时段文本。
//
// 纠正事实是代码生成的可信内容（trusted_context 的纠正要求段），
// 不是另一套关键词话术：语义仍全部由模型完成，代码只认"时段冲突"这一种硬冲突。
func (a *Agent) rewriteAfterTemporalConflict(ctx context.Context, bundle *contextassembler.Bundle,
	plan Plan, results []tooling.Result, denied []tooling.Denial, conflict, original string) string {
	synth, resp, err := a.synthesize(ctx, bundle, plan, conflict)
	a.recordUsage(ctx, "answer", resp)
	if err == nil && validAnswerText(synth.Answer) && detectGreetingConflict(synth.Answer, bundle.Temporal) == "" {
		return synth.Answer
	}
	a.logger.Warn("重写后问候仍冲突或失败，降为中性无时段回复")

	// 中性化：从最近一版模型文本里剔除问候语素。剔除后没有可用内容时，
	// 用一句确定性的中性兜底——这是降级路径（与模型不可用的兜底同一性质），
	// 不是闲聊路由：它不会根据用户说什么选择不同话术。
	base := original
	if err == nil && strings.TrimSpace(synth.Answer) != "" {
		base = synth.Answer
	}
	if neutral := neutralizeGreeting(base); neutral != "" {
		return neutral
	}
	return fmt.Sprintf("我在，%s。想聊点什么？", a.profile.Name)
}

// executeTools 通过统一执行器执行计划里的工具调用。
//
// 这里刻意不做任何"挑选"：模型请求什么就交给执行器处理，
// 该不该执行由策略决定，失败了也照实记下来告知模型。
func (a *Agent) executeTools(ctx context.Context, userID string, tc temporal.Context,
	turnSource string, calls []tooling.Call) ([]tooling.Result, []tooling.Denial) {
	if a.executor == nil {
		if len(calls) == 0 {
			return nil, nil
		}
		// 装配漏了执行器：如实说明这一步没做成，而不是假装模型没请求过。
		denied := make([]tooling.Denial, 0, len(calls))
		for _, c := range calls {
			denied = append(denied, tooling.Denial{Name: c.Name, Reason: "工具执行器未初始化"})
		}
		a.logger.Error("工具执行器未初始化，本轮全部拒绝")
		return nil, denied
	}
	return a.executor.Run(ctx, calls, tooling.RunInfo{
		Actor:      userID,
		Temporal:   tc,
		TurnSource: turnSource,
	})
}

// splitEvidence 把本轮证据按"活动记录"与"Agent 报告"分流，供审计使用。
func splitEvidence(results []tooling.Result) (sessionIDs, taskIDs []string) {
	for _, r := range results {
		switch r.Kind {
		case tooling.KindActivity:
			sessionIDs = appendUnique(sessionIDs, r.Evidence)
		case tooling.KindReportedTasks:
			taskIDs = appendUnique(taskIDs, r.Evidence)
		}
	}
	return sessionIDs, taskIDs
}

// appendUnique 追加去重，保持首次出现的顺序。
func appendUnique(dst []string, items []string) []string {
	for _, item := range items {
		if item == "" {
			continue
		}
		found := false
		for _, existing := range dst {
			if existing == item {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, item)
		}
	}
	return dst
}

// countMemoryWrites 统计本轮成功写入的候选记忆数。
func countMemoryWrites(results []tooling.Result) int {
	n := 0
	for _, r := range results {
		if r.Kind == tooling.KindMemoryWrite && r.Count > 0 {
			n += r.Count
		}
	}
	return n
}

// synthesize 调用模型把事实组织成回答。
//
// correction 非空时是 TemporalGuard 的纠正事实（代码生成的可信内容），
// 会进入 trusted_context 的纠正要求段。提示词由同一个 Bundle 渲染：
// trusted 部分与 Planner 看到的逐字相同，证据部分是工具执行后的最终状态。
func (a *Agent) synthesize(ctx context.Context, bundle *contextassembler.Bundle,
	plan Plan, correction string) (SynthResult, ai.Response, error) {
	req := SynthesizeRequest{
		SystemPrompt: synthesizerPromptTemplate,
		UserPrompt:   renderSynthPrompt(bundle, plan, correction),
	}
	return a.planner.Synthesize(ctx, req)
}

// conclusionEvidence 判断本轮是否取到了"结论证据"。
//
// 审查修复：系统信息（当前时间/身份/对话状态）、参考信息（项目清单）、
// 候选写入都**不是**结论证据。只有活动记录与 Agent 报告才能支撑
// "直接来自事实"的支持等级。
func conclusionEvidence(results []tooling.Result) bool {
	for _, r := range results {
		if (r.Kind == tooling.KindActivity || r.Kind == tooling.KindReportedTasks) && r.Count > 0 {
			return true
		}
	}
	return false
}

// factsFallback 在合成阶段失败时，用工具返回的事实拼一段诚实的摘要。
//
// 这不是"另一套关键词机器人"：它不做意图判断，只是把已经取到的结构化事实
// 原样陈述出来，并明确说明模型这轮不可用。
//
// 支持等级只看**结论证据**（活动记录 / Agent 报告）：因为存在系统信息或
// 候选写入就标 supported，会让"现在几点"这类回答也披上"有依据"的外衣。
func (a *Agent) factsFallback(_ Turn, plan Plan,
	results []tooling.Result, denied []tooling.Denial, status string) Reply {
	body := renderFacts(results)
	sessionIDs, taskIDs := splitEvidence(results)
	support := SupportInsufficient
	if conclusionEvidence(results) {
		support = SupportSupported
	}
	if body == "" {
		return Reply{
			Text: fmt.Sprintf("我是 %s。我这轮没能组织好回答，也没取到可用的记录。稍后再试试。", a.profile.Name),
			Mode: plan.Mode, SupportLevel: support,
			Status: status, DeniedTools: denied,
		}
	}
	if line := evidenceLine(results, a.loc); line != "" {
		body += "\n\n" + line
	}
	return Reply{
		Text:             body,
		Mode:             plan.Mode,
		SupportLevel:     support,
		SourceSessionIDs: sessionIDs,
		SourceTaskIDs:    taskIDs,
		Status:           status,
		ToolCalls:        plan.ToolNames(),
		DeniedTools:      denied,
		MemoryCandidates: countMemoryWrites(results),
	}
}

// fallbackReply 是模型整体不可用时的兜底。
func (a *Agent) fallbackReply(reason PlannerUnavailable) Reply {
	return Reply{
		Text:         fallbackAnswer(a.profile, reason),
		Mode:         ModeChat,
		SupportLevel: SupportInsufficient,
		Status:       "fallback_no_model",
	}
}

// plannerAvailable 判断模型与预算是否允许走 AI 路径。
func (a *Agent) plannerAvailable(ctx context.Context) bool {
	if a.planner == nil || !a.planner.Enabled() {
		return false
	}
	if a.budget != nil && a.budget.Exhausted(ctx, "plan") {
		return false
	}
	return true
}

func (a *Agent) recordUsage(ctx context.Context, kind string, resp ai.Response) {
	if a.budget == nil {
		return
	}
	if err := a.budget.Record(ctx, kind, resp); err != nil {
		// 用量记录失败不影响回答。
		_ = err
	}
}

// plannerSystemPrompt 组装 Planner 提示词；工具目录由注册表渲染，
// 身份与已确认信息在 trusted_context 里（Bundle 渲染），系统提示只保留规则。
func (a *Agent) plannerSystemPrompt() string {
	return fmt.Sprintf(plannerPromptTemplate, a.ToolCatalog())
}

// saveState 保存本轮结束后的对话状态（轮后记账，不是上下文装配：
// 下一轮的装配器会把它作为会话摘要读出去）。
func (a *Agent) saveState(ctx context.Context, userID, userText string, plan Plan, pending string) {
	if a.store == nil || userID == "" {
		return
	}
	state := storage.ConversationState{
		UserID:          userID,
		CurrentProject:  extractProject(plan),
		PendingQuestion: pending,
		LastMode:        plan.Mode,
		LastTimeRange:   plan.Understanding.TimeRange,
	}
	if err := a.store.SaveConversationState(ctx, state); err != nil {
		a.logger.Warn("保存对话状态失败", "error", err.Error())
	}
}

// extractProject 从计划实体里挑出最可能是项目名的一个，作为后续轮次的指代目标。
func extractProject(plan Plan) string {
	for _, e := range plan.Understanding.Entities {
		e = strings.TrimSpace(e)
		if e == "" || e == "unclassified" {
			continue
		}
		return truncateRunes(e, 64)
	}
	return ""
}

// completionClaimPhrases 是"声称完成了某事"的措辞。
//
// 只用于一个精确判断：模型是否在**宣称结论**。这不是意图识别——
// 我们不据它决定回答什么，只在它出现时收紧支持等级。
var completionClaimPhrases = []string{
	"完成", "做完", "搞定", "实现好", "已实现", "收尾",
}

// normalizeSupport 校验模型标注的支持等级；非法值按有无事实回退。
//
// 这里有一条**代码级的收紧**，不只依赖模型自觉：
// 如果模型在回答里宣称"完成了某事"，而本轮只取到活动记录（应用名与时长、
// git 提交信息）、没有任何 Agent 报告的结论，那么标 supported 一定是过度自信——
// "用了 ZCode 90 分钟"推不出"完成了某功能"。这种情况强制降为 inferred。
//
// 只在同时满足"有完成表述"且"没有 Agent 报告"时收紧，而不是一律降级：
// 单纯陈述"用了某应用多久"确实直接来自事实，标 supported 是诚实的，
// 无差别降级会让用户对正确的标注也失去信任。
//
// 之所以要放在代码里：模型很自然会把自己组织的措辞标成 supported，
// 而这个标注正是用户判断可信度的依据，不能由被评估方自己决定。
func normalizeSupport(level, answer string, results []tooling.Result) string {
	// 注意：这里的 hasFacts 沿用"存在任何工具结果"的旧语义——
	// 它只参与"完成宣称"的收紧判断；"结论证据"的划分（系统信息/参考信息
	// 不是证据）只作用于 factsFallback，见 conclusionEvidence。
	hasFacts := len(results) > 0

	switch level {
	case SupportSupported, SupportInferred, SupportInsufficient, SupportConflicted:
		if level == SupportSupported && hasFacts && !hasReportedFacts(results) &&
			claimsCompletion(answer) {
			return SupportInferred
		}
		return level
	default:
		if hasFacts {
			return SupportInferred
		}
		return SupportInsufficient
	}
}

// claimsCompletion 判断回答里是否出现了"完成某事"的表述。
func claimsCompletion(answer string) bool {
	for _, phrase := range completionClaimPhrases {
		if strings.Contains(answer, phrase) {
			return true
		}
	}
	return false
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ErrNotConfigured 表示编排器缺少必要依赖。
var ErrNotConfigured = errors.New("agent 未正确配置")

// Validate 检查构造是否可用，供启动期快速失败。
func (a *Agent) Validate() error {
	if a.planner == nil {
		return fmt.Errorf("%w: 缺少 planner", ErrNotConfigured)
	}
	if a.executor == nil {
		return fmt.Errorf("%w: 缺少工具执行器", ErrNotConfigured)
	}
	return nil
}

func maskID(id string) string {
	if len(id) <= 6 {
		return "***"
	}
	return id[:3] + "***" + id[len(id)-3:]
}
