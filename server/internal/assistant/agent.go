package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/storage"
	"lumen/server/internal/ulid"
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
type Turn struct {
	UserID string
	Text   string
}

// Reply 是一次对话轮次的结果。
type Reply struct {
	Text             string
	Mode             string
	SupportLevel     string
	SourceSessionIDs []string
	// SourceTaskIDs 是本轮回答依据的任务摘要 ID，用于审计。
	// 与 SourceSessionIDs 分开：两者的可信度不同，混在一起就分不清
	// 回答里哪些是 Agent 报告的、哪些是从活动记录推断的。
	SourceTaskIDs []string
	Status        string
	UsedAI        bool
	ToolCalls     []string
	DeniedTools   []DeniedToolCalls
	// MemoryCandidates 是本轮写入的候选记忆数（只写候选，不生效）。
	MemoryCandidates int
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

// Agent 是编排器：Planner → Policy Gate → Capability → Synthesizer → 校验。
//
// 它是默认产品入口。旧的正则 ParseQuery 只作为模型不可用时的最小安全兜底
// （见 fallback.go），不再决定语义。
type Agent struct {
	store    *storage.Store
	planner  Planner
	gate     *PolicyGate
	registry *CapabilityRegistry
	profile  Profile
	loc      *time.Location
	budget   Budget
	logger   *slog.Logger

	// maxMemoryCandidates 限制单轮写入的候选记忆数。
	maxMemoryCandidates int
}

// Options 是构造 Agent 的可选参数。
type Options struct {
	Store    *storage.Store
	Planner  Planner
	Registry *CapabilityRegistry
	Profile  Profile
	Location *time.Location
	Budget   Budget
	Logger   *slog.Logger
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
	return &Agent{
		store:               opts.Store,
		planner:             opts.Planner,
		registry:            opts.Registry,
		gate:                NewPolicyGate(opts.Registry),
		profile:             opts.Profile.Normalize(),
		loc:                 loc,
		budget:              opts.Budget,
		logger:              logger,
		maxMemoryCandidates: 3,
	}
}

// Profile 返回当前生效的身份配置。
func (a *Agent) Profile() Profile { return a.profile }

// Handle 处理一轮用户消息。
//
// 流程与失败点：
//  1. 模型不可用/预算耗尽 → 确定性兜底（不检索、不猜）；
//  2. Planner 失败或计划非法 → 降级兜底，绝不用"解释模型意图"的方式硬猜；
//  3. Policy Gate 拦截部分工具 → 继续执行放行的部分，并告知模型哪些没做成；
//  4. Synthesizer 失败 → 用能力返回的原始事实做确定性摘要；
//  5. 合成结果越界（编造能力、内部术语、超长）→ 同样降级。
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

	if !a.plannerAvailable(ctx) {
		return a.fallbackReply(PlannerUnavailableDisabled), nil
	}

	state, err := a.loadState(ctx, turn.UserID)
	if err != nil {
		a.logger.Warn("读取对话状态失败，按无状态处理", "error", err.Error())
	}
	planReq := PlanRequest{
		SystemPrompt: a.plannerSystemPrompt(),
		UserPrompt: plannerUserPrompt(text,
			time.Now().In(a.loc).Format("2006-01-02 15:04 (Monday)"), state),
	}
	plan, planResp, err := a.planner.Plan(ctx, planReq)
	a.recordUsage(ctx, "plan", planResp)
	if err != nil {
		a.logger.Warn("生成计划失败，走确定性兜底",
			"error", err.Error(), "user_id_mask", maskID(turn.UserID))
		return a.fallbackReply(PlannerUnavailableError), nil
	}

	allowed, denied := a.gate.Review(plan)

	results := make([]CapabilityResult, 0, len(allowed))
	for _, call := range allowed {
		capability, ok := a.registry.Get(call.Name)
		if !ok {
			// Gate 已保证存在；这里只是防御性处理。
			continue
		}
		out, err := capability.Run(ctx, call.Arguments)
		if err != nil {
			a.logger.Warn("能力执行失败", "capability", call.Name, "error", err.Error())
			denied = append(denied, DeniedToolCalls{Name: call.Name, Reason: "执行失败"})
			continue
		}
		results = append(results, CapabilityResult{Name: call.Name, Value: out})
	}

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
				Status:       "clarify",
				UsedAI:       true,
				ToolCalls:    plan.ToolNames(),
				DeniedTools:  denied,
			}, nil
		}
	}

	synth, synthResp, err := a.synthesize(ctx, text, plan, results, denied)
	a.recordUsage(ctx, "answer", synthResp)
	if err != nil {
		a.logger.Warn("合成回答失败，降级为事实摘要", "error", err.Error())
		return a.factsFallback(ctx, turn, plan, results, denied, "synth_failed")
	}

	// 答案必须过校验：越界的回复宁可降级，也不能发给用户。
	if !validAnswerText(synth.Answer) {
		a.logger.Warn("合成回答越界，降级为事实摘要")
		return a.factsFallback(ctx, turn, plan, results, denied, "answer_invalid")
	}

	sourceIDs := collectSessionIDs(results)
	if len(results) > 0 {
		// 有事实依据时统一附证据范围，避免模型漏写或写错。
		//
		// 证据行同时标注数据来源类型（Agent 报告 / 活动记录）：
		// 用户因此能自己判断哪部分结论可信、哪部分只是推断，
		// 不必依赖模型是否如实标注。
		synth.Answer = strings.TrimRight(synth.Answer, "\n") + "\n\n" + evidenceLine(results)
	}

	candidates, err := a.storeMemoryCandidates(ctx, turn.UserID, plan, results)
	if err != nil {
		a.logger.Warn("写入记忆候选失败", "error", err.Error())
	}
	a.saveState(ctx, turn.UserID, text, plan, "")

	return Reply{
		Text:             synth.Answer,
		Mode:             plan.Mode,
		SupportLevel:     normalizeSupport(synth.SupportLevel, synth.Answer, results),
		SourceSessionIDs: sourceIDs,
		SourceTaskIDs:    collectTaskIDs(results),
		Status:           "ok",
		UsedAI:           true,
		ToolCalls:        plan.ToolNames(),
		DeniedTools:      denied,
		MemoryCandidates: candidates,
	}, nil
}

