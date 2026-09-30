package initiative

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/contextassembler"
	"lumen/server/internal/identity"
	"lumen/server/internal/notification"
	"lumen/server/internal/storage"
	"lumen/server/internal/temporal"
	"lumen/server/internal/tooling"
	"lumen/server/internal/ulid"
)

// Outcome 是一次触发的结果（进日志、管理端点响应与测试断言）。
type Outcome struct {
	// Action: disabled / skipped / dry_run / sent / draft / failed / rejected
	Action string
	Reason string
	// OutboxIDs 是本次写入的出站记录 ID（审计入口）。
	OutboxIDs []string
	// Text 是生成的问题全文（dry-run 验收直接看它）。
	Text string
}

// Options 是构造服务的依赖（全部显式注入）。
type Options struct {
	Store    *storage.Store
	Executor *tooling.Executor
	Client   *ai.Client
	Profile  identity.Profile
	Loc      *time.Location
	Clock    temporal.Clock
	// Planner 允许测试注入假模型；留空则用 Client 构造 ModelPlanner。
	Planner Planner
	// Sender 是投递渠道（feishu.Messenger 实现 notification.Sender）。
	// nil 或未启用时草稿保留为 draft，不会标记为已发送。
	Sender notification.Sender
	// Targets 是投递目标（生产装配传飞书白名单用户）。
	Targets []string
	// DryRun 为 true 时不真发（默认）。
	DryRun bool
	// Policy 留空用 DefaultPolicy（再由调用方打开 Enabled）。
	Policy Policy
	Logger *slog.Logger
}

// Service 是主动关怀的编排器。
type Service struct {
	store    *storage.Store
	executor *tooling.Executor
	planner  Planner
	policy   Policy
	sender   notification.Sender
	targets  []string
	dryRun   bool
	loc      *time.Location
	clock    temporal.Clock
	logger   *slog.Logger

	// mu 串行化触发：调度器与管理端点可能并发调用，
	// 频率检查与写入必须是原子的，否则"每天最多 2 次"会被竞态打破。
	mu sync.Mutex
}

// New 创建服务。
func New(opts Options) *Service {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	policy := opts.Policy
	if policy.MaxPerDay <= 0 && policy.MinInterval <= 0 && policy.QuietStartMinutes == 0 && policy.QuietEndMinutes == 0 {
		policy = DefaultPolicy()
	}
	planner := opts.Planner
	if planner == nil {
		planner = &ModelPlanner{Client: opts.Client, Profile: opts.Profile}
	}
	loc := opts.Loc
	if loc == nil {
		loc = time.UTC
	}
	clock := opts.Clock
	if clock == nil {
		clock = temporal.SystemClock{}
	}
	targets := make([]string, 0, len(opts.Targets))
	for _, t := range opts.Targets {
		if t = strings.TrimSpace(t); t != "" {
			targets = append(targets, t)
		}
	}
	return &Service{
		store: opts.Store, executor: opts.Executor, planner: planner,
		policy: policy, sender: opts.Sender, targets: targets,
		dryRun: opts.DryRun, loc: loc, clock: clock, logger: logger,
	}
}

