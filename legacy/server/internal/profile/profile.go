// Package profile 是记忆生命周期服务的落点：候选 → 确认/拒绝 → UserProfile。
//
// 它只回答一个问题："哪条候选可以进入下一轮上下文"。规则全部由代码执行：
//   - 确认必须带明确的候选 ID（不允许"随便确认一条"）；
//   - 确认是**原子的**：候选决策、Profile 投影、版本历史收敛在 storage 的
//     单个事务里（条件状态转换），投影失败整体回滚、候选仍为 candidate，
//     并发确认只有一个生效、其余幂等返回 already_confirmed；
//   - rejected 是终态：拒绝的候选永远进不了 Profile（撤销不在本版本），
//     确认/拒绝并发互不覆盖（条件更新裁决唯一方向）；
//   - 敏感内容默认拒绝确认并记录原因（候选保持 candidate）；
//   - 只有 preference 类写入 Profile 读模型；fact/project 仅标记状态。
//
// 模型在其中的位置：模型只能**提议**（save_memory_candidate），
// 生命周期决策权全部在持有管理令牌的用户手里。
package profile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"lumen/server/internal/storage"
)

// 生命周期决策结果。
type Result struct {
	CandidateID string
	// Status 是候选的最终状态（confirmed / rejected / already_confirmed / already_rejected）。
	Status string
	// Key 与 Version 只在 preference 确认写入 Profile 时有值。
	Key     string
	Version int
	// Replaced 表示这次确认替换了同槽位的旧值（用户纠正）。
	Replaced bool
	// PreviousValue 是被替换前的旧值（纠正审计用）。
	PreviousValue string
	// Note 是给管理端看的人类可读说明。
	Note string
}

// 服务返回的语义化错误（API 层映射成 HTTP 状态码）。
var (
	// ErrNotFound：候选不存在。
	ErrNotFound = errors.New("候选记忆不存在")
	// ErrRejected：候选已被拒绝，拒绝是终态。
	ErrRejected = errors.New("候选已被拒绝，不能再确认")
	// ErrConfirmed：候选已确认；拒绝一条已确认的候选不在本版本支持范围。
	ErrConfirmed = errors.New("候选已确认；撤销确认不在当前版本支持范围")
	// ErrSensitive：内容命中敏感信息检查，拒绝确认。
	ErrSensitive = errors.New("内容疑似敏感信息，不允许确认")
)

// Service 是记忆生命周期服务。
type Service struct {
	store  *storage.Store
	logger *slog.Logger
}

// NewService 创建服务。
func NewService(store *storage.Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, logger: logger}
}

// Confirm 确认一条候选记忆。
//
// 核心状态机收敛在 storage.ConfirmMemoryCandidate 的**单个事务**里
// （条件状态转换 + Profile 投影 + 版本历史，任一步失败整体回滚）：
//   - 幂等：重复确认返回 already_confirmed，不重复计版本、不重复写历史；
//   - 并发：多个并发确认只有一个真正生效，其余拿到 already_confirmed；
//   - rejected 是终态；撤销确认不在本版本。
//
// 服务层在这里只做三件事务性的前置裁决（都不写库）：
// 敏感内容检查、类别白名单、槽位 key 归一化。候选内容不可变，
// 因此"先读后判再原子转换"没有 TOCTOU 窗口。
// preference 类写入读模型（同 key 旧值被替换并记历史）；
// fact / project 类只标记状态，不进上下文（本版本只有偏好类注入）。
func (s *Service) Confirm(ctx context.Context, candidateID, operator string) (Result, error) {
	if strings.TrimSpace(candidateID) == "" {
		return Result{}, fmt.Errorf("%w：缺少候选 ID", ErrNotFound)
	}
	c, ok, err := s.store.MemoryCandidateByID(ctx, candidateID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, ErrNotFound
	}
	switch c.Status {
	case storage.MemoryStatusConfirmed:
		return Result{CandidateID: c.ID, Status: "already_confirmed",
			Note: "该候选此前已确认，本次为重复确认（幂等，不产生新版本）"}, nil
	case storage.MemoryStatusRejected:
		return Result{}, fmt.Errorf("%w（%s）", ErrRejected, c.ID)
	}

	if reason := sensitiveReason(c.Content); reason != "" {
		// 拒绝确认但**不**改候选状态：保持 candidate，让管理端能看见
		// "有一条内容可疑的候选被拦下"，而不是悄悄吞掉。
		s.audit(ctx, "profile_confirm_refused",
			fmt.Sprintf("候选 %s（%s 类）确认被拒：%s", c.ID, c.Kind, reason))
		return Result{}, fmt.Errorf("%w：%s", ErrSensitive, reason)
	}
	if c.Kind != "preference" && c.Kind != "project" && c.Kind != "fact" {
		return Result{}, fmt.Errorf("未知候选类别 %q，不允许确认", c.Kind)
	}

	entry := storage.ProfileEntry{
		UserID:            c.UserID,
		Kind:              c.Kind,
		Value:             c.Content,
		SourceCandidateID: c.ID,
		SourceIDs:         c.SourceIDs,
	}
	eligible := c.Kind == "preference"
	if eligible {
		key := NormalizeSlotKey(c.Key)
		if key == "" {
			// 旧库候选没有 key 值：落到通用槽位，保证仍可确认。
			key = "general"
		}
		entry.Key = key
	}

	change, replaced, already, err := s.store.ConfirmMemoryCandidate(ctx, candidateID, operator, entry, eligible)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrCandidateNotFound):
			return Result{}, ErrNotFound
		case errors.Is(err, storage.ErrCandidateRejected):
			return Result{}, fmt.Errorf("%w（%s）", ErrRejected, c.ID)
		case errors.Is(err, storage.ErrCandidateConfirmed):
			return Result{}, fmt.Errorf("%w（%s）", ErrConfirmed, c.ID)
		default:
			return Result{}, err
		}
	}
	if already {
		// 并发下另一个确认先落库：同幂等路径，无新版本。
		return Result{CandidateID: c.ID, Status: "already_confirmed",
			Note: "该候选已由并发的确认生效（幂等，不产生新版本）"}, nil
	}

	out := Result{CandidateID: c.ID, Status: storage.MemoryStatusConfirmed}
	if eligible {
		out.Key = entry.Key
		out.Version = change.Version
		out.Replaced = replaced
		out.PreviousValue = change.PreviousValue
		if replaced {
			out.Note = "同一槽位已有确认值，本次为新值（用户纠正）"
		} else {
			out.Note = "已写入已确认信息，下一轮对话可见"
		}
	} else {
		out.Note = "非偏好类候选：仅标记确认，不进入对话上下文"
	}

	s.audit(ctx, "memory_confirmed",
		fmt.Sprintf("候选 %s 已确认（%s 类，槽位 %q，版本 %d）", c.ID, c.Kind, out.Key, out.Version))
	return out, nil
}

