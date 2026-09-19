package tooling

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lumen/server/internal/temporal"
)

// testNow 是工具层测试的固定"现在"：2026-09-18 13:19（周五，下午）。
// 用固定时刻而不是 time.Now：测试必须可复现，且时段断言不随跑测试的时间漂移。
var testNow = time.Date(2026, 9, 18, 13, 19, 0, 0, time.UTC)

// runInfo 构造一个带可信时间的 RunInfo（多数测试用）。
func runInfo() RunInfo {
	return RunInfo{Actor: "u1", Temporal: temporal.Build(temporal.FixedClock(testNow), time.UTC)}
}

// stubTool 是测试用的工具：声明可控，行为可控。
type stubTool struct {
	spec    Spec
	result  Result
	err     error
	ran     int
	lastInv Invocation
}

func (s *stubTool) Spec() Spec { return s.spec }

func (s *stubTool) Execute(_ context.Context, inv Invocation) (Result, error) {
	s.ran++
	s.lastInv = inv
	if s.err != nil {
		return Result{}, s.err
	}
	out := s.result
	out.Evidence = append([]string(nil), s.result.Evidence...)
	return out, nil
}

func readSpec(name string, params ...Param) Spec {
	return Spec{
		Name: name, Summary: "测试工具 " + name, Parameters: params,
		ResultSchema: `{"ok":true}`, Kind: KindActivity, Risk: RiskRead,
	}
}

// ---- 注册表 ----

