// Package assistant 实现 AI-first 的 Agent Runtime 第一条真实链路。
//
// 与旧实现的根本区别：旧路径是「正则判意图 → Go switch → 固定业务分支 →
// 模型只做后置润色」。这里默认路径是：
//
//	用户消息 + Profile + 对话状态 + Capability 目录
//	  → 模型生成 AgentPlan（严格 JSON Schema）
//	  → 代码只做 Policy Gate（工具白名单、参数校验、预算、超时、敏感内容）
//	  → 执行受限 Capability（只读，不碰 SQL/Shell）
//	  → 结果回交模型合成 Answer（标记 supported/inferred/insufficient/conflicted）
//	  → 代码校验来源、支持等级与敏感字段
//
// 代码不再决定"用户这句话是什么意思"，只负责"模型想做的事允不允许做"。
package assistant

import (
	"fmt"
	"strings"
)

// Profile 是助手的可配置身份。
//
// 所有用户可见的身份/语气都从这里读取，任何地方都不允许硬编码助手名字：
// 换个部署环境应该只改配置就能让助手自称别的名字、说别的语言、用别的语气。
type Profile struct {
	// Name 是助手自称的名字（默认产品名 Lumen）。
	Name string
	// Role 是助手对自己的定位描述（默认「个人助手/伙伴」，不是「记录工具」）。
	Role string
	// OwnerDisplayName 是助手对用户的称呼，留空则不称呼姓名。
	OwnerDisplayName string
	// Language 是回复语言，例如 zh-CN。
	Language string
	// Tone 是语气风格，例如「友好、简洁」。
	Tone string
	// Proactivity 是主动程度，例如「低：只在被问到时回应」。
	Proactivity string
}

// DefaultProfile 返回默认身份。名字默认是产品名，但调用方可以整体替换。
func DefaultProfile() Profile {
	return Profile{
		Name:             "Lumen",
		Role:             "个人助手/伙伴",
		OwnerDisplayName: "",
		Language:         "zh-CN",
		Tone:             "友好、简洁、像朋友",
		Proactivity:      "低：只在被问到或明确需要时回应，不主动打扰",
	}
}

// Normalize 补齐空字段并在非法值上回退，保证下游拿到的 Profile 一定可用。
func (p Profile) Normalize() Profile {
	d := DefaultProfile()
	if strings.TrimSpace(p.Name) == "" {
		p.Name = d.Name
	}
	if strings.TrimSpace(p.Role) == "" {
		p.Role = d.Role
	}
	if strings.TrimSpace(p.Language) == "" {
		p.Language = d.Language
	}
	if strings.TrimSpace(p.Tone) == "" {
		p.Tone = d.Tone
	}
	if strings.TrimSpace(p.Proactivity) == "" {
		p.Proactivity = d.Proactivity
	}
	return Profile{
		Name:             strings.TrimSpace(p.Name),
		Role:             strings.TrimSpace(p.Role),
		OwnerDisplayName: strings.TrimSpace(p.OwnerDisplayName),
		Language:         strings.TrimSpace(p.Language),
		Tone:             strings.TrimSpace(p.Tone),
		Proactivity:      strings.TrimSpace(p.Proactivity),
	}
}

// AddressLine 返回助手如何称呼用户；没配置称呼时返回空串。
func (p Profile) AddressLine() string {
	if p.OwnerDisplayName == "" {
		return ""
	}
	return p.OwnerDisplayName
}

// PromptBlock 把 Profile 渲染成给模型看的事实块。
//
// 这是"身份由配置驱动"的落点：模型看到的身份描述来自这里，
// 而不是写死在系统提示里的「你叫 Lumen」。
func (p Profile) PromptBlock() string {
	var b strings.Builder
	fmt.Fprintf(&b, "- 你的名字是：%s\n", p.Name)
	fmt.Fprintf(&b, "- 你的定位是：%s（不是记录工具/记录器，记录只是手段，目的是帮用户回忆和推进事情）\n", p.Role)
	if p.OwnerDisplayName != "" {
		fmt.Fprintf(&b, "- 你称呼用户为：%s\n", p.OwnerDisplayName)
	} else {
		b.WriteString("- 用户没有配置称呼，不要自行给用户起名字\n")
	}
	fmt.Fprintf(&b, "- 回复语言：%s\n", p.Language)
	fmt.Fprintf(&b, "- 语气：%s\n", p.Tone)
	fmt.Fprintf(&b, "- 主动性：%s\n", p.Proactivity)
	return b.String()
}

// ProfileResult 是 get_assistant_profile 能力的返回结果。
type ProfileResult struct {
	Name             string `json:"name"`
	Role             string `json:"role"`
	OwnerDisplayName string `json:"owner_display_name,omitempty"`
	Language         string `json:"language"`
	Tone             string `json:"tone"`
	Proactivity      string `json:"proactivity"`
}

// AsResult 把 Profile 转成能力返回结构。
func (p Profile) AsResult() ProfileResult {
	return ProfileResult{
		Name: p.Name, Role: p.Role, OwnerDisplayName: p.OwnerDisplayName,
		Language: p.Language, Tone: p.Tone, Proactivity: p.Proactivity,
	}
}
