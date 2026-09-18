package assistant

import (
	"fmt"
	"strings"
)

// GateDecision 是一次工具调用的放行结果。
type GateDecision struct {
	Allowed bool
	Reason  string
	// Args 是清洗后的参数：类型已归一，未声明的键已剔除。
	Args map[string]any
}

// DeniedToolCalls 记录被拦截的调用，用于审计与告知模型"这一步没做成"。
//
// 带 json 标签：这个结构会出现在只读调试接口的响应里，
// 字段名必须与其它接口一致地用 snake_case。
type DeniedToolCalls struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// PolicyGate 是模型意图与真实数据之间的唯一闸门。
//
// 它**不判断用户想干什么**（那是模型的事），只回答一个具体问题：
// "模型想调用的这个能力、带这些参数，允不允许执行？"
// 因此这里的规则必须是可枚举、可测试的硬规则，不能有任何模糊判断。
type PolicyGate struct {
	registry *CapabilityRegistry
	// maxCalls 限制单轮计划里的工具调用数，防止模型一次拉全库。
	maxCalls int
	// maxArgValueLen 限制单个字符串参数的绝对长度。
	maxArgValueLen int
}

// NewPolicyGate 创建闸门。
func NewPolicyGate(registry *CapabilityRegistry) *PolicyGate {
	return &PolicyGate{registry: registry, maxCalls: 4, maxArgValueLen: 128}
}

// Review 逐条审查计划里的工具调用，返回放行与拦截两组结果。
//
// 拦截不会让整轮失败：被拒的调用被剔除，模型仍能就"这一步没做成"作答。
// 这样比整轮报错更有用：用户至少能得到部分回答，并且知道哪部分没做成。
//
// 闸门或注册表未初始化时**全部拒绝**，而不是 panic：这是 fail closed。
// 装配漏了配置只应该让 Agent 少干活（回答里如实说"这一步没做成"），
// 绝不能变成"没有闸门就放行"，也不该让整个服务进程崩掉。
func (g *PolicyGate) Review(plan Plan) ([]ToolCall, []DeniedToolCalls) {
	if g == nil {
		return nil, denyPlan(plan, "策略闸门未初始化")
	}
	if g.registry == nil {
		return nil, denyPlan(plan, "能力注册表未初始化")
	}

	if len(plan.ToolCalls) > g.maxCalls {
		// 超量的调用全部拒绝，而不是截断执行前 N 个：截断会让模型以为
		// 自己拿到了完整数据，得出错误结论。
		return nil, []DeniedToolCalls{{
			Name:   fmt.Sprintf("%d 个工具调用", len(plan.ToolCalls)),
			Reason: fmt.Sprintf("单轮最多允许 %d 个工具调用", g.maxCalls),
		}}
	}

	allowed := make([]ToolCall, 0, len(plan.ToolCalls))
	var denied []DeniedToolCalls

	for _, call := range plan.ToolCalls {
		decision := g.reviewOne(call)
		if !decision.Allowed {
			denied = append(denied, DeniedToolCalls{Name: call.Name, Reason: decision.Reason})
			continue
		}
		allowed = append(allowed, ToolCall{Name: call.Name, Arguments: decision.Args})
	}
	return allowed, denied
}

// denyPlan 把计划里请求过的每一次调用都记为拒绝。
//
// 保留工具名（而不是给一条汇总）：审计时要能看出模型具体请求了什么，
// 这是判断"模型是不是在越权尝试"的唯一线索。
func denyPlan(plan Plan, reason string) []DeniedToolCalls {
	if len(plan.ToolCalls) == 0 {
		return nil
	}
	denied := make([]DeniedToolCalls, 0, len(plan.ToolCalls))
	for _, call := range plan.ToolCalls {
		denied = append(denied, DeniedToolCalls{Name: call.Name, Reason: reason})
	}
	return denied
}

// reviewOne 审查单条调用。
func (g *PolicyGate) reviewOne(call ToolCall) GateDecision {
	name := strings.TrimSpace(call.Name)
	if name == "" {
		return GateDecision{Reason: "工具名为空"}
	}

	if g.registry == nil {
		// 双保险：即使将来有人绕过 Review 直接调用这里，也不会 panic。
		return GateDecision{Reason: "能力注册表未初始化"}
	}
	capability, ok := g.registry.Get(name)
	if !ok {
		// 白名单之外的能力（含模型臆造的名字）一律拒绝。
		return GateDecision{Reason: fmt.Sprintf("未知能力 %q", name)}
	}
	if !capability.ReadOnly() {
		// V0.1 没有任何写能力；这条断言式的检查保证将来新增能力时，
		// 写操作必须显式经过单独的审查路径，而不是被顺手放行。
		return GateDecision{Reason: fmt.Sprintf("能力 %q 不是只读能力，V0.1 不允许执行", name)}
	}

	specs := capability.Parameters()
	clean := make(map[string]any, len(specs))
	for key, value := range call.Arguments {
		spec, declared := specs[key]
		if !declared {
			// 未声明的参数直接拒绝这条调用，而不是丢弃参数后照常执行：
			// 丢参数会让模型以为自己的意图被满足了。
			return GateDecision{Reason: fmt.Sprintf("能力 %q 不支持参数 %q", name, key)}
		}
		normalized, err := normalizeArg(value, spec)
		if err != nil {
			return GateDecision{Reason: fmt.Sprintf("参数 %q 非法: %v", key, err)}
		}
		clean[key] = normalized
	}

	for key, spec := range specs {
		if spec.Required {
			if _, ok := clean[key]; !ok {
				return GateDecision{Reason: fmt.Sprintf("缺少必需参数 %q", key)}
			}
		}
	}

	return GateDecision{Allowed: true, Args: clean}
}

// normalizeArg 把参数归一到声明的类型，并检查取值范围。
func normalizeArg(value any, spec ParamSpec) (any, error) {
	switch spec.Type {
	case "string":
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("应为字符串")
		}
		s = strings.TrimSpace(s)
		limit := spec.MaxLength
		if limit <= 0 {
			limit = 128
		}
		if len([]rune(s)) > limit {
			return nil, fmt.Errorf("长度超过 %d", limit)
		}
		if len(spec.Enum) > 0 {
			found := false
			for _, e := range spec.Enum {
				if s == e {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("取值不在允许范围内")
			}
		}
		return s, nil
	case "int":
		// JSON 解出来的数字统一是 float64；只接受整数值。
		switch v := value.(type) {
		case float64:
			if v != float64(int(v)) {
				return nil, fmt.Errorf("应为整数")
			}
			return int(v), nil
		case int:
			return v, nil
		default:
			return nil, fmt.Errorf("应为整数")
		}
	default:
		return nil, fmt.Errorf("未知参数类型 %q", spec.Type)
	}
}