// TestRegistryRejectsInvalidSpec 覆盖"声明写错就让启动失败"。
//
// 注册表构造期校验声明：写错的风险级别、空结果 schema、非法参数名
// 都必须在装配时暴露，而不是留到线上表现为"模型调用被莫名拒绝"。
func TestRegistryRejectsInvalidSpec(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
	}{
		{"非法工具名", Spec{Name: "Get Sessions", Summary: "x", ResultSchema: `{}`, Kind: KindActivity, Risk: RiskRead}},
		{"空说明", Spec{Name: "get_x", Summary: "  ", ResultSchema: `{}`, Kind: KindActivity, Risk: RiskRead}},
		{"未知风险", Spec{Name: "get_x", Summary: "x", ResultSchema: `{}`, Kind: KindActivity, Risk: "whatever"}},
		{"未知类别", Spec{Name: "get_x", Summary: "x", ResultSchema: `{}`, Kind: "weird", Risk: RiskRead}},
		{"结果 schema 非法", Spec{Name: "get_x", Summary: "x", ResultSchema: `[...]`, Kind: KindActivity, Risk: RiskRead}},
		{"只读工具要求来源", Spec{Name: "get_x", Summary: "x", ResultSchema: `{}`, Kind: KindActivity,
			Risk: RiskRead, RequiresEvidence: true}},
		{"重复参数", Spec{Name: "get_x", Summary: "x", ResultSchema: `{}`, Kind: KindActivity, Risk: RiskRead,
			Parameters: []Param{
				{Name: "date", Type: ParamString, MaxLength: 10, Description: "d"},
				{Name: "date", Type: ParamString, MaxLength: 10, Description: "d"},
			}}},
		{"参数缺说明", Spec{Name: "get_x", Summary: "x", ResultSchema: `{}`, Kind: KindActivity, Risk: RiskRead,
			Parameters: []Param{{Name: "date", Type: ParamString, MaxLength: 10}}}},
		{"枚举缺长度上限", Spec{Name: "get_x", Summary: "x", ResultSchema: `{}`, Kind: KindActivity, Risk: RiskRead,
			Parameters: []Param{{Name: "kind", Type: ParamString, Enum: []string{"a"}, Description: "k"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewRegistry(&stubTool{spec: c.spec}); err == nil {
				t.Fatalf("非法声明应被拒绝: %+v", c.spec)
			}
		})
	}
}

// TestRegistryRejectsDuplicateNames 覆盖重名拒绝。
func TestRegistryRejectsDuplicateNames(t *testing.T) {
	a := &stubTool{spec: readSpec("get_x")}
	b := &stubTool{spec: readSpec("get_x")}
	if _, err := NewRegistry(a, b); err == nil {
		t.Fatal("重名工具必须拒绝装配：否则「模型调用了哪个工具」是不确定的")
	}
}

// TestNilRegistryAccessors 覆盖注册表自身的空安全。
//
// 这些方法会被编排器与提示词生成直接调用，nil 时 panic 会波及整个进程。
func TestNilRegistryAccessors(t *testing.T) {
	var r *Registry
	if _, ok := r.Get("get_x"); ok {
		t.Fatal("空注册表不应返回命中")
	}
	if _, ok := r.Spec("get_x"); ok {
		t.Fatal("空注册表不应返回声明")
	}
	if names := r.Names(); len(names) != 0 {
		t.Fatalf("空注册表不应有名字，实际 %v", names)
	}
	if specs := r.Specs(); len(specs) != 0 {
		t.Fatalf("空注册表不应有声明，实际 %v", specs)
	}
	if catalog := r.Catalog(); catalog != "" {
		t.Fatalf("空注册表的目录应为空，实际 %q", catalog)
	}
	if n := r.Len(); n != 0 {
		t.Fatalf("空注册表长度应为 0，实际 %d", n)
	}
}

// TestCatalogShowsSchemaAndRisk 覆盖目录渲染：模型必须看到风险、参数与返回形状。
//
// 目录是模型唯一的能力清单。少了参数说明，模型只能猜参数名；
// 少了返回 schema，它会以为返回里有不存在的字段。
func TestCatalogShowsSchemaAndRisk(t *testing.T) {
	tool := &stubTool{spec: Spec{
		Name: "get_sessions", Summary: "查时段", ResultSchema: `{"sessions":[]}`,
		Kind: KindActivity, Risk: RiskRead,
		Parameters: []Param{
			{Name: "date", Type: ParamString, MaxLength: 10, Description: "日期 YYYY-MM-DD"},
			{Name: "limit", Type: ParamInteger, HasRange: true, Min: 1, Max: 30, Description: "条数上限"},
		},
	}}
	r, err := NewRegistry(tool)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	catalog := r.Catalog()
	for _, want := range []string{"get_sessions", "只读", "date", "最长 10 字", "limit", "范围 1~30", `"sessions"`} {
		if !strings.Contains(catalog, want) {
			t.Fatalf("目录应包含 %q，实际:\n%s", want, catalog)
		}
	}
}

// ---- 策略闸门 ----

// TestGateRejectsUnknownTool 覆盖工具白名单。
func TestGateRejectsUnknownTool(t *testing.T) {
	r, _ := NewRegistry(&stubTool{spec: readSpec("get_sessions")})
	gate := NewPolicyGate(r)

	allowed, denied := gate.Review([]Call{{Name: "run_sql", Arguments: map[string]any{"query": "SELECT 1"}}})
	if len(allowed) != 0 {
		t.Fatalf("未知工具不应被放行，实际 %d 个", len(allowed))
	}
	if len(denied) != 1 || denied[0].Name != "run_sql" {
		t.Fatalf("应记录被拦截的 run_sql，实际 %+v", denied)
	}
	if !strings.Contains(denied[0].Reason, "未知工具") {
		t.Fatalf("拒绝原因应说明工具不在目录里，实际 %q", denied[0].Reason)
	}
}

// TestGateRejectsUndeclaredArgument 覆盖参数白名单。
//
// 未声明的参数直接拒绝整条调用，而不是丢弃参数后照常执行：
// 丢参数会让模型以为自己的意图被满足了。
func TestGateRejectsUndeclaredArgument(t *testing.T) {
	tool := &stubTool{spec: readSpec("get_sessions",
		Param{Name: "project", Type: ParamString, MaxLength: 64, Description: "项目名"})}
	r, _ := NewRegistry(tool)
	gate := NewPolicyGate(r)

	allowed, denied := gate.Review([]Call{{Name: "get_sessions", Arguments: map[string]any{
		"project": "lumen",
		"sql":     "DROP TABLE sessions",
	}}})
	if len(allowed) != 0 {
		t.Fatal("带未声明参数的调用不应被放行")
	}
	if len(denied) != 1 || !strings.Contains(denied[0].Reason, "不支持参数") {
		t.Fatalf("应说明参数不被支持，实际 %+v", denied)
	}
}

// TestGateNormalizesArgs 覆盖类型归一与去空格。
func TestGateNormalizesArgs(t *testing.T) {
	tool := &stubTool{spec: readSpec("get_sessions",
		Param{Name: "project", Type: ParamString, MaxLength: 64, Description: "项目名"},
		Param{Name: "limit", Type: ParamInteger, HasRange: true, Min: 1, Max: 30, Description: "上限"})}
	r, _ := NewRegistry(tool)
	gate := NewPolicyGate(r)

	allowed, denied := gate.Review([]Call{{Name: "get_sessions", Arguments: map[string]any{
		"project": " lumen ", "limit": float64(5),
	}}})
	if len(denied) != 0 {
		t.Fatalf("合法调用不应被拦截，实际 %+v", denied)
	}
	if len(allowed) != 1 {
		t.Fatalf("应放行 1 个调用，实际 %d", len(allowed))
	}
	if allowed[0].Args.String("project") != "lumen" {
		t.Fatalf("字符串参数应去空格，实际 %v", allowed[0].Args["project"])
	}
	if allowed[0].Args.Int("limit") != 5 {
		t.Fatalf("JSON 数字应转成 int，实际 %#v", allowed[0].Args["limit"])
	}
}

// TestGateRejectsBadArgValues 覆盖各类非法取值。
func TestGateRejectsBadArgValues(t *testing.T) {
	spec := readSpec("get_sessions",
		Param{Name: "date", Type: ParamString, MaxLength: 10, Description: "日期"},
		Param{Name: "limit", Type: ParamInteger, HasRange: true, Min: 1, Max: 30, Description: "上限"},
		Param{Name: "mode", Type: ParamString, MaxLength: 8, Enum: []string{"a", "b"}, Description: "模式"},
		Param{Name: "tags", Type: ParamStringArray, MaxLength: 16, MaxItems: 3, Description: "标签"},
	)
	r, _ := NewRegistry(&stubTool{spec: spec})
	gate := NewPolicyGate(r)

	cases := map[string]map[string]any{
		"类型错误":  {"limit": "十"},
		"小数当整数": {"limit": 1.5},
		"超出范围":  {"limit": 99},
		"低于范围":  {"limit": 0},
		"枚举外":   {"mode": "z"},
		"超长字符串": {"date": strings.Repeat("2", 11)},
		"数组类型错": {"tags": "not-array"},
		"数组过长":  {"tags": []any{"a", "b", "c", "d"}},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			allowed, denied := gate.Review([]Call{{Name: "get_sessions", Arguments: args}})
			if len(allowed) != 0 {
				t.Fatalf("非法取值不应被放行: %+v", args)
			}
			if len(denied) != 1 {
				t.Fatalf("应记录一次拒绝，实际 %+v", denied)
			}
		})
	}
}

