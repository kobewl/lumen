// Package identity 是助手身份这一领域概念的唯一落点。
//
// 它是独立的域类型（而不是塞在 assistant 包里），因为身份同时被三方使用：
//   - 提示词组装（assistant）：把身份渲染成给模型看的事实块；
//   - get_assistant_profile 工具（tools）：把身份作为可调用的数据源返回；
//   - 配置（config）：从环境变量构造身份。
//
// 三方都依赖它，而它不依赖任何一方——这样"换个部署环境只改配置就能换名字、
// 换语言、换语气"这件事，不会被任何一层的实现细节绑死。
package identity

import (
	"fmt"
	"strings"
)

// Profile 是助手的可配置身份。
//
// 所有用户可见的身份/语气都从这里读取，任何地方都不允许硬编码助手名字。
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

// Default 返回默认身份。名字默认是产品名，但调用方可以整体替换。
func Default() Profile {
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
	d := Default()
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
	return p.OwnerDisplayName
}

// PromptBlock 把身份渲染成给模型看的事实块。
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
