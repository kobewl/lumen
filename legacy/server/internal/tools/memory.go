package tools

import (
	"context"
	"strings"
	"time"

	"lumen/server/internal/profile"
	"lumen/server/internal/storage"
	"lumen/server/internal/tooling"
	"lumen/server/internal/ulid"
)

// 候选记忆的状态与类别白名单。与 storage 的取值保持一致。
const (
	memoryKindPreference = "preference"
	memoryKindProject    = "project"
	memoryKindFact       = "fact"
)

// saveMemoryCandidateTool 是唯一的**低风险写入**工具：保存一条候选记忆。
//
// 它被刻意设计成"几乎不可能造成伤害"：
//   - 只写候选状态（candidate），不会自动晋升为长期记忆；
//   - 必须带可核实的来源（RequiresEvidence），来源由执行器注入，
//     模型给不出也伪造不了；
//   - 候选内容不进入任何后续轮次的模型上下文（读取侧只读 confirmed，
//     而当前版本没有任何晋升入口）；
//   - 疑似凭证的内容直接拒绝落库。
//
// 因此它扩大的是"助手能记住什么"，而不是"助手能做什么"。
type saveMemoryCandidateTool struct {
	store Store
	// maxPerTurn 限制单轮写入条数：模型一轮里刷十条候选不是"更贴心"，
	// 而是把噪声灌进候选池，让将来真正要确认的时候更难判断。
	maxPerTurn int
}

func (t *saveMemoryCandidateTool) Spec() tooling.Spec {
	return tooling.Spec{
		Name: "save_memory_candidate",
		Summary: "保存一条**候选**记忆。它只是候选：不会自动生效，也不会进入后续对话的上下文。" +
			"只有当用户的话里明确表达了值得长期记住的内容时才调用；不确定就不要调用。" +
			"来源由系统按类别自动填入，你不需要也不允许提供：" +
			"preference（用户直接表达的偏好）挂本轮用户消息；" +
			"fact/project（来自记录的结论）挂本轮读到的记录，没有读到记录就保存不了。" +
			"preference 必须给出 key：这条偏好的稳定槽位名（如：称呼、作息、沟通风格），" +
			"用户将来纠正时，同一槽位的新确认值会替换旧值。",
		Parameters: []tooling.Param{
			{Name: "kind", Type: tooling.ParamString, Required: true, MaxLength: 16,
				Enum:        []string{memoryKindPreference, memoryKindProject, memoryKindFact},
				Description: "记忆类别：preference=偏好，project=项目相关，fact=其它事实。"},
			{Name: "key", Type: tooling.ParamString, Required: false, MaxLength: 32,
				Description: "preference 必填：偏好的稳定槽位名（如：称呼/作息/沟通风格）。" +
					"纠正确认时同一槽位只保留一个值。fact/project 可留空。"},
			{Name: "content", Type: tooling.ParamString, Required: true, MinLength: 4, MaxLength: 200,
				Description: "要记住的内容，一句话，用自己的话概括；不要包含密钥、密码、令牌等敏感信息。"},
			{Name: "confidence", Type: tooling.ParamNumber, HasRange: true, Min: 0, Max: 1,
				Description: "你对这条记忆的确信程度，0~1；用户直接说出的偏好通常在 0.7 以上。"},
		},
		ResultSchema:     `{"saved":true,"kind":"preference","key":"称呼","content":"...","source_count":1,"status":"candidate"}`,
		Kind:             tooling.KindMemoryWrite,
		Risk:             tooling.RiskWriteLow,
		RequiresEvidence: true,
		MaxPerTurn:       2,
	}
}

// MemoryCandidateResult 是 save_memory_candidate 的返回结果。
//
// 刻意不回显内部 ID：模型不需要它，用户也不该看到。
type MemoryCandidateResult struct {
	Saved       bool   `json:"saved"`
	Kind        string `json:"kind"`
	Key         string `json:"key,omitempty"`
	Content     string `json:"content"`
	SourceCount int    `json:"source_count"`
	Status      string `json:"status"`
	Note        string `json:"note,omitempty"`
}