// TestGateRejectsMissingRequiredArgument 覆盖必填参数。
func TestGateRejectsMissingRequiredArgument(t *testing.T) {
	tool := &stubTool{spec: readSpec("save_x",
		Param{Name: "content", Type: ParamString, Required: true, MinLength: 4, MaxLength: 200, Description: "内容"})}
	r, _ := NewRegistry(tool)
	gate := NewPolicyGate(r)

	allowed, denied := gate.Review([]Call{{Name: "save_x", Arguments: map[string]any{}}})
	if len(allowed) != 0 || len(denied) != 1 {
		t.Fatalf("缺少必填参数不应放行，实际 allowed=%d denied=%+v", len(allowed), denied)
	}
	if !strings.Contains(denied[0].Reason, "必需参数") {
		t.Fatalf("拒绝原因应说明缺少必需参数，实际 %q", denied[0].Reason)
	}

	// 长度不足同样拒绝（避免"记下一条没有信息量的记忆"）。
	allowed, _ = gate.Review([]Call{{Name: "save_x", Arguments: map[string]any{"content": "ab"}}})
	if len(allowed) != 0 {
		t.Fatal("长度不足的参数不应放行")
	}
}

// TestGateRejectsHighRiskWrite 覆盖高风险写入一律拒执行。
func TestGateRejectsHighRiskWrite(t *testing.T) {
	spec := Spec{Name: "delete_everything", Summary: "危险操作",
		ResultSchema: `{"ok":true}`, Kind: KindMemoryWrite, Risk: RiskWriteHigh}
	r, _ := NewRegistry(&stubTool{spec: spec})
	gate := NewPolicyGate(r)

	allowed, denied := gate.Review([]Call{{Name: "delete_everything"}})
	if len(allowed) != 0 {
		t.Fatal("高风险写入不应被放行")
	}
	if len(denied) != 1 || !strings.Contains(denied[0].Reason, "高风险") {
		t.Fatalf("拒绝原因应说明高风险，实际 %+v", denied)
	}
}

