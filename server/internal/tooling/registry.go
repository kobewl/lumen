package tooling

import (
	"fmt"
	"strings"
)

// Registry 持有允许被调用的全部工具。
//
// 它同时是**模型看到的工具目录**：模型只能从这里列出的工具里选，
// 代码也只认这里的名字。因此"新增一种能力"必须显式注册，
// 不可能靠模型临时发明，也不可能靠某个 if 分支偷偷放行。
type Registry struct {
	order  []string
	byName map[string]Tool
	specs  map[string]Spec
}

// NewRegistry 用给定工具构建注册表。
//
// 构造即校验：声明不合法或重名的工具会让构造失败（调用方应让启动失败），
// 而不是留到某次线上调用才暴露成"模型调用被莫名拒绝"。
func NewRegistry(tools ...Tool) (*Registry, error) {
	r := &Registry{
		byName: make(map[string]Tool, len(tools)),
		specs:  make(map[string]Spec, len(tools)),
	}
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		spec := tool.Spec()
		if err := validateSpec(spec); err != nil {
			return nil, err
		}
		if _, dup := r.byName[spec.Name]; dup {
			// 重名会让"模型调用了哪个工具"变得不确定，必须拒绝装配。
			return nil, fmt.Errorf("工具 %s 重复注册", spec.Name)
		}
		r.byName[spec.Name] = tool
		r.specs[spec.Name] = spec
		r.order = append(r.order, spec.Name)
	}
	return r, nil
}

// Get 按名字取工具。
//
// nil 注册表返回"未命中"而不是 panic：注册表缺失是装配问题，
// 调用方的正确反应是拒绝这次调用，不是把服务弄崩。
func (r *Registry) Get(name string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	t, ok := r.byName[name]
	return t, ok
}

// Spec 按名字取工具声明。
func (r *Registry) Spec(name string) (Spec, bool) {
	if r == nil {
		return Spec{}, false
	}
	s, ok := r.specs[name]
	return s, ok
}

// Names 返回全部工具名（注册顺序，便于测试与提示词复现）。
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Specs 返回全部工具声明（注册顺序）。
func (r *Registry) Specs() []Spec {
	if r == nil {
		return nil
	}
	out := make([]Spec, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.specs[name])
	}
	return out
}

// Len 返回注册的工具数量。
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.order)
}

// Catalog 渲染给模型看的工具目录：名称、风险、说明、参数 schema、返回 schema。
//
// 顺序固定（注册顺序），因此在同样的注册表上，提示词完全可复现——
// 验收时"模型看到的能力清单"不会每次不一样。
func (r *Registry) Catalog() string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, name := range r.order {
		spec := r.specs[name]
		fmt.Fprintf(&b, "- %s（%s）：%s\n", spec.Name, spec.Risk.Label(), spec.Summary)
		if len(spec.Parameters) == 0 {
			b.WriteString("  参数：无\n")
		} else {
			b.WriteString("  参数：\n")
			for _, p := range spec.Parameters {
				fmt.Fprintf(&b, "    · %s：%s —— %s\n", p.Name, describeParam(p), p.Description)
			}
		}
		if spec.ResultSchema != "" {
			fmt.Fprintf(&b, "  返回：%s\n", spec.ResultSchema)
		}
	}
	return b.String()
}
