package initiative

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"lumen/server/internal/ai"
	"lumen/server/internal/contextassembler"
	"lumen/server/internal/identity"
	"lumen/server/internal/temporal"
)

// ProposalRequest 是模型提案的输入：全部是代码核实过的内容。
type ProposalRequest struct {
	// Temporal 是本轮可信时间（与问答链路同一来源）。
	Temporal temporal.Context
	// State 是有限对话状态（当前项目、待澄清问题）。
	State contextassembler.ConversationSummary
	// FactsJSON 是工具执行器返回的已核实事实（与问答路径同一份数据最小化约束）。
	FactsJSON string
	// EvidenceIDs 是本轮事实携带的全部可核实记录 ID。
	EvidenceIDs []string
}

// Proposal 是模型的提案：0 或 1 条关怀问题。
type Proposal struct {
	// Skip 为 true 表示模型判断此刻不该打扰。
	Skip bool
	// Reason 是跳过原因或提案依据的一句话说明（审计用）。
	Reason string
	// Question 是要发给用户的关怀问题全文。
	Question string
	// Basis 是这条问题依据的证据 ID；必须全部来自本轮事实。
	Basis []string
}

// Planner 是模型提案接口（生产实现走 ai.Client，测试注入假实现）。
type Planner interface {
	Propose(ctx context.Context, req ProposalRequest) (Proposal, error)
}

// ModelPlanner 用 ai.Client 实现提案。
type ModelPlanner struct {
	Client  *ai.Client
	Profile identity.Profile
}

// Propose 调用模型产出 0 或 1 条关怀问题。
func (p *ModelPlanner) Propose(ctx context.Context, req ProposalRequest) (Proposal, error) {
	profile := p.Profile.Normalize()
	system := fmt.Sprintf(initiativeSystemPrompt, profile.Name, profile.Role, profile.PromptBlock())
	resp, err := p.Client.CompleteJSON(ctx, system, req.FactsJSON, 600)
	if err != nil {
		return Proposal{}, err
	}
	return parseProposal(resp.Content)
}

// proposalDoc 是模型输出的严格形状。
type proposalDoc struct {
	Skip     bool     `json:"skip"`
	Reason   string   `json:"reason"`
	Question string   `json:"question"`
	Basis    []string `json:"basis"`
}

func parseProposal(content string) (Proposal, error) {
	raw, err := ai.ExtractJSON(content)
	if err != nil {
		return Proposal{}, fmt.Errorf("提案不是合法 JSON: %v", err)
	}
	var doc proposalDoc
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return Proposal{}, fmt.Errorf("提案结构不符: %v", err)
	}
	p := Proposal{Skip: doc.Skip, Reason: strings.TrimSpace(doc.Reason),
		Question: strings.TrimSpace(doc.Question), Basis: doc.Basis}
	if !p.Skip && p.Question == "" {
		return Proposal{}, fmt.Errorf("提案未跳过但问题为空")
	}
	return p, nil
}

// initiativeSystemPrompt 是提案阶段的系统提示。
//
// 与问答链路同一套价值观：只依据给定事实、区分活动记录与 Agent 报告、
// 不做开放域承诺。额外收紧的三条：0 或 1 条、必须有依据、禁止替用户做判断。
const initiativeSystemPrompt = `你是 %s（一个 %s）。现在请你判断：**此刻**要不要向用户主动说一句关怀的话。

你的身份与风格：
%s
硬性规则：
1. 最多产出 **1 条**关怀问题；没有合适的理由就产出 0 条（skip=true）。宁可不说，不说不是失职；
2. 问题必须**依据** facts 里给出的已核实事实（时段记录或 Agent 报告），并把依据的记录 ID 写进 basis。
   没有任何合适的事实依据时必须 skip，不要编话题；
3. 依据活动记录（应用名与时长）时只能问"投入"层面的问题（例如"上午在 X 上花了不少时间，进展顺利吗"），
   不能说"完成了 X"——只有 Agent 报告的任务状态才能陈述完成；
4. 不要提出心理、情绪诊断、医疗、财务方面的建议或判断；不要心灵鸡汤；
5. 用中文口语，一句话，以问号结尾，控制在 80 字以内；不要提内部 ID、内部术语；
6. <trusted_context>（如果 facts 里有）是唯一权威的时间事实，问候与时段必须与它一致。

facts 是系统给出的已核实事实 JSON（可能包含 current_time/sessions/task_summaries 等块）。
输出严格 JSON（不要 markdown 代码块、不要额外解释）：
{
  "skip": false,
  "reason": "为什么此刻说/不说的一句话说明",
  "question": "要发给用户的一句话关怀问题（skip 时留空）",
  "basis": ["依据的记录 ID"]
}`
