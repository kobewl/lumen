package tooling

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"lumen/server/internal/temporal"
)

// Executor 是工具调用的**唯一执行入口**：策略校验 → 执行 → 审计。
//
// 它存在的原因是让"治理"只有一处实现：
//   - Agent 只调用 Run，不自己遍历工具；
//   - 新增工具不会绕过策略与审计；
//   - 执行顺序（读在前、写在后）与证据累积由代码决定，而不是由模型排列决定。
type Executor struct {
	registry *Registry
	gate     *PolicyGate
	audit    AuditSink
	logger   *slog.Logger
}

// ExecutorOptions 是构造执行器的依赖。
type ExecutorOptions struct {
	// Registry 是工具注册表（必需）。
	Registry *Registry
	// Gate 是策略闸门；留空则使用默认策略（只读 + 低风险写，单轮 ≤4 次）。
	Gate *PolicyGate
	// Audit 是审计写入点；留空则记为 NopSink。
	// 生产装配应显式传入存储实现，否则"审计记录"就是空的。
	Audit AuditSink
	// Logger 用于记录执行期异常；留空用 slog.Default()。
	Logger *slog.Logger
}

// NewExecutor 创建执行器。
func NewExecutor(opts ExecutorOptions) (*Executor, error) {
	if opts.Registry == nil {
		return nil, fmt.Errorf("工具执行器缺少注册表")
	}
	gate := opts.Gate
	if gate == nil {
		gate = NewPolicyGate(opts.Registry)
	}
	audit := opts.Audit
	if audit == nil {
		audit = NopSink{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Executor{registry: opts.Registry, gate: gate, audit: audit, logger: logger}, nil
}

// Registry 暴露注册表，供提示词渲染与调试接口使用。
func (e *Executor) Registry() *Registry {
	if e == nil {
		return nil
	}
	return e.registry
}

// Catalog 返回工具目录文本；执行器未初始化时返回空串。
func (e *Executor) Catalog() string {
	if e == nil {
		return ""
	}
	return e.registry.Catalog()
}

// RunInfo 是一次执行的会话级上下文，全部由代码生成，模型无法影响。
type RunInfo struct {
	// Actor 是本轮请求者（用户标识）。
	Actor string
	// Temporal 是本轮的可信时间快照。必须有效：执行器对"缺少可信时间"
	// fail-closed（整轮拒绝）——"禁止各层自行取 time.Now"由此成为结构保证，
	// 而不是一条靠自觉的约定。
	Temporal temporal.Context
	// TurnSource 是"本轮用户消息"的代码生成来源 ID（u_ 前缀），可为空
	// （没有用户输入的触发，例如主动关怀）。
	TurnSource string
}

// Run 执行模型请求的一批调用，返回结果与被拒/失败的调用。
//
// 执行顺序有两条硬规则：
//  1. 先读后写：写工具（RequiresEvidence）的来源必须来自本轮真实读到的记录，
//     所以先执行全部只读调用，再执行写入调用。顺序由代码决定，
//     模型把写入排在最前面也一样安全。
//  2. 证据累积：只读结果的 Evidence 会累积给后续写入调用（去重、保持顺序）。
func (e *Executor) Run(ctx context.Context, calls []Call, info RunInfo) ([]Result, []Denial) {
	if e == nil || e.registry == nil {
		return nil, denyPlan(calls, "工具执行器未初始化")
	}
	if !info.Temporal.Valid() {
		// fail-closed：没有可信时间就不执行任何工具。
		// 这不是锦上添花的校验——它保证"任何一层都不自己取 time.Now"
		// 不会被绕过：装配漏了时间，结果只是诚实的拒绝，
		// 而不是悄悄用各自的本地时钟（那样"中午说早呀"会换个形式回来）。
		return nil, denyPlan(calls, "缺少可信时间上下文")
	}
	allowed, denied := e.gate.Review(calls)
	e.auditGateDenials(ctx, info.Actor, denied)
	if len(allowed) == 0 {
		return nil, denied
	}

	reads := make([]Invocation, 0, len(allowed))
	writes := make([]Invocation, 0, len(allowed))
	for _, inv := range allowed {
		inv.Actor = info.Actor
		inv.Temporal = info.Temporal
		inv.TurnSource = info.TurnSource
		if inv.Risk == RiskRead {
			reads = append(reads, inv)
		} else {
			writes = append(writes, inv)
		}
	}

	results := make([]Result, 0, len(allowed))
	evidence := make([]string, 0, 8)
	seen := map[string]bool{}
	// used 记录写工具在本轮的调用次数，用于 Spec.MaxPerTurn 限制。
	used := map[string]int{}

	for _, inv := range reads {
		res, denial := e.runOne(ctx, inv)
		if denial != nil {
			denied = append(denied, *denial)
			continue
		}
		results = append(results, res)
		for _, id := range res.Evidence {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			evidence = append(evidence, id)
		}
	}

	for _, inv := range writes {
		inv.Evidence = append([]string(nil), evidence...)
		if spec, ok := e.registry.Spec(inv.Tool); ok && spec.MaxPerTurn > 0 {
			if used[inv.Tool] >= spec.MaxPerTurn {
				reason := fmt.Sprintf("本轮最多调用 %d 次", spec.MaxPerTurn)
				e.auditCall(ctx, inv, spec, AuditRecord{Decision: DecisionDenied, Reason: reason})
				denied = append(denied, Denial{Name: inv.Tool, Reason: reason})
				continue
			}
			used[inv.Tool]++
		}
		res, denial := e.runOne(ctx, inv)
		if denial != nil {
			denied = append(denied, *denial)
			continue
		}
		results = append(results, res)
	}

	return results, denied
}

// auditGateDenials 为闸门阶段的拒绝补上审计。
//
// 为什么必须记：被拒的调用是"模型是否在尝试越权"的唯一线索。
// 只记成功调用会让审计账本里看不到任何异常，等于没有审计。
// 这里没有参数（闸门在参数校验前就拒绝了），也没有真实耗时。
func (e *Executor) auditGateDenials(ctx context.Context, actor string, denied []Denial) {
	for _, d := range denied {
		spec, _ := e.registry.Spec(d.Name)
		e.auditCall(ctx, Invocation{Tool: d.Name, Actor: actor}, spec, AuditRecord{
			Decision: DecisionDenied,
			Reason:   d.Reason,
		})
	}
}

// runOne 执行单条调用，并保证**无论成功失败都留一条审计**。
func (e *Executor) runOne(ctx context.Context, inv Invocation) (Result, *Denial) {
	spec, ok := e.registry.Spec(inv.Tool)
	if !ok {
		// Gate 已保证存在；这里只是防御性处理。
		return Result{}, &Denial{Name: inv.Tool, Reason: "未知工具"}
	}
	inv.Risk = spec.Risk

	if spec.RequiresEvidence && len(inv.Evidence) == 0 {
		// 代码级约束：这类工具的来源必须来自本轮真实读到的记录。
		// 拦在调用之前，而不是让它写进去再事后检查。
		reason := "本轮没有可核实的来源证据（需要先成功调用读取工具）"
		e.auditCall(ctx, inv, spec, AuditRecord{
			Decision: DecisionDenied, Reason: reason,
		})
		return Result{}, &Denial{Name: inv.Tool, Reason: reason}
	}

	tool, _ := e.registry.Get(inv.Tool)
	start := time.Now()
	res, err := e.invoke(ctx, tool, inv)
	elapsed := time.Since(start)

	if err != nil {
		reason, deniedByTool := AsDenial(err)
		decision := DecisionError
		if deniedByTool {
			decision = DecisionDenied
		} else {
			// 执行失败的具体错误只进日志：它可能含内部细节，
			// 而给模型与用户的信息只需要"这一步没做成"。
			reason = "执行失败"
			e.logger.Warn("工具执行失败", "tool", inv.Tool, "error", err.Error())
		}
		e.auditCall(ctx, inv, spec, AuditRecord{
			Decision: decision, Reason: reason, DurationMS: elapsed.Milliseconds(),
		})
		return Result{}, &Denial{Name: inv.Tool, Reason: reason}
	}

	res.Tool = inv.Tool
	res.Kind = spec.Kind
	e.auditCall(ctx, inv, spec, AuditRecord{
		Decision:   DecisionAllowed,
		DurationMS: elapsed.Milliseconds(),
		ResultKind: res.Kind,
		Count:      res.Count,
		EvidenceN:  len(res.Evidence),
		Truncated:  res.Truncated,
	})
	return res, nil
}

// invoke 调用工具本身，并把 panic 收敛成错误。
//
// 工具是"外部代码"：一个越界索引不应该让整个 HTTP 请求 500，
// 更不应该让服务进程挂掉——它只应该变成"这一步没做成"。
func (e *Executor) invoke(ctx context.Context, tool Tool, inv Invocation) (res Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			e.logger.Error("工具执行 panic", "tool", inv.Tool, "panic", fmt.Sprint(rec))
			err = fmt.Errorf("工具 panic: %v", rec)
		}
	}()
	return tool.Execute(ctx, inv)
}

// auditCall 写入一条审计；失败只记日志，不影响本轮回答。
//
// 理由：审计是旁路，用户的数据请求不应该因为"审计表写不进去"而失败。
// 但失败必须留痕，否则会变成一个安静的盲区。
func (e *Executor) auditCall(ctx context.Context, inv Invocation, spec Spec, rec AuditRecord) {
	rec.At = time.Now().UTC()
	rec.Actor = inv.Actor
	rec.Tool = inv.Tool
	rec.Risk = spec.Risk
	if rec.ResultKind == "" {
		rec.ResultKind = spec.Kind
	}
	rec.Args = make(map[string]any, len(inv.Args))
	for key, value := range inv.Args {
		rec.Args[key] = clipValueForAudit(value)
	}
	if err := e.audit.Record(ctx, rec); err != nil {
		e.logger.Warn("写入工具审计失败", "tool", inv.Tool, "error", err.Error())
	}
}