// TestGateRejectsTooManyCalls 覆盖单轮调用数量上限。
//
// 超量时全部拒绝而不是截断执行前 N 个：截断会让模型以为拿到了完整数据。
func TestGateRejectsTooManyCalls(t *testing.T) {
	r, _ := NewRegistry(&stubTool{spec: readSpec("get_sessions")})
	gate := NewPolicyGate(r)

	calls := make([]Call, 0, 6)
	for i := 0; i < 6; i++ {
		calls = append(calls, Call{Name: "get_sessions"})
	}
	allowed, denied := gate.Review(calls)
	if len(allowed) != 0 {
		t.Fatalf("超量调用应全部拒绝，实际放行 %d 个", len(allowed))
	}
	if len(denied) != 1 {
		t.Fatalf("应记录一次超量拦截，实际 %+v", denied)
	}
}

// TestGateDeniesSafelyWithoutRegistry 覆盖注册表缺失时的安全拒绝。
//
// fail closed：装配漏了注册表只应该让 Agent 少干活，
// 绝不能变成"没有闸门就放行"，也不该 panic 把服务打崩。
func TestGateDeniesSafelyWithoutRegistry(t *testing.T) {
	gate := NewPolicyGate(nil)
	calls := []Call{
		{Name: "get_sessions", Arguments: map[string]any{"date": "2026-09-18"}},
		{Name: "run_sql", Arguments: map[string]any{"query": "SELECT 1"}},
	}

	allowed, denied := gate.Review(calls)
	if len(allowed) != 0 {
		t.Fatalf("注册表缺失时不应放行任何调用，实际放行 %d 个", len(allowed))
	}
	if len(denied) != 2 {
		t.Fatalf("应逐条记录拒绝（含模型请求过的名字），实际 %+v", denied)
	}
	for _, d := range denied {
		if d.Name == "" || d.Reason == "" {
			t.Fatalf("拒绝记录应保留工具名与原因，实际 %+v", d)
		}
	}
}

// TestNilPolicyGateDeniesSafely 覆盖闸门本身为 nil 的情况。
func TestNilPolicyGateDeniesSafely(t *testing.T) {
	var gate *PolicyGate
	allowed, denied := gate.Review([]Call{{Name: "get_sessions"}})
	if len(allowed) != 0 {
		t.Fatal("nil 闸门不应放行任何调用")
	}
	if len(denied) != 1 {
		t.Fatalf("应记录一次拒绝，实际 %+v", denied)
	}
}

// TestGateAllowsPartialApproval 覆盖部分放行。
func TestGateAllowsPartialApproval(t *testing.T) {
	r, _ := NewRegistry(&stubTool{spec: readSpec("get_sessions")})
	gate := NewPolicyGate(r)

	allowed, denied := gate.Review([]Call{
		{Name: "get_sessions"},
		{Name: "run_sql"},
	})
	if len(allowed) != 1 || allowed[0].Tool != "get_sessions" {
		t.Fatalf("应只放行 get_sessions，实际 %+v", allowed)
	}
	if len(denied) != 1 || denied[0].Name != "run_sql" {
		t.Fatalf("应只拦截 run_sql，实际 %+v", denied)
	}
}

// ---- 执行器 ----

// TestExecutorRunsReadsBeforeWrites 覆盖执行顺序与证据注入。
//
// 模型把写入排在最前面也一样安全：代码保证先读后写，
// 写入拿到的来源是本轮真实读到的记录。
func TestExecutorRunsReadsBeforeWrites(t *testing.T) {
	reader := &stubTool{
		spec: readSpec("get_sessions"),
		result: Result{Evidence: []string{"s_1", "s_2"},
			Digest: []string{"读到 2 段"}, Count: 2},
	}
	writer := &stubTool{spec: Spec{
		Name: "save_memory_candidate", Summary: "保存候选",
		ResultSchema: `{"saved":true}`, Kind: KindMemoryWrite,
		Risk: RiskWriteLow, RequiresEvidence: true, MaxPerTurn: 2,
	}, result: Result{Count: 1, Digest: []string{"已保存"}}}

	r, err := NewRegistry(reader, writer)
	if err != nil {
		t.Fatalf("构造注册表失败: %v", err)
	}
	audit := &MemorySink{}
	exec, err := NewExecutor(ExecutorOptions{Registry: r, Audit: audit})
	if err != nil {
		t.Fatalf("构造执行器失败: %v", err)
	}

	// 刻意把写入排在读之前：顺序应由代码决定，不由模型排列决定。
	results, denied := exec.Run(context.Background(), []Call{
		{Name: "save_memory_candidate", Arguments: map[string]any{}},
		{Name: "get_sessions"},
	}, runInfo())

	if len(denied) != 0 {
		t.Fatalf("不应有拒绝，实际 %+v", denied)
	}
	if len(results) != 2 {
		t.Fatalf("应返回 2 个结果，实际 %d", len(results))
	}
	if results[0].Tool != "get_sessions" || results[1].Tool != "save_memory_candidate" {
		t.Fatalf("执行顺序应为先读后写，实际 %s → %s", results[0].Tool, results[1].Tool)
	}
	if got := writer.lastInv.Evidence; len(got) != 2 || got[0] != "s_1" {
		t.Fatalf("写入应拿到本轮读取的证据，实际 %v", got)
	}
	if writer.lastInv.Actor != "u1" {
		t.Fatalf("请求者身份应由代码注入，实际 %q", writer.lastInv.Actor)
	}
}

