package tooling

import (
	"fmt"
	"strings"
)

// Call 是模型请求的一次工具调用（只有名字与参数，全部来自模型）。
type Call struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// PolicyGate 是模型意图与真实数据之间的唯一闸门。
//
// 它**不判断用户想干什么**（那是模型的事），只回答一个具体问题：
// "模型想调用的这个工具、带这些参数，允不允许执行？"
// 因此这里的规则必须是可枚举、可测试的硬规则，不能有任何模糊判断。
type PolicyGate struct {
	registry *Registry
	// maxCalls 限制单轮计划里的工具调用数，防止模型一次拉全库。
	maxCalls int
}

// NewPolicyGate 创建闸门。
func NewPolicyGate(registry *Registry) *PolicyGate {
	return &PolicyGate{registry: registry, maxCalls: 4}
}

// Review 逐条审查模型请求的调用，返回放行（已带干净参数）与拦截两组结果。
//
// 拦截不会让整轮失败：被拒的调用被剔除，模型仍能就"这一步没做成"作答。
// 这样比整轮报错更有用：用户至少能得到部分回答，并且知道哪部分没做成。
//
// 闸门或注册表未初始化时**全部拒绝**，而不是 panic：这是 fail closed。
// 装配漏了配置只应该让 Agent 少干活（回答里如实说"这一步没做成"），
// 绝不能变成"没有闸门就放行"，也不该让整个服务进程崩掉。
func (g *PolicyGate) Review(calls []Call) ([]Invocation, []Denial) {
	if g == nil {
		return nil, denyPlan(calls, "策略闸门未初始化")
	}
	if g.registry == nil {
		return nil, denyPlan(calls, "工具注册表未初始化")
	}
	if len(calls) == 0 {
		return nil, nil
	}
	if len(calls) > g.maxCalls {
		// 超量的调用全部拒绝，而不是截断执行前 N 个：截断会让模型以为
		// 自己拿到了完整数据，得出错误结论。
		return nil, []Denial{{
			Name:   fmt.Sprintf("%d 个工具调用", len(calls)),
			Reason: fmt.Sprintf("单轮最多允许 %d 个工具调用", g.maxCalls),
		}}
	}

	allowed := make([]Invocation, 0, len(calls))
	var denied []Denial

	for _, call := range calls {
		inv, reason := g.reviewOne(call)
		if reason != "" {
			denied = append(denied, Denial{Name: strings.TrimSpace(call.Name), Reason: reason})
			continue
		}
		allowed = append(allowed, inv)
	}
	return allowed, denied
}

// denyPlan 把请求过的每一次调用都记为拒绝。
//
// 保留工具名（而不是给一条汇总）：审计时要能看出模型具体请求了什么，
// 这是判断"模型是不是在越权尝试"的唯一线索。
func denyPlan(calls []Call, reason string) []Denial {
	if len(calls) == 0 {
		return nil
	}
	denied := make([]Denial, 0, len(calls))
	for _, call := range calls {
		denied = append(denied, Denial{Name: strings.TrimSpace(call.Name), Reason: reason})
	}
	return denied
}

// reviewOne 审查单条调用，返回放行后的调用或拒绝原因。
func (g *PolicyGate) reviewOne(call Call) (Invocation, string) {
	name := strings.TrimSpace(call.Name)
	if name == "" {
		return Invocation{}, "工具名为空"
	}
	if g.registry == nil {
		// 双保险：即使将来有人绕过 Review 直接调用这里，也不会 panic。
		return Invocation{}, "工具注册表未初始化"
	}
	tool, ok := g.registry.Get(name)
	if !ok {
		// 目录之外的工具（含模型臆造的名字）一律拒绝。
		return Invocation{}, fmt.Sprintf("未知工具 %q（只能使用工具目录里的名字）", name)
	}
	spec := tool.Spec()
	if spec.Risk == RiskWriteHigh {
		// 高风险写入必须走单独的审查路径，不会被顺手放行。
		return Invocation{}, fmt.Sprintf("工具 %q 属于高风险写入，当前版本不允许执行", name)
	}

	args, reason := validateArgs(spec.Parameters, call.Arguments)
	if reason != "" {
		return Invocation{}, fmt.Sprintf("工具 %q %s", name, reason)
	}
	return Invocation{Tool: name, Args: args, Risk: spec.Risk}, ""
}