// RunOnce 执行一次"要不要主动说一句话"的完整判定与投递。
//
// force 仅供本地验收跳过频率限制（见 Policy.Decide）；
// 全程不区分"测试消息"与"真实消息"的口径差异——测试一律走 dry-run 或
// fake messenger，不向真实用户发送。
func (s *Service) RunOnce(ctx context.Context, force bool) (Outcome, error) {
	if !s.policy.Enabled {
		return Outcome{Action: "disabled", Reason: "主动关怀未启用"}, nil
	}
	tc := temporal.Build(s.clock, s.loc)

	// 串行化：频率检查与写入必须是原子的。
	s.mu.Lock()
	defer s.mu.Unlock()

	// ① 策略：逐目标判定（计数与间隔都按用户隔离）。
	var allowed []string
	var denyReason string
	for _, target := range s.targets {
		sent, err := s.store.CountInitiativeDeliveredToday(ctx, target, tc.Date)
		if err != nil {
			return Outcome{}, err
		}
		last, haveLast, err := s.store.LastInitiativeAttemptAt(ctx, target)
		if err != nil {
			return Outcome{}, err
		}
		d := s.policy.Decide(tc, sent, last, haveLast, force)
		if d.Allowed {
			allowed = append(allowed, target)
		} else if denyReason == "" {
			denyReason = d.Reason
		}
	}
	if len(allowed) == 0 {
		// 策略层面的跳过不写 outbox 行：静默时段每 30 分钟 tick 一次，
		// 写行只会把账本灌满噪声。原因进日志与端点响应。
		s.logger.Info("主动关怀跳过", "reason", denyReason, "clock", tc.ClockText())
		return Outcome{Action: "skipped", Reason: denyReason}, nil
	}

	// ② 经工具执行器取已核实事实：与问答同一套策略、审计与数据最小化。
	results, denied := s.executor.Run(ctx, []tooling.Call{
		{Name: "get_today_status", Arguments: map[string]any{}},
		{Name: "get_task_summaries", Arguments: map[string]any{"date": tc.Date}},
	}, tooling.RunInfo{Actor: "initiative", Temporal: tc})
	if len(denied) > 0 {
		return Outcome{Action: "skipped", Reason: "取事实被拒绝: " + denied[0].Reason}, nil
	}

	// 没有结论证据就不打扰：没有事实依据的关心就是没话找话。
	var evidenceIDs, taskIDs []string
	hasConclusion := false
	for _, r := range results {
		evidenceIDs = appendUnique(evidenceIDs, r.Evidence)
		switch r.Kind {
		case tooling.KindActivity, tooling.KindReportedTasks:
			if r.Count > 0 {
				hasConclusion = true
			}
			if r.Kind == tooling.KindReportedTasks {
				taskIDs = appendUnique(taskIDs, r.Evidence)
			}
		}
	}
	if !hasConclusion {
		s.logger.Info("主动关怀跳过：没有可依据的记录", "clock", tc.ClockText())
		return Outcome{Action: "skipped", Reason: "没有可依据的记录"}, nil
	}

	// ③ 模型提案（0 或 1 条）。失败与跳过都落 rejected 行：
	// 它消耗了一次模型调用，间隔限制必须把这类尝试也计进去，
	// 否则调度器会以 tick 频率反复烧钱。
	proposal, err := s.propose(ctx, tc, allowed[0], results, evidenceIDs)
	if err != nil {
		s.record(ctx, allowed, storage.InitiativeOutbox{
			Status: storage.InitiativeStatusRejected, SkipReason: "提案失败: " + err.Error(),
		}, tc, evidenceIDs)
		return Outcome{Action: "rejected", Reason: "提案失败: " + err.Error()}, nil
	}
	if proposal.Skip {
		reason := proposal.Reason
		if reason == "" {
			reason = "模型判断此刻不打扰"
		}
		s.record(ctx, allowed, storage.InitiativeOutbox{
			Status: storage.InitiativeStatusRejected, SkipReason: reason,
		}, tc, evidenceIDs)
		return Outcome{Action: "rejected", Reason: reason}, nil
	}

	// ④ 代码校验提案：证据、内容边界。不过关就不发。
	// 本轮实际运行过的工具名（模型引用事实来源的合法写法之一）。
	toolNames := make([]string, 0, len(results))
	for _, r := range results {
		toolNames = append(toolNames, r.Tool)
	}
	if reason := validateProposal(proposal, evidenceIDs, taskIDs, toolNames); reason != "" {
		s.record(ctx, allowed, storage.InitiativeOutbox{
			Status: storage.InitiativeStatusRejected, SkipReason: reason, Text: proposal.Question,
		}, tc, evidenceIDs)
		return Outcome{Action: "rejected", Reason: reason}, nil
	}

	// ⑤ 出站：先写草稿，再按 dry-run/真实发送落状态。
	ids := make([]string, 0, len(allowed))
	for _, target := range allowed {
		rec := storage.InitiativeOutbox{
			ID: ulid.New(), UserID: target, LocalDate: tc.Date,
			CreatedAt: time.Now().UTC(), Status: storage.InitiativeStatusDraft,
			Text: proposal.Question, Basis: proposal.Basis, Evidence: evidenceIDs,
		}
		if err := s.store.SaveInitiativeOutbox(ctx, rec); err != nil {
			return Outcome{}, err
		}
		ids = append(ids, rec.ID)

		if s.dryRun {
			rec.Status = storage.InitiativeStatusDryRun
			rec.SkipReason = "dry-run：不真实发送"
			if err := s.store.SaveInitiativeOutbox(ctx, rec); err != nil {
				return Outcome{}, err
			}
			continue
		}
		if s.sender == nil || !s.sender.Enabled() {
			// 渠道不可用：草稿保留，不算已发送。
			rec.SkipReason = "没有可用的投递渠道"
			if err := s.store.SaveInitiativeOutbox(ctx, rec); err != nil {
				return Outcome{}, err
			}
			continue
		}
		rec.Channel = s.sender.Name()
		if err := s.sender.SendText(ctx, target, rec.Text); err != nil {
			s.logger.Warn("主动关怀投递失败", "error", err.Error())
			rec.Status = storage.InitiativeStatusFailed
			rec.SkipReason = "投递失败: " + err.Error()
			if err := s.store.SaveInitiativeOutbox(ctx, rec); err != nil {
				return Outcome{}, err
			}
			continue
		}
		rec.Status = storage.InitiativeStatusSent
		rec.Delivered = time.Now().UTC()
		if err := s.store.SaveInitiativeOutbox(ctx, rec); err != nil {
			return Outcome{}, err
		}
		s.logger.Info("主动关怀已投递", "target_mask", maskID(target), "clock", tc.ClockText())
	}

	action := Outcome{OutboxIDs: ids, Text: proposal.Question}
	if s.dryRun {
		action.Action = "dry_run"
		action.Reason = "dry-run：不真实发送"
	} else if s.sender == nil || !s.sender.Enabled() {
		action.Action = "draft"
		action.Reason = "没有可用的投递渠道，草稿已保留"
	} else {
		action.Action = "sent"
	}
	return action, nil
}

