package tooling

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 本文件是"参数与声明"的唯一校验点：工具的声明是否自洽、模型的参数是否合规，
// 都在这里判断。校验失败一律返回**可读的中文原因**——它会被写进审计，
// 也会作为 <denied_steps> 告诉模型"这一步为什么没做成"。

// maxParamValueRunes 是单个字符串参数的绝对上限（与 schema 里声明的上限取小）。
//
// 它是防御性的第二道：schema 已经声明了每个参数的长度，但参数校验代码本身
// 也不能假设"schema 一定写全了"。
const maxParamValueRunes = 256

// validToolName 判断工具名是否合法：小写字母/数字/下划线，且不以数字开头。
func validToolName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// validParamName 与工具名同样规则。
func validParamName(name string) bool { return validToolName(name) }

// validateSpec 校验一个工具的声明是否自洽。
//
// 注册表构造期就会调用它：声明写错的工具会让启动直接失败，
// 而不是等到某次线上调用才暴露成"模型调用被莫名其妙拒绝"。
func validateSpec(spec Spec) error {
	if !validToolName(spec.Name) {
		return fmt.Errorf("工具名 %q 不合法（只允许小写字母、数字与下划线，且不以数字开头）", spec.Name)
	}
	if strings.TrimSpace(spec.Summary) == "" {
		return fmt.Errorf("工具 %s 缺少说明", spec.Name)
	}
	switch spec.Risk {
	case RiskRead, RiskWriteLow, RiskWriteHigh:
	default:
		return fmt.Errorf("工具 %s 的风险级别 %q 未知", spec.Name, spec.Risk)
	}
	switch spec.Kind {
	case KindSystemInfo, KindReference, KindActivity, KindReportedTasks, KindMemoryWrite:
	default:
		return fmt.Errorf("工具 %s 的结果类别 %q 未知", spec.Name, spec.Kind)
	}
	if strings.TrimSpace(spec.ResultSchema) == "" {
		return fmt.Errorf("工具 %s 缺少结果 schema", spec.Name)
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(spec.ResultSchema), &probe); err != nil {
		return fmt.Errorf("工具 %s 的结果 schema 不是合法 JSON 对象: %v", spec.Name, err)
	}
	if spec.RequiresEvidence && spec.Risk == RiskRead {
		return fmt.Errorf("工具 %s 是只读工具，不该要求来源证据", spec.Name)
	}
	if spec.MaxPerTurn < 0 {
		return fmt.Errorf("工具 %s 的单轮次数上限不能为负", spec.Name)
	}
	if spec.MaxPerTurn > 0 && spec.Risk == RiskRead {
		// 只读工具重复调用同一个查询只是浪费预算，不需要额外规则去限制；
		// 这条检查是为了让"次数上限"只出现在它真正有意义的地方。
		return fmt.Errorf("工具 %s 是只读工具，不该声明单轮次数上限", spec.Name)
	}

	seen := make(map[string]bool, len(spec.Parameters))
	for _, p := range spec.Parameters {
		if err := validateParam(spec.Name, p); err != nil {
			return err
		}
		if seen[p.Name] {
			return fmt.Errorf("工具 %s 的参数 %q 重复声明", spec.Name, p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// validateParam 校验单个参数声明。
func validateParam(tool string, p Param) error {
	if !validParamName(p.Name) {
		return fmt.Errorf("工具 %s 的参数名 %q 不合法", tool, p.Name)
	}
	if strings.TrimSpace(p.Description) == "" {
		return fmt.Errorf("工具 %s 的参数 %q 缺少说明", tool, p.Name)
	}
	switch p.Type {
	case ParamString:
		if len(p.Enum) > 0 && p.MaxLength == 0 {
			return fmt.Errorf("工具 %s 的参数 %q 声明了枚举但没限制长度", tool, p.Name)
		}
	case ParamInteger, ParamNumber:
		if len(p.Enum) > 0 {
			return fmt.Errorf("工具 %s 的参数 %q 是数值，不能用枚举限制", tool, p.Name)
		}
	case ParamStringArray:
		if p.MaxLength == 0 {
			return fmt.Errorf("工具 %s 的参数 %q 是数组，必须声明单元素长度上限", tool, p.Name)
		}
	default:
		return fmt.Errorf("工具 %s 的参数 %q 类型 %q 未知", tool, p.Name, p.Type)
	}
	if p.HasRange && p.Min > p.Max {
		return fmt.Errorf("工具 %s 的参数 %q 取值范围颠倒（min > max）", tool, p.Name)
	}
	return nil
}

// validateArgs 按 schema 校验并归一模型给的参数。
//
// 返回的 Args 只包含 schema 声明过的键。未声明的参数**拒绝整条调用**，
// 而不是丢掉参数后照常执行：丢参数会让模型以为自己的意图被满足了。
//
// 第二个返回值是拒绝原因（空串表示通过）。
func validateArgs(params []Param, raw map[string]any) (Args, string) {
	byName := make(map[string]Param, len(params))
	for _, p := range params {
		byName[p.Name] = p
	}

	clean := make(Args, len(raw))
	for key, value := range raw {
		p, declared := byName[key]
		if !declared {
			return nil, fmt.Sprintf("不支持参数 %q", key)
		}
		normalized, reason := normalizeValue(value, p)
		if reason != "" {
			return nil, fmt.Sprintf("参数 %q %s", key, reason)
		}
		clean[key] = normalized
	}

	for _, p := range params {
		if p.Required && !clean.Has(p.Name) {
			return nil, fmt.Sprintf("缺少必需参数 %q", p.Name)
		}
	}
	return clean, ""
}

// normalizeValue 把参数归一到声明的类型，并检查取值范围。
func normalizeValue(value any, p Param) (any, string) {
	switch p.Type {
	case ParamString:
		s, ok := value.(string)
		if !ok {
			return nil, "应为字符串"
		}
		s = strings.TrimSpace(s)
		if reason := checkString(s, p); reason != "" {
			return nil, reason
		}
		return s, ""

	case ParamInteger:
		// JSON 解出来的数字统一是 float64；只接受整数值。
		f, reason := toNumber(value)
		if reason != "" {
			return nil, reason
		}
		if f != float64(int(f)) {
			return nil, "应为整数"
		}
		if reason := checkRange(f, p); reason != "" {
			return nil, reason
		}
		return int(f), ""

	case ParamNumber:
		f, reason := toNumber(value)
		if reason != "" {
			return nil, reason
		}
		if reason := checkRange(f, p); reason != "" {
			return nil, reason
		}
		return f, ""

	case ParamStringArray:
		raw, ok := value.([]any)
		if !ok {
			return nil, "应为字符串数组"
		}
		if p.MaxItems > 0 && len(raw) > p.MaxItems {
			return nil, fmt.Sprintf("最多 %d 项", p.MaxItems)
		}
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			s, ok := item.(string)
			if !ok {
				return nil, "应为字符串数组"
			}
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if reason := checkString(s, p); reason != "" {
				return nil, reason
			}
			out = append(out, s)
		}
		return out, ""
	}
	return nil, "类型未知"
}

func toNumber(value any) (float64, string) {
	switch v := value.(type) {
	case float64:
		return v, ""
	case int:
		return float64(v), ""
	default:
		return 0, "应为数值"
	}
}

// checkString 检查字符串的长度与枚举。
func checkString(s string, p Param) string {
	n := len([]rune(s))
	limit := p.MaxLength
	if limit <= 0 || limit > maxParamValueRunes {
		limit = maxParamValueRunes
	}
	if n > limit {
		return fmt.Sprintf("长度超过 %d", limit)
	}
	if p.MinLength > 0 && n < p.MinLength {
		return fmt.Sprintf("长度不足 %d", p.MinLength)
	}
	if len(p.Enum) > 0 {
		for _, e := range p.Enum {
			if s == e {
				return ""
			}
		}
		return "取值不在允许范围内"
	}
	return ""
}

// checkRange 检查数值范围（仅在声明了范围时）。
func checkRange(f float64, p Param) string {
	if !p.HasRange {
		return ""
	}
	if f < p.Min || f > p.Max {
		return fmt.Sprintf("应在 %g 到 %g 之间", p.Min, p.Max)
	}
	return ""
}

// describeParam 渲染参数约束，供工具目录使用。
func describeParam(p Param) string {
	parts := []string{p.Type.Label()}
	if p.Required {
		parts = append(parts, "必填")
	} else {
		parts = append(parts, "可选")
	}
	switch p.Type {
	case ParamString:
		if len(p.Enum) > 0 {
			parts = append(parts, "取值："+strings.Join(p.Enum, "|"))
		}
		if p.MaxLength > 0 {
			parts = append(parts, fmt.Sprintf("最长 %d 字", p.MaxLength))
		}
	case ParamInteger, ParamNumber:
		if p.HasRange {
			parts = append(parts, fmt.Sprintf("范围 %g~%g", p.Min, p.Max))
		}
	case ParamStringArray:
		if p.MaxItems > 0 {
			parts = append(parts, fmt.Sprintf("最多 %d 项", p.MaxItems))
		}
		if p.MaxLength > 0 {
			parts = append(parts, fmt.Sprintf("单元素最长 %d 字", p.MaxLength))
		}
	}
	return strings.Join(parts, "，")
}

// clipValueForAudit 把参数值裁剪到审计可接受的长度。
//
// 审计表要长期保存，不能因为某个参数很长就把大段用户内容写进去；
// 而审计需要的是"模型当时请求了什么形状的参数"，不是全文。
func clipValueForAudit(value any) any {
	const limit = 64
	switch v := value.(type) {
	case string:
		r := []rune(v)
		if len(r) <= limit {
			return v
		}
		return string(r[:limit]) + "…"
	case []string:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if r := []rune(item); len(r) > limit {
				item = string(r[:limit]) + "…"
			}
			out = append(out, item)
		}
		return out
	default:
		return value
	}
}
