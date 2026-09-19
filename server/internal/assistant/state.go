package assistant

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lumen/server/internal/temporal"
	"lumen/server/internal/tooling"
)

// StateView 已迁往 contextassembler.ConversationSummary：
// 会话摘要属于"送进模型的上下文"，由装配器统一渲染（bundle.RenderTrusted）。
// 本文件只保留模型完全不可用时的最小兜底。

// MinimalFallback 是模型完全不可用时的最小安全兜底。
//
// 它的定位被严格限制：**不是**产品入口，也**不是**另一套关键词机器人。
// 它只做一件最保守的事——把用户最明确、最不可能误判的几种"要数据"的说法
// 映射到一次只读工具调用，其余一律诚实说明模型不可用。
//
// 之所以保留这层：模型不可用时如果完全答不了"今天做了什么"，
// 用户会以为记录丢了。宁可给一份确定性的事实摘要，也不要让人怀疑数据。
//
// 注意它**没有**绕过工具层：它走的是同一个执行器（因此照样过策略、
// 照样留审计），只是不经过模型的语义判断。
type MinimalFallback struct {
	Executor *tooling.Executor
	Loc      *time.Location
}

// CanHandle 判断这句话是否属于"最明确的数据请求"。
func (f *MinimalFallback) CanHandle(text string) bool {
	if f == nil || f.Executor == nil {
		return false
	}
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	// 只认最直白的说法，宁可不匹配也不要误判。
	for _, kw := range []string{
		"今天做了什么", "今天干了什么", "今天怎么样",
		"昨天做了什么", "昨天干了什么",
	} {
		if strings.Contains(t, kw) {
			return true
		}
	}
	return false
}

// Handle 通过工具执行器取今天的活动摘要，并渲染成确定性事实。
//
// tc 必须由调用方传入（与 Planner/Synthesizer 同一份可信时间）：
// 兜底同样不允许自己取 time.Now——执行器对缺少可信时间 fail-closed，
// 兜底拿不到时间时会诚实报错，而不是悄悄换个时钟。
func (f *MinimalFallback) Handle(ctx context.Context, tc temporal.Context) (string, error) {
	if f == nil || f.Executor == nil {
		return "", fmt.Errorf("兜底执行器未配置")
	}
	results, denied := f.Executor.Run(ctx, []tooling.Call{
		{Name: "get_today_status", Arguments: map[string]any{}},
	}, tooling.RunInfo{Actor: "fallback", Temporal: tc})

	body := renderFacts(results)
	if body == "" {
		date := tc.Date
		if date == "" {
			date = time.Now().In(locationOr(f.Loc)).Format("2006-01-02")
		}
		body = fmt.Sprintf("（%s）我这边没有查到记录。", date)
		if len(denied) > 0 {
			body = "我现在连接不上自己的模型，也暂时取不到记录，稍后再试试。"
		}
	}
	if line := evidenceLine(results, f.Loc); line != "" {
		body += "\n\n" + line
	}
	return body, nil
}

func locationOr(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}
