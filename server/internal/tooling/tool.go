// Package tooling 是 Agent 工具层的**协议与治理**（domain 层）。
//
// 它只回答四个问题：
//  1. 一个工具长什么样（名称、中文说明、严格参数 schema、结果 schema、风险级别）；
//  2. 模型请求的这次调用允不允许执行（Policy Gate）；
//  3. 执行结果里哪些给模型看、哪些只用于审计（Result 的字段划分）；
//  4. 每次调用怎么留痕（AuditRecord）。
//
// 它**不含任何具体工具**，也不碰数据库、HTTP 或飞书：
//   - 具体工具在 internal/tools（应用层执行器，依赖显式注入）；
//   - 存储、HTTP、飞书适配在 internal/storage、internal/api、internal/feishu。
//
// 这样切分的意义：加一个新工具不需要动策略、执行与审计；
// 收紧策略或加审计字段也不需要动任何工具实现。
package tooling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"lumen/server/internal/temporal"
)

// RiskLevel 是工具的风险级别。策略闸门按它决定放不放行。
type RiskLevel string

const (
	// RiskRead 只读：不改变任何数据。
	RiskRead RiskLevel = "read"
	// RiskWriteLow 低风险写入：只写"候选/草稿"这类不生效、可再确认的数据。
	// 它必须可被用户撤销，且不能自动变成生效状态。
	RiskWriteLow RiskLevel = "write_low"
	// RiskWriteHigh 高风险写入：会改变既有事实或对外产生副作用。
	// 当前版本一律拒绝；保留这个级别是为了让"将来要放开什么"必须显式写进代码。
	RiskWriteHigh RiskLevel = "write_high"
)

// Label 返回风险级别给模型与用户看的中文说明。
func (r RiskLevel) Label() string {
	switch r {
	case RiskRead:
		return "只读"
	case RiskWriteLow:
		return "低风险写入"
	case RiskWriteHigh:
		return "高风险写入（禁止）"
	default:
		return "未知风险"
	}
}

// Kind 是结果的语义类别。
//
// 它决定了结果在"回答可信度"上的性质：活动记录只能推断，Agent 报告的结论
// 才能直接陈述。这条区分由代码强制，不靠模型自觉（见 assistant 的支持等级收紧）。
type Kind string

const (
	// KindSystemInfo 系统信息（当前时间、身份配置、对话状态）：不构成证据。
	KindSystemInfo Kind = "system_info"
	// KindReference 参考信息（项目清单等）：帮助对齐名字，不构成证据。
	KindReference Kind = "reference"
	// KindActivity 活动记录（工作时段）：只能说明"用了什么、多久"。
	KindActivity Kind = "activity"
	// KindReportedTasks 专业 Agent 汇报的任务摘要：唯一带结论的来源。
	KindReportedTasks Kind = "reported_tasks"
	// KindMemoryWrite 记忆候选写入结果。
	KindMemoryWrite Kind = "memory_write"
)

// ParamType 是参数类型，取 JSON Schema 的一个严格子集。
//
// 刻意不做完整的 JSON Schema：支持的形状越少，"模型能给什么参数"
// 就越容易在代码里枚举清楚，也越容易测试。
type ParamType string

const (
	ParamString      ParamType = "string"
	ParamInteger     ParamType = "integer"
	ParamNumber      ParamType = "number"
	ParamStringArray ParamType = "string_array"
)

// Label 返回参数类型的中文说明。
func (t ParamType) Label() string {
	switch t {
	case ParamString:
		return "字符串"
	case ParamInteger:
		return "整数"
	case ParamNumber:
		return "数字"
	case ParamStringArray:
		return "字符串数组"
	default:
		return "未知类型"
	}
}