// TestExecutorDeniesWriteWithoutEvidence 覆盖无证据写入被拒。
func TestExecutorDeniesWriteWithoutEvidence(t *testing.T) {
	writer := &stubTool{spec: Spec{
		Name: "save_memory_candidate", Summary: "保存候选",
		ResultSchema: `{"saved":true}`, Kind: KindMemoryWrite,
		Risk: RiskWriteLow, RequiresEvidence: true,
	}}

	r, _ := NewRegistry(writer)
	audit := &MemorySink{}
	exec, _ := NewExecutor(ExecutorOptions{Registry: r, Audit: audit})

	results, denied := exec.Run(context.Background(), []Call{{Name: "save_memory_candidate"}}, runInfo())
	if len(results) != 0 {
		t.Fatalf("没有证据时不应执行写入，实际 %d 个结果", len(results))
	}
	if writer.ran != 0 {
		t.Fatal("被拒的写入不应真正执行")
	}
	if len(denied) != 1 || !strings.Contains(denied[0].Reason, "来源") {
		t.Fatalf("拒绝原因应说明缺少来源，实际 %+v", denied)
	}
	// 拒绝必须留审计，"模型尝试过什么"是审计的核心线索。
	records := audit.All()
	if len(records) != 1 || records[0].Decision != DecisionDenied {
		t.Fatalf("应记录一条拒绝审计，实际 %+v", records)
	}
}

// TestExecutorEnforcesMaxPerTurn 覆盖写工具的单轮次数上限。
func TestExecutorEnforcesMaxPerTurn(t *testing.T) {
	reader := &stubTool{spec: readSpec("get_sessions"),
		result: Result{Evidence: []string{"s_1"}, Count: 1}}
	writer := &stubTool{spec: Spec{
		Name: "save_memory_candidate", Summary: "保存候选",
		ResultSchema: `{"saved":true}`, Kind: KindMemoryWrite,
		Risk: RiskWriteLow, RequiresEvidence: true, MaxPerTurn: 1,
	}, result: Result{Count: 1}}

	r, _ := NewRegistry(reader, writer)
	exec, _ := NewExecutor(ExecutorOptions{Registry: r, Audit: &MemorySink{}})

	results, denied := exec.Run(context.Background(), []Call{
		{Name: "get_sessions"},
		{Name: "save_memory_candidate"},
		{Name: "save_memory_candidate"},
	}, runInfo())

	if writer.ran != 1 {
		t.Fatalf("超过单轮上限的调用不应执行，实际执行 %d 次", writer.ran)
	}
	if len(denied) != 1 || !strings.Contains(denied[0].Reason, "最多调用") {
		t.Fatalf("应记录一次超量拒绝，实际 %+v", denied)
	}
	if len(results) != 2 {
		t.Fatalf("应有 2 个成功结果（1 读 + 1 写），实际 %d", len(results))
	}
}

// TestExecutorAuditsFailuresAndPanics 覆盖失败与 panic 都留审计且不拖垮请求。
func TestExecutorAuditsFailuresAndPanics(t *testing.T) {
	boom := &stubTool{spec: readSpec("get_sessions"), err: errors.New("数据库连接中断")}
	panicky := &stubTool{spec: readSpec("get_today_status")}
	panicky.spec.Name = "get_today_status"

	r, _ := NewRegistry(boom, &panicTool{spec: readSpec("get_known_projects")})
	audit := &MemorySink{}
	logger := testLogger()
	exec, _ := NewExecutor(ExecutorOptions{Registry: r, Audit: audit, Logger: logger})

	_, denied := exec.Run(context.Background(), []Call{
		{Name: "get_sessions"}, {Name: "get_known_projects"},
	}, runInfo())

	if len(denied) != 2 {
		t.Fatalf("失败与 panic 都应成为被拒步骤，实际 %+v", denied)
	}
	// 失败原因不能把内部错误细节直接送给模型与用户。
	for _, d := range denied {
		if strings.Contains(d.Reason, "连接中断") {
			t.Fatalf("对外原因不应包含内部错误细节，实际 %q", d.Reason)
		}
	}
	records := audit.All()
	if len(records) != 2 {
		t.Fatalf("两次失败都应留审计，实际 %d 条", len(records))
	}
	for _, rec := range records {
		if rec.Decision != DecisionError {
			t.Fatalf("审计决策应为 error，实际 %+v", rec)
		}
	}
}

