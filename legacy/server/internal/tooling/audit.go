package tooling

import (
	"context"
	"sync"
	"time"
)

// 审计决策取值。
const (
	// DecisionAllowed：调用通过策略校验并成功执行。
	DecisionAllowed = "allowed"
	// DecisionDenied：被拒绝（策略拦截、缺少来源证据、工具主动拒绝）。
	DecisionDenied = "denied"
	// DecisionError：执行失败（存储报错、超时等），与"按规则不该做"区分开。
	DecisionError = "error"
)

// AuditRecord 是一次工具调用的审计记录。
//
// 审计的目的是回答"这一轮到底发生了什么"，因此它必须留下：
// 谁请求的、调了什么、带什么参数、放行还是拒绝、为什么、耗时、拿到了什么；
// 同时**不留下**用户数据本身——参数值会被裁剪（clipValueForAudit），
// 结果内容一律不记（只记类别、条数与证据条数）。
type AuditRecord struct {
	// At 是记录时间（UTC）。
	At time.Time
	// Actor 是本轮请求者（用户标识）。由代码注入，不是模型参数。
	Actor string
	// Tool 是工具名。被拒的调用同样记录，因为"模型尝试过什么"本身是重要线索。
	Tool string
	// Risk 是工具风险级别（未知工具时为空）。
	Risk RiskLevel
	// Args 是清洗后的参数（只含声明过的键，值已裁短）。
	Args map[string]any
	// Decision 见上方三个常量。
	Decision string
	// Reason 是拒绝或失败的原因（放行时为空）。
	Reason string
	// DurationMS 是执行耗时（毫秒）。
	DurationMS int64
	// ResultKind 是结果类别（未被执行的调用为空）。
	ResultKind Kind
	// Count 是结果里的记录条数。
	Count int
	// EvidenceN 是结果带回的可核实记录 ID 条数。
	EvidenceN int
	// Truncated 表示结果被限额裁剪过。
	Truncated bool
}

// AuditSink 是审计写入点。
//
// 抽成接口的原因：生产环境写 SQLite，测试用内存实现断言"该记的都记了"，
// 而执行器本身不需要知道存储长什么样。
type AuditSink interface {
	Record(ctx context.Context, rec AuditRecord) error
}

// NopSink 不记录任何审计。
//
// 它只应该出现在"明确不需要审计"的场合（例如单元测试里的无关路径）；
// 生产装配用 storage 的实现。
type NopSink struct{}

// Record 实现 AuditSink。
func (NopSink) Record(context.Context, AuditRecord) error { return nil }

// MemorySink 把审计留在内存里，供测试断言。
type MemorySink struct {
	mu      sync.Mutex
	Records []AuditRecord
	// Err 非 nil 时，Record 返回它，用于验证"审计失败不影响执行"。
	Err error
}

// Record 实现 AuditSink。
func (m *MemorySink) Record(_ context.Context, rec AuditRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	m.Records = append(m.Records, rec)
	return nil
}

// All 返回已记录的审计（副本）。
func (m *MemorySink) All() []AuditRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AuditRecord, len(m.Records))
	copy(out, m.Records)
	return out
}