// Param 是单个参数的严格 schema。
//
// HasRange 用来表达"数值范围是否受限"：0 是合法取值（例如 confidence 允许 0），
// 因此不能用零值表示"不限制"，必须显式声明。
type Param struct {
	// Name 是参数名（snake_case）。
	Name string
	// Type 决定接受什么形状的值。
	Type ParamType
	// Description 是给模型看的中文说明（含取值范围与用途）。
	Description string
	// Required 表示必须提供。
	Required bool
	// Enum 非空时取值必须落在其中（仅对 string）。
	Enum []string
	// MinLength/MaxLength 限制字符串长度（string 按字符数，string_array 按单元素）。
	MinLength int
	MaxLength int
	// Min/Max 是数值范围，仅当 HasRange 为真时检查（对 integer/number）。
	Min      float64
	Max      float64
	HasRange bool
	// MaxItems 限制数组元素个数（string_array）。
	MaxItems int
}

// Spec 是一个工具的完整声明：它既是给模型看的目录，也是策略校验的依据。
type Spec struct {
	// Name 是工具名（snake_case），模型在 plan.tool_calls[].name 里引用它。
	Name string
	// Summary 是中文一句话说明：这个工具做什么、什么时候该用它。
	Summary string
	// Parameters 是严格参数 schema，按声明顺序渲染给模型。
	Parameters []Param
	// ResultSchema 是返回结构的 JSON 示例：模型据此知道能读到哪些字段。
	ResultSchema string
	// Kind 是结果语义类别（决定它算证据还是推断）。
	Kind Kind
	// Risk 是风险级别。
	Risk RiskLevel
	// RequiresEvidence 表示调用之前，本轮必须已经读到可核实的记录证据。
	// 写工具用它把"必须带来源"变成代码级约束，而不是靠模型自觉。
	RequiresEvidence bool
	// MaxPerTurn 限制同一个工具在一轮里最多被调用几次（0 表示不限）。
	// 写工具用它防止模型一轮刷出一堆候选。
	MaxPerTurn int
}

// Tool 是一个可被模型选择的工具。
//
// 实现方（internal/tools）只负责"用给定参数做一件事，并返回脱敏后的结果"：
// 权限、参数校验、执行顺序与审计都在本包里统一处理。
type Tool interface {
	// Spec 返回工具声明。同一个工具每次返回必须一致（注册表会缓存校验结果）。
	Spec() Spec
	// Execute 执行工具。
	//
	// 返回值必须是"给模型的视图"：不含内部 ID、不含用户不该看到的内容。
	// 拒绝这次调用（例如缺少来源证据）时返回 Deny(...)，而不是普通 error：
	// 前者是"按规则不该做"，后者是"做失败了"，两者在审计与提示词里待遇不同。
	Execute(ctx context.Context, inv Invocation) (Result, error)
}

// Args 是校验后的参数：只含 schema 声明过的键，类型已归一。
type Args map[string]any

// String 读取字符串参数，缺失时返回空串。
func (a Args) String(name string) string {
	s, _ := a[name].(string)
	return s
}

// Int 读取整数参数，缺失时返回 0。
func (a Args) Int(name string) int {
	v, _ := a[name].(int)
	return v
}

// IntOr 读取整数参数，缺失时返回默认值。
func (a Args) IntOr(name string, def int) int {
	if v, ok := a[name].(int); ok {
		return v
	}
	return def
}

// Number 读取数值参数，缺失时返回 0。
func (a Args) Number(name string) float64 {
	v, _ := a[name].(float64)
	return v
}

// Strings 读取字符串数组参数，缺失时返回 nil。
func (a Args) Strings(name string) []string {
	v, _ := a[name].([]string)
	return v
}

// Has 判断参数是否存在。
func (a Args) Has(name string) bool {
	_, ok := a[name]
	return ok
}