// TestExecutorAuditsWriteSources 覆盖"写入的审计要能回答依据是什么"。
//
// 写工具的回填结果里带着它实际挂上的来源，审计必须记下条数——
// 否则事后无法回答"这条记忆是凭什么写下的"。
func TestExecutorAuditsWriteSources(t *testing.T) {
	reader := &stubTool{spec: readSpec("get_today_status"),
		result: Result{Evidence: []string{"s_1", "s_2"}, Count: 2}}
	writer := &stubTool{spec: Spec{
		Name: "save_memory_candidate", Summary: "保存候选",
		ResultSchema: `{"saved":true}`, Kind: KindMemoryWrite,
		Risk: RiskWriteLow, RequiresEvidence: true,
	}, result: Result{Count: 1, Evidence: []string{"s_1", "s_2"}}}

	r, _ := NewRegistry(reader, writer)
	audit := &MemorySink{}
	exec, _ := NewExecutor(ExecutorOptions{Registry: r, Audit: audit})

	if _, denied := exec.Run(context.Background(), []Call{
		{Name: "get_today_status"}, {Name: "save_memory_candidate"},
	}, runInfo()); len(denied) != 0 {
		t.Fatalf("调用应被放行，实际 %+v", denied)
	}

	var writeAudit *AuditRecord
	for i, rec := range audit.All() {
		if rec.Tool == "save_memory_candidate" {
			writeAudit = &audit.All()[i]
		}
	}
	if writeAudit == nil {
		t.Fatalf("写入应留审计，实际 %+v", audit.All())
	}
	if writeAudit.EvidenceN != 2 {
		t.Fatalf("写入的审计应记录来源条数，实际 %d", writeAudit.EvidenceN)
	}
	if writeAudit.Risk != RiskWriteLow {
		t.Fatalf("写入的审计应记录风险级别，实际 %q", writeAudit.Risk)
	}
	if writeAudit.ResultKind != KindMemoryWrite {
		t.Fatalf("写入的审计应记录结果类别，实际 %q", writeAudit.ResultKind)
	}
}

// TestExecutorAuditFailureDoesNotBreakRun 覆盖审计失败不影响回答。
func TestExecutorAuditFailureDoesNotBreakRun(t *testing.T) {
	tool := &stubTool{spec: readSpec("get_sessions"), result: Result{Count: 1}}
	r, _ := NewRegistry(tool)
	audit := &MemorySink{Err: errors.New("审计表写不进去")}
	exec, _ := NewExecutor(ExecutorOptions{Registry: r, Audit: audit, Logger: testLogger()})

	results, denied := exec.Run(context.Background(), []Call{{Name: "get_sessions"}}, runInfo())
	if len(denied) != 0 {
		t.Fatalf("审计失败不应变成执行失败，实际 %+v", denied)
	}
	if len(results) != 1 {
		t.Fatalf("应正常返回结果，实际 %d 个", len(results))
	}
}

// TestExecutorRequiresTemporal 覆盖"缺少可信时间 fail-closed"。
//
// 这是"任何一层都不自己取 time.Now"的结构保证：装配漏了时间，
// 结果只是诚实的拒绝，而不是悄悄用各自的本地时钟。
func TestExecutorRequiresTemporal(t *testing.T) {
	tool := &stubTool{spec: readSpec("get_sessions"), result: Result{Count: 1}}
	r, _ := NewRegistry(tool)
	exec, _ := NewExecutor(ExecutorOptions{Registry: r, Audit: &MemorySink{}})

	results, denied := exec.Run(context.Background(), []Call{{Name: "get_sessions"}}, RunInfo{Actor: "u1"})
	if len(results) != 0 {
		t.Fatal("缺少可信时间时不应执行任何工具")
	}
	if tool.ran != 0 {
		t.Fatal("缺少可信时间时工具不应被执行")
	}
	if len(denied) != 1 || !strings.Contains(denied[0].Reason, "可信时间") {
		t.Fatalf("应记录一次拒绝并说明原因，实际 %+v", denied)
	}
}

