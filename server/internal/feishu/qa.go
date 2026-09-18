// Package feishu 负责飞书渠道本身：长连接收消息、白名单校验、幂等、回复发送。
//
// 语义判断不在这里。这里只保留"渠道"需要的东西：展示名映射、幂等写入、
// 回复发送。用户这句话是什么意思，由 assistant.Agent 里的模型计划决定，
// 不再用正则判意图。
package feishu

// UnclassifiedProject 与 sessions.Unclassified 保持一致。
//
// 用字面量而不是 import sessions 包：sessions 已经依赖 storage 与 events，
// 这里再引入会让 feishu 包与聚合引擎互相纠缠，而这里需要的只是一个字符串。
const UnclassifiedProject = "unclassified"

// ProjectDisplayName 把内部项目标记转成用户可读文案。
//
// 内部标记（unclassified）只用于存储、日志与 API，绝不能直接出现在飞书消息里。
func ProjectDisplayName(project string) string {
	if project == UnclassifiedProject || project == "" {
		return "暂未识别项目"
	}
	return project
}