// Reject 拒绝一条候选记忆。拒绝的候选不产生任何 Profile 投影。
//
// 状态转换收敛在 storage.RejectMemoryCandidate 的条件更新里：
// 只有 candidate 可拒绝；确认/拒绝并发只有一个方向生效。
// 幂等：重复拒绝返回 already_rejected。拒绝已确认的候选返回 ErrConfirmed
// （撤销确认会牵涉"下一轮注入什么"的一致性，本版本不做）。
func (s *Service) Reject(ctx context.Context, candidateID, operator, reason string) (Result, error) {
	if strings.TrimSpace(candidateID) == "" {
		return Result{}, fmt.Errorf("%w：缺少候选 ID", ErrNotFound)
	}
	c, ok, err := s.store.MemoryCandidateByID(ctx, candidateID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, ErrNotFound
	}
	switch c.Status {
	case storage.MemoryStatusRejected:
		return Result{CandidateID: c.ID, Status: "already_rejected",
			Note: "该候选此前已拒绝（幂等）"}, nil
	case storage.MemoryStatusConfirmed:
		return Result{}, fmt.Errorf("%w（%s）", ErrConfirmed, c.ID)
	}

	already, err := s.store.RejectMemoryCandidate(ctx, candidateID, operator, strings.TrimSpace(reason))
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrCandidateNotFound):
			return Result{}, ErrNotFound
		case errors.Is(err, storage.ErrCandidateConfirmed):
			return Result{}, fmt.Errorf("%w（%s）", ErrConfirmed, c.ID)
		default:
			return Result{}, err
		}
	}
	if already {
		return Result{CandidateID: c.ID, Status: "already_rejected",
			Note: "该候选已由并发的拒绝生效（幂等）"}, nil
	}
	s.audit(ctx, "memory_rejected",
		fmt.Sprintf("候选 %s 已拒绝（%s 类，原因：%s）", c.ID, c.Kind, strings.TrimSpace(reason)))
	return Result{CandidateID: c.ID, Status: storage.MemoryStatusRejected,
		Note: "已拒绝，不会进入已确认信息"}, nil
}

// audit 记录管理动作。只记 ID/类别/槽位等元信息，不扩散候选正文。
//
// 失败策略（明确约定）：security_events 是**观测层**——写入失败只记 WARN
// 日志，绝不回滚已经原子提交的核心状态机（决策+投影+历史），审计失败
// 不能把"已确认"变成半状态。事务内的 user_profile_history 行才是
// 逐槽位的权威审计，它与核心状态同生共死。
func (s *Service) audit(ctx context.Context, kind, detail string) {
	s.logger.Info("记忆生命周期", "action", kind, "detail", detail)
	if err := s.store.RecordSecurityEvent(ctx, kind, detail, "admin"); err != nil {
		s.logger.Warn("记忆生命周期审计写入失败（核心状态不受影响）",
			"action", kind, "error", err.Error())
	}
}

// sensitiveReason 检查内容是否疑似敏感信息。返回空串表示通过；
// 非空是拒绝原因。确认是"记忆生效"的最后闸门：一旦内容带敏感信息
// 进了 Profile，之后每轮都会注入，比一次候选写错的代价大得多，
// 所以这里的检查独立于写入时的检查（宁可重复，不可遗漏）。
func sensitiveReason(content string) string {
	lower := strings.ToLower(content)
	for _, bad := range sensitivePatterns {
		if strings.Contains(lower, bad) {
			return "包含疑似敏感信息（" + bad + "）"
		}
	}
	return ""
}

// sensitivePatterns 是确认时的敏感内容特征（小写比较）。
// 与工具写入侧的清单（tools.memory）相互独立：写入挡一次，确认再挡一次。
var sensitivePatterns = []string{
	"password", "密码", "passcode", "口令",
	"token", "令牌", "secret", "秘钥", "密钥", "api_key", "api key", "apikey",
	"sk-", "凭证", "credential", "私钥", "授权码",
}

// NormalizeSlotKey 归一化偏好槽位名：去空白、压平内部空白为下划线、
// ASCII 转小写、限长 32 runes。稳定 key 的意义在于"纠正时能对上槽位"，
// 因此归一化必须确定：同一写法永远得到同一个 key。
func NormalizeSlotKey(raw string) string {
	key := strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range key {
		switch {
		case r == ' ' || r == '\t' || r == '　':
			b.WriteRune('_')
		case r < 0x20:
			// 控制字符丢弃
		default:
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len([]rune(out)) > 32 {
		out = string([]rune(out)[:32])
	}
	return out
}