// TestExecutorPassesTemporalToTools 覆盖 Invocation 携带同一份可信时间。
func TestExecutorPassesTemporalToTools(t *testing.T) {
	tool := &stubTool{spec: readSpec("get_current_time")}
	r, _ := NewRegistry(tool)
	exec, _ := NewExecutor(ExecutorOptions{Registry: r, Audit: &MemorySink{}})

	info := runInfo()
	if _, denied := exec.Run(context.Background(), []Call{{Name: "get_current_time"}}, info); len(denied) != 0 {
		t.Fatalf("不应有拒绝: %+v", denied)
	}
	if !tool.lastInv.Temporal.Valid() {
		t.Fatal("工具应拿到可信时间")
	}
	if !tool.lastInv.Temporal.Now.Equal(info.Temporal.Now) {
		t.Fatal("工具拿到的应是同一份时间快照")
	}
}

// TestExecutorPassesTurnSourceToWrites 覆盖 TurnSource 只由代码注入。
func TestExecutorPassesTurnSourceToWrites(t *testing.T) {
	reader := &stubTool{spec: readSpec("get_today_status"), result: Result{Evidence: []string{"s_1"}}}
	writer := &stubTool{spec: Spec{
		Name: "save_memory_candidate", Summary: "保存候选",
		ResultSchema: `{"saved":true}`, Kind: KindMemoryWrite,
		Risk: RiskWriteLow, RequiresEvidence: true,
	}, result: Result{Count: 1}}
	r, _ := NewRegistry(reader, writer)
	exec, _ := NewExecutor(ExecutorOptions{Registry: r, Audit: &MemorySink{}})

	info := runInfo()
	info.TurnSource = "u_test_001"
	if _, denied := exec.Run(context.Background(), []Call{
		{Name: "get_today_status"}, {Name: "save_memory_candidate"},
	}, info); len(denied) != 0 {
		t.Fatalf("不应有拒绝: %+v", denied)
	}
	if writer.lastInv.TurnSource != "u_test_001" {
		t.Fatalf("写工具应拿到代码生成的 TurnSource，实际 %q", writer.lastInv.TurnSource)
	}
}

// TestExecutorAuditClipsArgs 覆盖审计里的参数被裁短。
//
// 审计要长期保存，不能因为某个参数很长就把大段用户内容写进去。
func TestExecutorAuditClipsArgs(t *testing.T) {
	tool := &stubTool{spec: readSpec("get_sessions",
		Param{Name: "project", Type: ParamString, MaxLength: 200, Description: "项目名"})}
	r, _ := NewRegistry(tool)
	audit := &MemorySink{}
	exec, _ := NewExecutor(ExecutorOptions{Registry: r, Audit: audit})

	long := strings.Repeat("项", 120)
	if _, denied := exec.Run(context.Background(),
		[]Call{{Name: "get_sessions", Arguments: map[string]any{"project": long}}}, runInfo()); len(denied) != 0 {
		t.Fatalf("调用应被放行，实际 %+v", denied)
	}

	records := audit.All()
	if len(records) != 1 {
		t.Fatalf("应有一条审计，实际 %d 条", len(records))
	}
	got, _ := records[0].Args["project"].(string)
	if len([]rune(got)) > 70 {
		t.Fatalf("审计里的参数应被裁短，实际 %d 字", len([]rune(got)))
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("裁短处应显式标注，实际 %q", got)
	}
}

// TestNilExecutorDeniesSafely 覆盖执行器为 nil 时的安全拒绝。
func TestNilExecutorDeniesSafely(t *testing.T) {
	var exec *Executor
	results, denied := exec.Run(context.Background(), []Call{{Name: "get_sessions"}}, runInfo())
	if len(results) != 0 {
		t.Fatal("nil 执行器不应返回结果")
	}
	if len(denied) != 1 {
		t.Fatalf("应记录一次拒绝，实际 %+v", denied)
	}
	if exec.Catalog() != "" {
		t.Fatal("nil 执行器的目录应为空")
	}
	if exec.Registry() != nil {
		t.Fatal("nil 执行器不应返回注册表")
	}
}

// TestNewExecutorRequiresRegistry 覆盖装配检查。
func TestNewExecutorRequiresRegistry(t *testing.T) {
	if _, err := NewExecutor(ExecutorOptions{}); err == nil {
		t.Fatal("缺少注册表的执行器必须构造失败")
	}
}

