package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 本文件是 UserProfile 读模型：已确认信息的当前值与版本历史。
//
// 与候选记忆的关系（刻意的两层）：
//   - memory_candidates 是**提议层**：模型写入，内容不改写，正文不进上下文；
//   - user_profile_entries 是**生效层**：只有管理 API 的确认动作往这里写，
//     上下文装配器只读这一层。
//
// 用户纠正一条偏好 = 同一 (user_id, key) 的新确认 → version+1 替换当前值；
// 旧值从 entries 消失，但完整保留在 user_profile_history（追加式）里，
// 因此"现在的值"与"曾经确认过什么"永远都能回答，且同一时刻
// 每个槽位只有一个值被注入，不存在两个互相矛盾值同时可见。

// ProfileEntry 是一条已确认信息的当前值。
type ProfileEntry struct {
	UserID            string
	Key               string
	Kind              string
	Value             string
	Version           int
	SourceCandidateID string
	SourceIDs         []string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ProfileChange 是一次确认/替换的历史记录（兼确认动作审计）。
type ProfileChange struct {
	ID            int64
	UserID        string
	Key           string
	Version       int
	Action        string // ConfirmActionInitial / ConfirmActionReplace
	CandidateID   string
	Value         string
	PreviousValue string
	Operator      string
	CreatedAt     time.Time
}

// 确认动作取值。
const (
	ConfirmActionInitial = "confirm_initial" // 槽位第一次有确认值
	ConfirmActionReplace = "confirm_replace" // 同槽位的新值替换旧值（用户纠正）
)

// 生命周期条件转换的结果哨兵。profile 服务把它们映射成 HTTP 语义
// （404 / 409），storage 层不关心 HTTP。
var (
	// ErrCandidateNotFound：候选不存在或 ID 为空。
	ErrCandidateNotFound = errors.New("候选记忆不存在")
	// ErrCandidateRejected：候选已被拒绝；拒绝是终态，不能再确认。
	ErrCandidateRejected = errors.New("候选记忆已被拒绝，不能再确认")
	// ErrCandidateConfirmed：候选已确认；不能改为拒绝（撤销确认不在本版本）。
	ErrCandidateConfirmed = errors.New("候选记忆已确认，不能改为拒绝")
)

// ConfirmMemoryCandidate 把"确认"收敛为**单个原子操作**：
//
//	条件状态转换（仅 candidate 状态可确认）→ Profile 当前值投影 → 版本历史
//
// 三步在同一个事务里，任何一步失败整体回滚：候选仍是 candidate、
// 读模型与历史不动——不会出现"候选已 confirmed 但没有投影"的半状态。
//
// 并发语义：条件 UPDATE（WHERE status='candidate'）保证多个并发确认
// 只有一个真正生效，其余返回 already=true（幂等），version 只加一次；
// 确认/拒绝并发时同样只有一个方向生效。
//
// profileEligible=false 时只做状态转换（fact/project 类不进读模型）；
// 为 true 时 entry.Key 必须非空（服务层负责归一化，这里兜底校验）。
// entry.UserID 以库内行为准（不信任调用方传入的归属）。
func (s *Store) ConfirmMemoryCandidate(ctx context.Context, candidateID, operator string,
	entry ProfileEntry, profileEligible bool) (change ProfileChange, replaced bool, already bool, err error) {
	if strings.TrimSpace(candidateID) == "" {
		return ProfileChange{}, false, false, fmt.Errorf("%w：缺少候选 ID", ErrCandidateNotFound)
	}
	now := time.Now().UTC()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return ProfileChange{}, false, false, fmt.Errorf("开启确认事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var status, userID string
	err = tx.QueryRowContext(ctx, `
		SELECT status, user_id FROM memory_candidates WHERE id = ?`, candidateID).
		Scan(&status, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return ProfileChange{}, false, false, fmt.Errorf("%w（%s）", ErrCandidateNotFound, candidateID)
	}
	if err != nil {
		return ProfileChange{}, false, false, fmt.Errorf("读取候选失败: %w", err)
	}

	switch status {
	case MemoryStatusConfirmed:
		// 幂等路径：什么都不写，直接返回"已确认过"。
		return ProfileChange{}, false, true, nil
	case MemoryStatusRejected:
		return ProfileChange{}, false, false, fmt.Errorf("%w（%s）", ErrCandidateRejected, candidateID)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE memory_candidates
		SET status = ?, decided_at = ?, decided_by = ?, reject_reason = ''
		WHERE id = ? AND status = ?`,
		MemoryStatusConfirmed, FormatISO(now), operator, candidateID, MemoryStatusCandidate)
	if err != nil {
		return ProfileChange{}, false, false, fmt.Errorf("写入候选决策失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 条件更新未命中：状态在事务中被并发改写（防御路径；当前连接池
		// 为单连接，正常不会走到）。重读并给出确定结果，绝不盲写。
		var cur string
		if scanErr := tx.QueryRowContext(ctx,
			`SELECT status FROM memory_candidates WHERE id = ?`, candidateID).Scan(&cur); scanErr != nil {
			return ProfileChange{}, false, false, fmt.Errorf("重读候选状态失败: %w", scanErr)
		}
		switch cur {
		case MemoryStatusConfirmed:
			return ProfileChange{}, false, true, nil
		case MemoryStatusRejected:
			return ProfileChange{}, false, false, fmt.Errorf("%w（%s）", ErrCandidateRejected, candidateID)
		default:
			return ProfileChange{}, false, false, fmt.Errorf("候选 %s 状态并发变化（%s）", candidateID, cur)
		}
	}

	if profileEligible {
		if strings.TrimSpace(entry.Key) == "" {
			return ProfileChange{}, false, false, errors.New("preference 确认缺少槽位 key")
		}
		entry.UserID = userID
		change, replaced, err = s.confirmProfileValueTx(ctx, tx, entry, candidateID, operator, now)
		if err != nil {
			return ProfileChange{}, false, false, err
		}
	}

	if err = tx.Commit(); err != nil {
		return ProfileChange{}, false, false, fmt.Errorf("提交确认事务失败: %w", err)
	}
	return change, replaced, false, nil
}

// RejectMemoryCandidate 用**条件状态转换**拒绝一条候选：
// 只有 candidate 状态能被拒绝。确认/拒绝并发时只有一个方向生效——
// 已确认的返回 ErrCandidateConfirmed，已拒绝的幂等返回 already=true。
// 拒绝不触碰 Profile 读模型（拒绝的候选从未产生过投影）。
func (s *Store) RejectMemoryCandidate(ctx context.Context, candidateID, operator, reason string) (already bool, err error) {
	if strings.TrimSpace(candidateID) == "" {
		return false, fmt.Errorf("%w：缺少候选 ID", ErrCandidateNotFound)
	}
	now := time.Now().UTC()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("开启拒绝事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM memory_candidates WHERE id = ?`, candidateID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w（%s）", ErrCandidateNotFound, candidateID)
	}
	if err != nil {
		return false, fmt.Errorf("读取候选失败: %w", err)
	}
	switch status {
	case MemoryStatusRejected:
		return true, nil
	case MemoryStatusConfirmed:
		return false, fmt.Errorf("%w（%s）", ErrCandidateConfirmed, candidateID)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE memory_candidates
		SET status = ?, decided_at = ?, decided_by = ?, reject_reason = ?
		WHERE id = ? AND status = ?`,
		MemoryStatusRejected, FormatISO(now), operator, strings.TrimSpace(reason),
		candidateID, MemoryStatusCandidate)
	if err != nil {
		return false, fmt.Errorf("写入候选决策失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var cur string
		if scanErr := tx.QueryRowContext(ctx,
			`SELECT status FROM memory_candidates WHERE id = ?`, candidateID).Scan(&cur); scanErr != nil {
			return false, fmt.Errorf("重读候选状态失败: %w", scanErr)
		}
		switch cur {
		case MemoryStatusRejected:
			return true, nil
		case MemoryStatusConfirmed:
			return false, fmt.Errorf("%w（%s）", ErrCandidateConfirmed, candidateID)
		default:
			return false, fmt.Errorf("候选 %s 状态并发变化（%s）", candidateID, cur)
		}
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("提交拒绝事务失败: %w", err)
	}
	return false, nil
}

// ProfileEntries 返回用户的全部已确认信息（最新更新的在前）。
func (s *Store) ProfileEntries(ctx context.Context, userID string) ([]ProfileEntry, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT user_id, key, kind, value, version, source_candidate_id, source_ids_json,
		       created_at, updated_at
		FROM user_profile_entries
		WHERE user_id = ?
		ORDER BY updated_at DESC, key ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("查询已确认信息失败: %w", err)
	}
	defer rows.Close()
	var out []ProfileEntry
	for rows.Next() {
		e, err := scanProfileEntry(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ProfileEntryByKey 取用户某个槽位的当前值；不存在时 ok=false。
func (s *Store) ProfileEntryByKey(ctx context.Context, userID, key string) (ProfileEntry, bool, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT user_id, key, kind, value, version, source_candidate_id, source_ids_json,
		       created_at, updated_at
		FROM user_profile_entries
		WHERE user_id = ? AND key = ?`, userID, key)
	e, err := scanProfileEntry(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProfileEntry{}, false, nil
		}
		return ProfileEntry{}, false, fmt.Errorf("查询已确认信息失败: %w", err)
	}
	return e, true, nil
}

// ConfirmProfileValue 把一次"确认值"写入读模型：当前值替换 + 版本历史。
//
// 它是底层原语（历史数据回填与测试用）；**生命周期入口**是
// ConfirmMemoryCandidate——它把候选决策与本投影放进同一个事务，
// 保证不出现半状态。
// replaced=true 表示这次确认替换了同槽位的旧值（用户纠正）。
func (s *Store) ConfirmProfileValue(ctx context.Context, e ProfileEntry,
	candidateID, operator string) (change ProfileChange, replaced bool, err error) {
	if e.Key == "" {
		return ProfileChange{}, false, errors.New("已确认信息缺少槽位 key")
	}
	now := time.Now().UTC()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return ProfileChange{}, false, fmt.Errorf("开启确认事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	change, replaced, err = s.confirmProfileValueTx(ctx, tx, e, candidateID, operator, now)
	if err != nil {
		return ProfileChange{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return ProfileChange{}, false, fmt.Errorf("提交确认事务失败: %w", err)
	}
	return change, replaced, nil
}

// confirmProfileValueTx 在给定事务里完成"当前值替换 + 版本历史"。
//
// 被两处复用：ConfirmProfileValue（独立事务，回填/测试）与
// ConfirmMemoryCandidate（生命周期主入口，与候选决策同事务）。
func (s *Store) confirmProfileValueTx(ctx context.Context, tx *sql.Tx, e ProfileEntry,
	candidateID, operator string, now time.Time) (change ProfileChange, replaced bool, err error) {
	var prevValue string
	var prevVersion int
	var prevCreated string
	err = tx.QueryRowContext(ctx, `
		SELECT value, version, created_at FROM user_profile_entries
		WHERE user_id = ? AND key = ?`, e.UserID, e.Key).
		Scan(&prevValue, &prevVersion, &prevCreated)
	prevExists := true
	switch {
	case errors.Is(err, sql.ErrNoRows):
		prevExists = false
	case err != nil:
		return ProfileChange{}, false, fmt.Errorf("读取旧确认值失败: %w", err)
	}

	version := 1
	if prevExists {
		version = prevVersion + 1
		if prevVersion <= 0 {
			// 历史脏数据兜底：版本号必须严格递增。
			version = 2
		}
	}
	prevCreatedAt, err := parseOrCreate(prevCreated, now)
	if err != nil {
		return ProfileChange{}, false, err
	}
	sourceJSON, err := json.Marshal(e.SourceIDs)
	if err != nil {
		sourceJSON = []byte("[]")
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO user_profile_entries
			(user_id, key, kind, value, version, source_candidate_id, source_ids_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, key) DO UPDATE SET
			kind = excluded.kind,
			value = excluded.value,
			version = excluded.version,
			source_candidate_id = excluded.source_candidate_id,
			source_ids_json = excluded.source_ids_json,
			updated_at = excluded.updated_at`,
		e.UserID, e.Key, e.Kind, e.Value, version, e.SourceCandidateID,
		string(sourceJSON), FormatISO(prevCreatedAt), FormatISO(now))
	if err != nil {
		return ProfileChange{}, false, fmt.Errorf("写入已确认信息失败: %w", err)
	}

	action := ConfirmActionInitial
	if prevExists {
		action = ConfirmActionReplace
	}
	change = ProfileChange{
		UserID:        e.UserID,
		Key:           e.Key,
		Version:       version,
		Action:        action,
		CandidateID:   candidateID,
		Value:         e.Value,
		PreviousValue: prevValue,
		Operator:      operator,
		CreatedAt:     now,
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO user_profile_history
			(user_id, key, version, action, candidate_id, value, previous_value, operator, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		change.UserID, change.Key, change.Version, change.Action, change.CandidateID,
		change.Value, change.PreviousValue, change.Operator, FormatISO(change.CreatedAt))
	if err != nil {
		return ProfileChange{}, false, fmt.Errorf("写入确认历史失败: %w", err)
	}
	if id, idErr := res.LastInsertId(); idErr == nil {
		change.ID = id
	}
	return change, prevExists, nil
}

// ProfileHistory 返回用户某槽位（或全部槽位，key 为空时）的确认历史。
func (s *Store) ProfileHistory(ctx context.Context, userID, key string, limit int) ([]ProfileChange, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id, user_id, key, version, action, candidate_id, value, previous_value, operator, created_at
		FROM user_profile_history WHERE user_id = ?`
	args := []any{userID}
	if key != "" {
		query += ` AND key = ?`
		args = append(args, key)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询确认历史失败: %w", err)
	}
	defer rows.Close()
	var out []ProfileChange
	for rows.Next() {
		var c ProfileChange
		var created string
		if err := rows.Scan(&c.ID, &c.UserID, &c.Key, &c.Version, &c.Action, &c.CandidateID,
			&c.Value, &c.PreviousValue, &c.Operator, &created); err != nil {
			return nil, fmt.Errorf("解析确认历史失败: %w", err)
		}
		if c.CreatedAt, err = ParseISO(created); err != nil {
			return nil, fmt.Errorf("解析确认历史时间失败: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// parseOrCreate 解析时间字符串；空串视为 created（新槽位的创建时间）。
func parseOrCreate(raw string, created time.Time) (time.Time, error) {
	if raw == "" {
		return created, nil
	}
	return ParseISO(raw)
}

// scanProfileEntry 把一行 entries 扫描成结构体。
func scanProfileEntry(scan func(dest ...any) error) (ProfileEntry, error) {
	var e ProfileEntry
	var sourceJSON, created, updated string
	if err := scan(&e.UserID, &e.Key, &e.Kind, &e.Value, &e.Version, &e.SourceCandidateID,
		&sourceJSON, &created, &updated); err != nil {
		return ProfileEntry{}, fmt.Errorf("解析已确认信息失败: %w", err)
	}
	_ = json.Unmarshal([]byte(sourceJSON), &e.SourceIDs)
	var err error
	if e.CreatedAt, err = ParseISO(created); err != nil {
		return ProfileEntry{}, fmt.Errorf("解析创建时间失败: %w", err)
	}
	if e.UpdatedAt, err = ParseISO(updated); err != nil {
		return ProfileEntry{}, fmt.Errorf("解析更新时间失败: %w", err)
	}
	return e, nil
}
