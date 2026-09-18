package assistant

import (
	"context"
	"testing"
)

// noopCapability 是测试用的只读能力，声明两个参数用于覆盖参数校验。
type noopCapability struct {
	name     string
	readOnly bool
}

func (c *noopCapability) Name() string        { return c.name }
func (c *noopCapability) Description() string { return "测试能力" }
func (c *noopCapability) Parameters() map[string]ParamSpec {
	return map[string]ParamSpec{
		"date":    {Type: "string", MaxLength: 10},
		"limit":   {Type: "int"},
		"project": {Type: "string", Required: true},
		"scope":   {Type: "string", Enum: []string{"today", "week"}},
	}
}
func (c *noopCapability) ReadOnly() bool { return c.readOnly }
func (c *noopCapability) Run(context.Context, map[string]any) (any, error) {
	return "ok", nil
}

func newTestGate() *PolicyGate {
	return NewPolicyGate(NewRegistry(
		&noopCapability{name: "get_sessions", readOnly: true},
		&noopCapability{name: "write_notes", readOnly: false},
	))
}

// TestGateRejectsUnknownCapability 覆盖工具白名单：模型臆造的能力名必须被拒。
func TestGateRejectsUnknownCapability(t *testing.T) {
	gate := newTestGate()
	allowed, denied := gate.Review(Plan{ToolCalls: []ToolCall{
		{Name: "run_sql", Arguments: map[string]any{"query": "SELECT * FROM sessions"}},
	}})

	if len(allowed) != 0 {
		t.Fatalf("未知能力不应被放行，实际放行 %d 个", len(allowed))
	}
	if len(denied) != 1 || denied[0].Name != "run_sql" {
		t.Fatalf("应记录被拦截的 run_sql，实际 %+v", denied)
	}
}

// TestGateRejectsWriteCapability 覆盖只读约束：V0.1 不允许任何写能力。
func TestGateRejectsWriteCapability(t *testing.T) {
	gate := newTestGate()
	allowed, denied := gate.Review(Plan{ToolCalls: []ToolCall{
		{Name: "write_notes", Arguments: map[string]any{"project": "lumen"}},
	}})

	if len(allowed) != 0 {
		t.Fatal("写能力不应被放行")
	}
	if len(denied) != 1 {
		t.Fatalf("应记录一次拦截，实际 %+v", denied)
	}
}

// TestGateRejectsUndeclaredArgument 覆盖参数白名单。
//
// 未声明的参数直接拒绝整条调用，而不是丢弃参数后照常执行：
// 丢参数会让模型以为自己的意图被满足了。
func TestGateRejectsUndeclaredArgument(t *testing.T) {
	gate := newTestGate()
	allowed, denied := gate.Review(Plan{ToolCalls: []ToolCall{
		{Name: "get_sessions", Arguments: map[string]any{
			"project": "lumen",
			"sql":     "DROP TABLE sessions",
		}},
	}})

	if len(allowed) != 0 {
		t.Fatal("带未声明参数的调用不应被放行")
	}
	if len(denied) != 1 {
		t.Fatalf("应记录一次拦截，实际 %+v", denied)
	}
}

// TestGateRejectsWrongArgumentType 覆盖参数类型校验。
func TestGateRejectsWrongArgumentType(t *testing.T) {
	gate := newTestGate()
	allowed, _ := gate.Review(Plan{ToolCalls: []ToolCall{
		{Name: "get_sessions", Arguments: map[string]any{"project": "lumen", "limit": "十"}},
	}})
	if len(allowed) != 0 {
		t.Fatal("参数类型错误不应被放行")
	}
}

// TestGateRejectsTooLongArgument 覆盖参数长度上限，防止把整库内容塞进查询条件。
func TestGateRejectsTooLongArgument(t *testing.T) {
	gate := newTestGate()
	long := make([]rune, 200)
	for i := range long {
		long[i] = 'a'
	}
	allowed, _ := gate.Review(Plan{ToolCalls: []ToolCall{
		{Name: "get_sessions", Arguments: map[string]any{"project": string(long)}},
	}})
	if len(allowed) != 0 {
		t.Fatal("超长参数不应被放行")
	}
}