// TestMergeSpans 覆盖区间合并（无序、跨天、空输入）。
func TestMergeSpans(t *testing.T) {
	at := func(h, m int) time.Time {
		return time.Date(2026, 9, 17, h, m, 0, 0, time.UTC)
	}

	// 无序输入：最早开始与最晚结束都在中间。
	got := MergeSpans(
		Span{Start: at(14, 0), End: at(15, 30)},
		Span{Start: at(9, 0), End: at(10, 15)},
		Span{Start: at(19, 45), End: at(22, 10)},
	)
	if !got.Start.Equal(at(9, 0)) || !got.End.Equal(at(22, 10)) {
		t.Fatalf("应取最早开始 ~ 最晚结束，实际 %v ~ %v", got.Start, got.End)
	}

	// 跨天：结束在次日。
	overnight := MergeSpans(
		Span{Start: at(23, 30), End: at(23, 30).AddDate(0, 0, 1)},
		Span{Start: at(9, 0), End: at(10, 0)},
	)
	if !overnight.End.Equal(at(23, 30).AddDate(0, 0, 1)) {
		t.Fatalf("跨天区间应取最晚结束，实际 %v", overnight.End)
	}

	// 空输入与非法区间。
	if MergeSpans().Valid() {
		t.Fatal("空输入不应产生有效区间")
	}
	if MergeSpans(Span{Start: at(10, 0), End: at(9, 0)}).Valid() {
		t.Fatal("结束早于开始的区间应无效")
	}
}

// TestDenyHelpers 覆盖工具级拒绝的构造与识别。
func TestDenyHelpers(t *testing.T) {
	err := Deny("缺少 %s", "来源")
	reason, ok := AsDenial(err)
	if !ok || reason != "缺少 来源" {
		t.Fatalf("应识别工具级拒绝，实际 ok=%v reason=%q", ok, reason)
	}
	if _, ok := AsDenial(errors.New("普通错误")); ok {
		t.Fatal("普通错误不应被当成工具级拒绝")
	}
	if _, ok := AsDenial(nil); ok {
		t.Fatal("nil 不应被当成工具级拒绝")
	}
	// 包装后仍应识别（errors.As 的语义）。
	wrapped := errors.New("外层")
	if _, ok := AsDenial(wrapped); ok {
		t.Fatal("普通错误包装不应被误判")
	}
}

// TestArgsAccessors 覆盖参数读取的零值与默认值语义。
func TestArgsAccessors(t *testing.T) {
	args := Args{"s": "x", "n": 3, "f": 0.5, "list": []string{"a"}}

	if args.String("s") != "x" || args.String("missing") != "" {
		t.Fatal("String 应返回字符串或空串")
	}
	if args.Int("n") != 3 || args.Int("missing") != 0 {
		t.Fatal("Int 应返回整数或 0")
	}
	if args.IntOr("missing", 7) != 7 || args.IntOr("n", 7) != 3 {
		t.Fatal("IntOr 应返回默认值或实际值")
	}
	if args.Number("f") != 0.5 || args.Number("missing") != 0 {
		t.Fatal("Number 应返回数值或 0")
	}
	if got := args.Strings("list"); len(got) != 1 || got[0] != "a" {
		t.Fatalf("Strings 应返回切片，实际 %v", got)
	}
	if args.Strings("missing") != nil {
		t.Fatal("Strings 缺失时应返回 nil")
	}
	if !args.Has("s") || args.Has("missing") {
		t.Fatal("Has 应正确判断存在性")
	}
}

// TestRiskAndTypeLabels 覆盖面向模型的中文标签不缺项。
func TestRiskAndTypeLabels(t *testing.T) {
	for _, risk := range []RiskLevel{RiskRead, RiskWriteLow, RiskWriteHigh, "unknown"} {
		if risk.Label() == "" {
			t.Fatalf("风险级别 %q 缺少标签", risk)
		}
	}
	for _, typ := range []ParamType{ParamString, ParamInteger, ParamNumber, ParamStringArray, "unknown"} {
		if typ.Label() == "" {
			t.Fatalf("参数类型 %q 缺少标签", typ)
		}
	}
}

// panicTool 在 Execute 里 panic，用于验证执行器的收敛行为。
type panicTool struct{ spec Spec }

func (p *panicTool) Spec() Spec { return p.spec }

func (p *panicTool) Execute(context.Context, Invocation) (Result, error) {
	panic("工具内部越界")
}
