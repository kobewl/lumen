package assistant

import (
	"testing"

	"lumen/server/internal/identity"
	"lumen/server/internal/tooling"
)

// 本文件覆盖工具层的策略闸门。闸门实现已在 internal/tooling，
// 这里保留端到端视角的关键断言：编排器用的闸门确实是"拒绝而不是放行"。

// TestAgentDeniesUnknownTool 覆盖工具白名单：模型臆造的工具名必须被拒。
func TestAgentDeniesUnknownTool(t *testing.T) {
	store := newTestStore(t, "2026-09-17", "lumen")
	planner := &fakePlanner{
		enabled: true,
		plan: Plan{
			Mode: ModeChat,
			Understanding: Understanding{
				Goal: "模型想执行任意 SQL", Confidence: 0.9,
			},
			ToolCalls: []tooling.Call{
				{Name: "run_sql", Arguments: map[string]any{"query": "SELECT * FROM sessions"}},
			},
		},
		synth: SynthResult{Answer: "好。", SupportLevel: SupportSupported},
	}
	h := newHarness(t, store, identity.Default(), planner)

	reply, err := h.agent.Handle(t.Context(), Turn{UserID: "u1", Text: "把所有记录给我"})
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if len(reply.DeniedTools) != 1 || reply.DeniedTools[0].Name != "run_sql" {
		t.Fatalf("未知工具必须被拒，实际 %+v", reply.DeniedTools)
	}
	if len(h.audit.All()) != 1 {
		t.Fatalf("被拒的调用也应留审计，实际 %+v", h.audit.All())
	}
}