// TestGateRejectsEnumViolation 覆盖枚举取值。
func TestGateRejectsEnumViolation(t *testing.T) {
	gate := newTestGate()
	allowed, _ := gate.Review(Plan{ToolCalls: []ToolCall{
		{Name: "get_sessions", Arguments: map[string]any{"project": "lumen", "scope": "forever"}},
	}})
	if len(allowed) != 0 {
		t.Fatal("枚举外的取值不应被放行")
	}
}

// TestGateRejectsMissingRequiredArgument 覆盖必填参数。
func TestGateRejectsMissingRequiredArgument(t *testing.T) {
	gate := newTestGate()
	allowed, denied := gate.Review(Plan{ToolCalls: []ToolCall{
		{Name: "get_sessions", Arguments: map[string]any{"date": "2026-09-17"}},
	}})
	if len(allowed) != 0 {
		t.Fatal("缺少必填参数不应被放行")
	}
	if len(denied) != 1 {
		t.Fatalf("应记录一次拦截，实际 %+v", denied)
	}
}

// TestGateRejectsTooManyCalls 覆盖单轮调用数量上限。
//
// 超量时全部拒绝而不是截断执行前 N 个：截断会让模型以为自己拿到完整数据。
func TestGateRejectsTooManyCalls(t *testing.T) {
	gate := newTestGate()
	calls := make([]ToolCall, 0, 6)
	for i := 0; i < 6; i++ {
		calls = append(calls, ToolCall{Name: "get_sessions", Arguments: map[string]any{"project": "lumen"}})
	}
	allowed, denied := gate.Review(Plan{ToolCalls: calls})
	if len(allowed) != 0 {
		t.Fatalf("超量调用应全部拒绝，实际放行 %d 个", len(allowed))
	}
	if len(denied) != 1 {
		t.Fatalf("应记录一次超量拦截，实际 %+v", denied)
	}
}

// TestGateAllowsValidCallAndNormalizesArgs 覆盖合法调用放行与参数归一。
func TestGateAllowsValidCallAndNormalizesArgs(t *testing.T) {
	gate := newTestGate()
	allowed, denied := gate.Review(Plan{ToolCalls: []ToolCall{
		{Name: "get_sessions", Arguments: map[string]any{
			"project": " lumen ",
			"limit":   float64(5),
			"scope":   "today",
		}},
	}})

	if len(denied) != 0 {
		t.Fatalf("合法调用不应被拦截，实际 %+v", denied)
	}
	if len(allowed) != 1 {
		t.Fatalf("应放行 1 个调用，实际 %d", len(allowed))
	}
	args := allowed[0].Arguments
	if args["project"] != "lumen" {
		t.Fatalf("字符串参数应去空格，实际 %v", args["project"])
	}
	if args["limit"] != 5 {
		t.Fatalf("JSON 数字应转成 int，实际 %#v", args["limit"])
	}
}

// TestGatePartialApproval 覆盖部分放行：被拒的调用不影响其余调用执行。
//
// 这样比整轮报错更有用：用户至少能得到部分回答，并且知道哪部分没做成。
func TestGatePartialApproval(t *testing.T) {
	gate := newTestGate()
	allowed, denied := gate.Review(Plan{ToolCalls: []ToolCall{
		{Name: "get_sessions", Arguments: map[string]any{"project": "lumen"}},
		{Name: "run_sql", Arguments: map[string]any{}},
	}})

	if len(allowed) != 1 || allowed[0].Name != "get_sessions" {
		t.Fatalf("应只放行 get_sessions，实际 %+v", allowed)
	}
	if len(denied) != 1 || denied[0].Name != "run_sql" {
		t.Fatalf("应只拦截 run_sql，实际 %+v", denied)
	}
}