// propose 组装提案输入并调用模型。
//
// 事实 JSON 由代码拼装：只含可信时间、当天活动、任务摘要与有限对话状态——
// 与问答路径同一份数据最小化约束。
func (s *Service) propose(ctx context.Context, tc temporal.Context, userID string,
	results []tooling.Result, evidenceIDs []string) (Proposal, error) {
	var b strings.Builder
	b.WriteString(`{"trusted_context":{"date":"` + tc.Date + `","weekday":"` + tc.Weekday +
		`","day_part":"` + tc.DayPart + `","time":"` + tc.Now.Format("15:04") + `"}}`)

	state := contextassembler.ConversationSummary{}
	if st, err := s.store.ConversationStateByUser(ctx, userID); err == nil {
		state = contextassembler.ConversationSummary{
			CurrentProject:  st.CurrentProject,
			PendingQuestion: st.PendingQuestion,
			LastTimeRange:   st.LastTimeRange,
		}
	}
	stateRaw, err := json.Marshal(state)
	if err != nil {
		stateRaw = []byte("{}")
	}
	stateJSON := string(stateRaw)
	b.WriteString(`,"conversation_state":` + stateJSON)

	for _, r := range results {
		b.WriteString(`,"` + r.Tool + `":` + r.ModelJSON())
	}
	b.WriteString(`}`)

	return s.planner.Propose(ctx, ProposalRequest{
		Temporal: tc, State: state,
		FactsJSON:   b.String(),
		EvidenceIDs: evidenceIDs,
	})
}