// Invocation 是一次将要执行（或已执行）的调用。
//
// 参数来自模型，其余字段全部由代码注入——尤其是 Actor：
// 模型**不能**通过参数指定"读谁的数据"，否则越权只是一个参数的事。
type Invocation struct {
	// Tool 是工具名。
	Tool string
	// Args 是校验后的参数。
	Args Args
	// Actor 是本轮请求者（用户标识）。
	Actor string
	// Temporal 是本轮的可信时间快照（执行器注入，与 Planner/Synthesizer 同一对象）。
	// 需要当前时间的工具从这里读，不允许自己取 time.Now。
	Temporal temporal.Context
	// TurnSource 是"本轮用户消息"的代码生成来源 ID（u_ 前缀）。
	// 供写工具给"用户直接表达"的记忆挂来源；模型给不出也伪造不了。
	TurnSource string
	// Evidence 是这次调用之前，本轮已核实的记录 ID。
	// 写工具据此挂来源；读工具通常不需要它。
	Evidence []string
	// Risk 由注册表填入，审计用。
	Risk RiskLevel
}

// Span 是结果覆盖的时间范围。
type Span struct {
	Start time.Time
	End   time.Time
}

// Valid 判断区间是否可用（两端都有值且顺序正确）。
func (s Span) Valid() bool {
	return !s.Start.IsZero() && !s.End.IsZero() && !s.End.Before(s.Start)
}

// MergeSpans 计算一组区间的并集范围：最早开始 ~ 最晚结束。
//
// 为什么必须这样算：多个查询的返回各自成序（按项目查是倒序、按日期查是正序），
// 拼在一起整体并不有序。取首尾元素会写出一个根本不存在的区间，
// 用户拿它去核对就会发现对不上。
func MergeSpans(spans ...Span) Span {
	var out Span
	for _, s := range spans {
		if !s.Valid() {
			continue
		}
		if out.Start.IsZero() || s.Start.Before(out.Start) {
			out.Start = s.Start
		}
		if out.End.IsZero() || s.End.After(out.End) {
			out.End = s.End
		}
	}
	return out
}

// Result 是一次工具执行的结果。
//
// 字段按"给谁看"严格划分：
//   - Model / Digest 会进入提示词与用户可见文本，因此必须已脱敏、已限额；
//   - Evidence / Count / Span 只用于代码判断与审计，绝不进入模型上下文。
type Result struct {
	// Tool 是产生这个结果的工具名。
	Tool string
	// Kind 是结果语义类别，来自 Spec。
	Kind Kind
	// Model 是给模型看的视图（脱敏）。Synthesizer 只会看到它。
	Model any
	// Digest 是合成失败时直接陈述的确定性事实行（脱敏）。
	Digest []string
	// Evidence 是可在审计与来源校验中核实的记录 ID。
	Evidence []string
	// Count 是结果里的实质记录条数（证据行与支持等级判断用）。
	Count int
	// Span 是结果覆盖的时间范围；零值表示不适用。
	Span Span
	// Truncated 表示结果因限额被裁剪过。
	Truncated bool
}

// ModelJSON 把模型视图序列化成提示词里的 JSON。
//
// 序列化失败时返回 "{}"：宁可少给事实，也不能把半截 JSON 塞进提示词——
// 模型会把残缺内容当成完整数据来读。
func (r Result) ModelJSON() string {
	if r.Model == nil {
		return "{}"
	}
	b, err := json.Marshal(r.Model)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Denial 记录一次被拒绝或失败的调用，用于审计与告知模型"这一步没做成"。
//
// 带 json 标签：它会出现在只读调试接口的响应里。
type Denial struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// DeniedError 表示工具主动拒绝这次调用（例如缺少可核实的来源证据）。
//
// 与普通错误的区别：它说明"这次操作按规则就不该发生"，
// 因此要作为拒绝进审计与 <denied_steps>，而不是当成故障重试。
type DeniedError struct {
	Reason string
}

func (e *DeniedError) Error() string { return e.Reason }

// Deny 构造一次工具级拒绝。
func Deny(format string, args ...any) error {
	return &DeniedError{Reason: fmt.Sprintf(format, args...)}
}

// AsDenial 判断错误是否属于工具级拒绝，并取出原因。
func AsDenial(err error) (string, bool) {
	var de *DeniedError
	if errors.As(err, &de) {
		return de.Reason, true
	}
	return "", false
}
