// Package storage 封装服务端 SQLite 存储。
//
// 设计要点：
//   - WAL 模式 + busy_timeout，写操作串行化（单写者）；
//   - 所有时间以 UTC RFC3339 字符串存储，便于字符串比较与索引；
//   - 事件按 event_id 主键幂等，重复写入返回 duplicate 而不是报错。
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无需 CGO
)

// Store 持有数据库连接池。
type Store struct {
	DB *sql.DB
}

// Open 打开（或创建）SQLite 数据库并完成迁移。
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}

	// _pragma 参数确保每条连接都启用 WAL、busy_timeout 和外键。
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// SQLite 单写者：限制连接数，避免写冲突。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("数据库连接失败: %w", err)
	}

	s := &Store{DB: db}
	if err := Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.Close()
}

// Checkpoint 执行一次 WAL checkpoint，控制 WAL 文件增长。
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Backup 使用 SQLite VACUUM INTO 生成一致性快照（等价于在线备份 API）。
func (s *Store) Backup(ctx context.Context, destPath string) error {
	if dir := filepath.Dir(destPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("创建备份目录失败: %w", err)
		}
	}
	// 清理可能存在的旧文件，VACUUM INTO 要求目标不存在。
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清理旧备份失败: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `VACUUM INTO ?`, destPath); err != nil {
		return fmt.Errorf("生成快照失败: %w", err)
	}
	return nil
}

// NowISO 返回当前 UTC 时间的 RFC3339 字符串，作为所有存储时间的统一格式。
func NowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// FormatISO 把时间转换为存储格式。
func FormatISO(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// ParseISO 解析存储格式的时间。
func ParseISO(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}