// synthesize 调用模型把事实组织成回答。
func (a *Agent) synthesize(ctx context.Context, userText string, plan Plan,
	results []CapabilityResult, denied []DeniedToolCalls) (SynthResult, ai.Response, error) {
	req := SynthesizeRequest{
		SystemPrompt: fmt.Sprintf(synthesizerPromptTemplate,
			a.profile.Name, a.profile.Role, a.profile.PromptBlock()),
		UserPrompt: synthesizerUserPrompt(userText, plan, results, denied),
	}
	return a.planner.Synthesize(ctx, req)
}

// factsFallback 在合成阶段失败时，用能力返回的事实拼一段诚实的摘要。
//
// 这不是"另一套关键词机器人"：它不做意图判断，只是把已经取到的结构化事实
// 原样陈述出来，并明确说明模型这轮不可用。
func (a *Agent) factsFallback(_ context.Context, turn Turn, plan Plan,
	results []CapabilityResult, denied []DeniedToolCalls, status string) (Reply, error) {
	body := renderFacts(results, a.loc)
	if body == "" {
		return Reply{
			Text: fmt.Sprintf("我是 %s。我这轮没能组织好回答，也没取到可用的记录。稍后再试试。", a.profile.Name),
			Mode: plan.Mode, SupportLevel: SupportInsufficient,
			Status: status, DeniedTools: denied,
		}, nil
	}
	text := body
	if len(results) > 0 {
		text += "\n\n" + evidenceLine(results)
	}
	return Reply{
		Text:             text,
		Mode:             plan.Mode,
		SupportLevel:     SupportSupported,
		SourceSessionIDs: collectSessionIDs(results),
		Status:           status,
		ToolCalls:        plan.ToolNames(),
		DeniedTools:      denied,
	}, nil
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

// plannerSystemPrompt 组装 Planner 提示词，身份部分来自 Profile 而不是硬编码。
func (a *Agent) plannerSystemPrompt() string {
	return fmt.Sprintf(plannerPromptTemplate,
		a.profile.Name, a.profile.Role, a.profile.PromptBlock(), a.registry.Catalog())
}

// storeMemoryCandidates 把计划里的候选记忆落库。
//
// 只写候选状态：模型提议的内容不会自动成为"已确认记忆"，
// 因此也不会进入后续轮次的事实上下文。
//
// 来源必须在**本轮能力返回**里能核实，否则丢弃——包括整条丢弃。
// 模型看不到数据库，它写出的 ID 只是字符串；一条标着不存在出处的记忆
// 比没有出处的记忆更坏，因为它看起来是可核对的。
func (a *Agent) storeMemoryCandidates(ctx context.Context, userID string, plan Plan,
	results []CapabilityResult) (int, error) {
	if a.store == nil || len(plan.MemoryCandidates) == 0 {
		return 0, nil
	}
	verified := evidenceIDSet(results)
	saved := 0
	for _, mc := range plan.MemoryCandidates {
		if saved >= a.maxMemoryCandidates {
			break
		}
		content := strings.TrimSpace(mc.Content)
		if content == "" || !validMemoryContent(content) {
			continue
		}
		sourceIDs, dropped, ok := filterSourceIDs(mc.SourceIDs, verified)
		if !ok {
			// 来源全是编的：整条丢弃。留一条"看起来有出处"的候选记忆
			// 比不留更危险，用户将来核对时会发现出处根本不存在。
			a.logger.Warn("候选记忆的来源无法核实，整条丢弃",
				"dropped_source_ids", dropped, "user_id_mask", maskID(userID))
			continue
		}
		if dropped > 0 {
			a.logger.Warn("候选记忆含无法核实的来源，已剔除",
				"dropped_source_ids", dropped, "user_id_mask", maskID(userID))
		}
		candidate := storage.MemoryCandidate{
			ID:         ulid.New(),
			UserID:     userID,
			Kind:       normalizeMemoryKind(mc.Kind),
			Content:    truncateRunes(content, 200),
			SourceIDs:  sourceIDs,
			Confidence: clamp01(mc.Confidence),
			Status:     storage.MemoryStatusCandidate,
			CreatedAt:  time.Now().UTC(),
		}
		if err := a.store.SaveMemoryCandidate(ctx, candidate); err != nil {
			return saved, err
		}
		saved++
	}
	return saved, nil
}

// loadState 读取有限对话状态。
func (a *Agent) loadState(ctx context.Context, userID string) (StateView, error) {
	if a.store == nil || userID == "" {
		return StateView{}, nil
	}
	stored, err := a.store.ConversationStateByUser(ctx, userID)
	if err != nil {
		return StateView{}, err
	}
	return stateView(stored), nil
}

// saveState 保存本轮结束后的对话状态。
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

// collectSessionIDs / collectTaskIDs / evidenceLine 等"依据处理"都在
// evidence.go：回答引用了什么、候选记忆的来源是否可核实、证据行怎么写。

// hasReportedFacts 判断本轮是否取到了"Agent 报告的结论"。
//
// 这是区分 supported 与 inferred 的唯一依据：只有任务摘要是结论本身，
// 应用名与时长、git 提交信息都只是活动痕迹，从它们推出的"完成了什么"
// 始终是推断。
func hasReportedFacts(results []CapabilityResult) bool {
	for _, r := range results {
		if v, ok := r.Value.(TaskSummariesResult); ok && len(v.TaskSummaries) > 0 {
			return true
		}
	}
	return false
}

// CapabilityResult 是一次能力调用的结果。
type CapabilityResult struct {
	Name  string
	Value any
}

// JSON 把结果序列化为给模型看的事实文本。
func (r CapabilityResult) JSON() string {
	b, err := json.Marshal(r.Value)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// completionClaimPhrases 是"声称完成了某事"的措辞。
//
// 只用于一个精确判断：模型是否在**宣称结论**。这不是意图识别——
// 我们不据它决定回答什么，只在它出现时收紧支持等级。
var completionClaimPhrases = []string{
	"完成", "做完", "搞定", "实现好", "已实现", "收尾", "搞定",
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
func normalizeSupport(level, answer string, results []CapabilityResult) string {
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

func normalizeMemoryKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "preference":
		return "preference"
	case "project":
		return "project"
	case "fact":
		return "fact"
	default:
		return "fact"
	}
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// ErrNotConfigured 表示编排器缺少必要依赖。
var ErrNotConfigured = errors.New("agent 未正确配置")

// Validate 检查构造是否可用，供启动期快速失败。
func (a *Agent) Validate() error {
	if a.planner == nil {
		return fmt.Errorf("%w: 缺少 planner", ErrNotConfigured)
	}
	if a.registry == nil {
		return fmt.Errorf("%w: 缺少能力注册表", ErrNotConfigured)
	}
	return nil
}

func maskID(id string) string {
	if len(id) <= 6 {
		return "***"
	}
	return id[:3] + "***" + id[len(id)-3:]
}
