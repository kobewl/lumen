package tools

import (
	"time"

	"lumen/server/internal/identity"
	"lumen/server/internal/tooling"
)

// Options 是构造首批工具的依赖。
//
// 全部显式注入（存储、身份、时区）：工具不自己读全局状态，也**不持有时钟**——
// 当前时间在每次调用时经 Invocation.Temporal 由执行器注入（同一轮同一份），
// 因此不存在"某个工具自己取 time.Now"的口子。
type Options struct {
	// Store 是记录读取与候选记忆写入的存储。
	Store Store
	// Profile 是助手身份（get_assistant_profile 的数据源）。
	Profile identity.Profile
	// Location 是展示与查询用的时区。
	Location *time.Location
	// KnownProjectsDays 是 get_known_projects 的默认回看天数（默认 14）。
	KnownProjectsDays int
}

// All 返回第一批工具：七读 + 一写（低风险）。
//
// 顺序即提示词目录里的顺序，固定下来便于复现验收时的工具清单。
//
//	读：当前时间与时段 / 助手身份 / 对话状态 / 今天活动摘要 /
//	    活动时段查询 / 项目清单 / Agent 任务摘要
//	写：保存记忆候选（只写候选，必须带来源证据）
func All(opts Options) []tooling.Tool {
	loc := opts.Location
	if loc == nil {
		loc = time.UTC
	}
	return []tooling.Tool{
		&currentTimeTool{},
		&assistantProfileTool{profile: opts.Profile},
		&conversationStateTool{store: opts.Store},
		&todayStatusTool{store: opts.Store},
		&sessionsTool{store: opts.Store, maxLimit: maxSessionsPerView},
		&knownProjectsTool{store: opts.Store, days: opts.KnownProjectsDays},
		&taskSummariesTool{store: opts.Store, loc: loc, maxLimit: 50},
		&saveMemoryCandidateTool{store: opts.Store},
	}
}