// validateProposal 校验提案的内容与证据。返回空串表示通过。
//
// basis 允许两种写法（模型的视野决定它**不可能**编造时段 ID）：
//   - 任务摘要的 task_id（模型上下文里可见）；
//   - 事实来源的工具名（get_today_status / get_task_summaries）。
//
// 代码把声明映射回本轮核实的记录 ID：时段的内部 ID 刻意不进模型上下文，
// 因此"依据是本轮事实"由模型声明来源、代码核实记录两级共同保证。
func validateProposal(p Proposal, evidenceIDs, taskIDs, toolNames []string) string {
	q := strings.TrimSpace(p.Question)
	if q == "" {
		return "问题为空"
	}
	if len([]rune(q)) > 200 {
		return "问题超过 200 字"
	}
	if !strings.Contains(q, "？") && !strings.Contains(q, "?") {
		return "关怀必须以提问的方式给出"
	}
	// 依据必须全部能落到本轮事实：task_id ∈ 本轮任务证据，或工具名 ∈ 本轮运行过。
	if len(p.Basis) == 0 {
		return "问题没有任何依据"
	}
	taskSet := map[string]bool{}
	for _, id := range taskIDs {
		taskSet[id] = true
	}
	toolSet := map[string]bool{}
	for _, name := range toolNames {
		toolSet[name] = true
	}
	for _, id := range p.Basis {
		id = strings.TrimSpace(id)
		if !taskSet[id] && !toolSet[id] {
			return fmt.Sprintf("依据 %q 不在本轮已核实的事实里", id)
		}
	}
	// 内容边界：禁词表是封闭的硬规则，不是语义判断。
	lower := strings.ToLower(q)
	for _, bad := range proposalForbiddenTerms {
		if strings.Contains(lower, bad) {
			return fmt.Sprintf("问题包含不允许的内容（%s）", bad)
		}
	}
	for _, bad := range internalTerms {
		if strings.Contains(q, bad) {
			return "问题包含内部术语"
		}
	}
	// 活动时长 ≠ 完成任务：声称"完成"却没有任何 Agent 报告作依据 → 拒。
	// basis 写工具名时，只有任务摘要工具算"有任务依据"。
	hasTaskBasis := intersect(p.Basis, taskIDs)
	for _, b := range p.Basis {
		if strings.TrimSpace(b) == "get_task_summaries" {
			hasTaskBasis = len(taskIDs) > 0
		}
	}
	if claimsCompletion(q) && !hasTaskBasis {
		return "问题宣称了完成状态，但依据里没有 Agent 报告的任务"
	}
	return ""
}

// proposalForbiddenTerms 是关怀问题不允许出现的主题（心理/医疗/财务判断）。
var proposalForbiddenTerms = []string{
	"抑郁", "焦虑", "自杀", "诊断", "治疗", "吃药", "用药", "心理咨询",
	"投资", "理财", "股票", "基金", "借贷", "贷款", "加密货币", "炒币",
}

// internalTerms 是绝不能出现在用户可见文本里的内部术语。
var internalTerms = []string{"unclassified", "session", "```"}

// claimsCompletion 判断是否出现"完成某事"的措辞（与 assistant 同一措辞表）。
func claimsCompletion(text string) bool {
	for _, phrase := range []string{"完成", "做完", "搞定"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func intersect(a, b []string) bool {
	set := map[string]bool{}
	for _, item := range b {
		set[item] = true
	}
	for _, item := range a {
		if set[strings.TrimSpace(item)] {
			return true
		}
	}
	return false
}

// record 给每个目标写一条审计行（拒绝/跳过类结果）。
func (s *Service) record(ctx context.Context, targets []string, rec storage.InitiativeOutbox,
	tc temporal.Context, evidenceIDs []string) {
	for _, target := range targets {
		out := rec
		out.ID = ulid.New()
		out.UserID = target
		out.LocalDate = tc.Date
		out.CreatedAt = time.Now().UTC()
		out.Evidence = evidenceIDs
		if err := s.store.SaveInitiativeOutbox(ctx, out); err != nil {
			s.logger.Warn("写入主动关怀审计失败", "error", err.Error())
		}
	}
}

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

func maskID(id string) string {
	if len(id) <= 6 {
		return "***"
	}
	return id[:3] + "***" + id[len(id)-3:]
}
