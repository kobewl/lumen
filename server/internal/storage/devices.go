package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrDeviceNotFound 表示设备不存在。
var ErrDeviceNotFound = errors.New("device not found")

// Device 是注册过的 Desktop 设备。
// 注意：这里从不保存 device token 原文，只有 SHA-256 哈希。
type Device struct {
	ID         string
	Name       string
	TokenHash  string
	CreatedAt  time.Time
	LastSeenAt *time.Time
	RevokedAt  *time.Time
}

// CreateDevice 写入一台新设备。
func (s *Store) CreateDevice(ctx context.Context, id, name, tokenHash string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO devices (id, name, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		id, name, tokenHash, NowISO())
	if err != nil {
		return fmt.Errorf("创建设备失败: %w", err)
	}
	return nil
}

// CountActiveDevices 返回未吊销设备数量。V0.1 是单用户，用于限制重复注册。
func (s *Store) CountActiveDevices(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM devices WHERE revoked_at IS NULL`).Scan(&n)
	return n, err
}

// DeviceByTokenHash 按 token 哈希查找未吊销设备。
func (s *Store) DeviceByTokenHash(ctx context.Context, tokenHash string) (Device, error) {
	var d Device
	var created string
	var lastSeen, revoked sql.NullString
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, name, token_hash, created_at, last_seen_at, revoked_at
		FROM devices WHERE token_hash = ? AND revoked_at IS NULL LIMIT 1`, tokenHash).
		Scan(&d.ID, &d.Name, &d.TokenHash, &created, &lastSeen, &revoked)
	if err == sql.ErrNoRows {
		return Device{}, ErrDeviceNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("查询设备失败: %w", err)
	}
	if d.CreatedAt, err = ParseISO(created); err != nil {
		return Device{}, fmt.Errorf("解析设备创建时间失败: %w", err)
	}
	if lastSeen.Valid {
		if t, err := ParseISO(lastSeen.String); err == nil {
			d.LastSeenAt = &t
		}
	}
	if revoked.Valid {
		if t, err := ParseISO(revoked.String); err == nil {
			d.RevokedAt = &t
		}
	}
	return d, nil
}

// DeviceByID 按 ID 查找设备。
func (s *Store) DeviceByID(ctx context.Context, id string) (Device, error) {
	var tokenHash string
	err := s.DB.QueryRowContext(ctx, `SELECT token_hash FROM devices WHERE id = ? LIMIT 1`, id).Scan(&tokenHash)
	if err == sql.ErrNoRows {
		return Device{}, ErrDeviceNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("查询设备失败: %w", err)
	}
	return Device{ID: id, TokenHash: tokenHash}, nil
}

// TouchDevice 更新设备最后活跃时间。
func (s *Store) TouchDevice(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE devices SET last_seen_at = ? WHERE id = ?`, NowISO(), id)
	return err
}

// RevokeDevice 吊销设备（事件响应流程使用）。
func (s *Store) RevokeDevice(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE devices SET revoked_at = ? WHERE id = ?`, NowISO(), id)
	return err
}

// RecordSecurityEvent 记录安全事件（如非白名单来源被拒绝）。
// detail 与 source 只允许写入标识信息，禁止写入消息正文或凭证。
func (s *Store) RecordSecurityEvent(ctx context.Context, kind, detail, source string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO security_events (kind, detail, source, created_at) VALUES (?, ?, ?, ?)`,
		kind, truncate(detail, 200), truncate(source, 128), NowISO())
	return err
}

// RecentSecurityEvents 返回最近的安全事件，供运维排查。
func (s *Store) RecentSecurityEvents(ctx context.Context, limit int) ([]map[string]string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT kind, detail, COALESCE(source, ''), created_at FROM security_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]string
	for rows.Next() {
		var kind, detail, source, created string
		if err := rows.Scan(&kind, &detail, &source, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{"kind": kind, "detail": detail, "source": source, "created_at": created})
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