func (t *saveMemoryCandidateTool) Execute(ctx context.Context, inv tooling.Invocation) (tooling.Result, error) {
	if t.store == nil {
		return tooling.Result{}, tooling.Deny("记忆存储未配置，无法保存")
	}
	if inv.Actor == "" {
		return tooling.Result{}, tooling.Deny("缺少请求者身份，无法保存")
	}

	kind := normalizeKind(inv.Args.String("kind"))
	content := strings.TrimSpace(inv.Args.String("content"))
	if !validMemoryContent(content) {
		// 与凭证、内部术语相关的内容绝不落库：记忆是长期保存的，
		// 写错一次比说错一句话严重得多。
		return tooling.Result{}, tooling.Deny("这条内容不适合作为记忆保存（疑似包含凭证或内部术语，或信息量不足）")
	}

	// 槽位 key（审查修复的延续）：preference 必须给出稳定槽位名，
	// 这样"用户纠正"才有落点——同一槽位的新确认值替换旧值，
	// 而不是在 Profile 里堆出两个互相矛盾的值。
	// 归一化在 profile 包：保存与确认两侧用同一个规则，key 才能对上。
	slotKey := profile.NormalizeSlotKey(inv.Args.String("key"))
	var sources []string
	switch kind {
	case memoryKindPreference:
		if slotKey == "" {
			return tooling.Result{}, tooling.Deny("preference 必须提供 key（这条偏好的稳定槽位名，如：称呼、作息、沟通风格）")
		}
		if inv.TurnSource == "" {
			return tooling.Result{}, tooling.Deny("没有本轮用户消息来源，无法保存这条偏好（偏好的依据只能是用户自己的话）")
		}
		sources = []string{inv.TurnSource}
	default:
		sources = dedupeStrings(inv.Evidence)
		if len(sources) == 0 {
			return tooling.Result{}, tooling.Deny("本轮没有读到任何记录，无法为这条内容提供依据；与记录无关的个人陈述暂不支持保存")
		}
	}

	confidence := inv.Args.Number("confidence")
	if confidence <= 0 {
		// 没给就按"模型认为值得记"的中等确信处理，但不允许默认为 1。
		confidence = 0.6
	}
	confidence = clamp01(confidence)

	candidate := storage.MemoryCandidate{
		ID:         ulid.New(),
		UserID:     inv.Actor,
		Kind:       kind,
		Content:    truncateRunes(content, 200),
		Key:        slotKey,
		SourceIDs:  sources,
		Confidence: confidence,
		// 状态永远是 candidate：这是"只写候选"的代码级保证。
		Status:    storage.MemoryStatusCandidate,
		CreatedAt: time.Now().UTC(),
	}
	if err := t.store.SaveMemoryCandidate(ctx, candidate); err != nil {
		return tooling.Result{}, err
	}

	out := MemoryCandidateResult{
		Saved: true, Kind: kind, Content: candidate.Content, Key: candidate.Key,
		SourceCount: len(sources), Status: storage.MemoryStatusCandidate,
		Note: "已保存为候选记忆，尚未生效；需要用户确认后才可能成为长期记忆。",
	}
	return tooling.Result{
		Model:  out,
		Digest: []string{"已记下一条候选（未生效）：" + candidate.Content},
		Count:  1,
		// 回填实际挂上的来源：审计据此能回答"这条记忆的依据是什么"。
		// 它不会被算进证据行——那不是活动记录，也不是 Agent 报告。
		Evidence: sources,
	}, nil
}

// normalizeKind 把类别归一到白名单取值。
func normalizeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case memoryKindPreference:
		return memoryKindPreference
	case memoryKindProject:
		return memoryKindProject
	default:
		return memoryKindFact
	}
}

// validMemoryContent 校验候选记忆内容是否可以落库。
//
// 拒绝短到没有信息量的内容，以及含内部术语或疑似凭证的内容。
func validMemoryContent(content string) bool {
	trimmed := strings.TrimSpace(content)
	if len([]rune(trimmed)) < 4 {
		return false
	}
	for _, bad := range memoryForbiddenTerms {
		if strings.Contains(trimmed, bad) {
			return false
		}
	}
	lower := strings.ToLower(trimmed)
	for _, bad := range memoryForbiddenValues {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	return true
}

// memoryForbiddenTerms 是绝不能写进记忆库的内部术语。
var memoryForbiddenTerms = []string{
	"unclassified", "session_id", "```", "s_",
}

// memoryForbiddenValues 是疑似凭证的片段（小写比较）。
var memoryForbiddenValues = []string{
	"sk-", "api key", "api_key", "token", "password", "密码", "密钥", "凭证",
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
