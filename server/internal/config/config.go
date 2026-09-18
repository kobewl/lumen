// Package config 负责从环境变量读取服务端配置。
//
// 设计原则：
//  1. 业务密钥（DeepSeek Key、飞书 App Secret）只从环境变量读取，绝不写入代码或日志；
//  2. 模型名、Base URL 等可变参数必须可配置，不写死在代码里；
//  3. 缺失的密钥不会导致进程启动失败，只会关闭对应能力，保证采集链路不受影响。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是 lumen-server 的完整运行时配置。
type Config struct {
	// HTTP
	ListenAddr string // 默认 127.0.0.1:8787，只监听本机或 Tailnet 地址

	// 存储
	DBPath             string
	BackupDir          string
	Timezone           string
	EventRetentionDays int
	LogLevel           string

	// 设备注册
	EnrollmentToken string // 一次性注册令牌；为空则关闭注册接口

	// 管理接口
	AdminToken string // 手工触发总结生成等管理操作；为空则关闭

	// DeepSeek
	DeepSeekBaseURL   string
	DeepSeekAPIKey    string
	DeepSeekModel     string
	DeepSeekTimeout   time.Duration
	SummaryDailyLimit int
	QueryDailyLimit   int
	DailyTokenLimit   int
	SummaryHour       int // 每日总结触发小时（本地时区）
	SummaryMinute     int

	// 飞书
	FeishuAppID          string
	FeishuAppSecret      string
	FeishuAllowedUserIDs []string
	FeishuConnectionMode string
	// FeishuPairingMode 是一次性配对模式，仅用于首次取得 users 的 open_id。
	// 开启后非白名单用户会收到"你的 open_id 是 xxx"的回复，但**不查询任何数据、
	// 不调用模型**。配对完成后必须立即关闭。
	FeishuPairingMode bool
	FeishuEnabled     bool

	// 限制
	MaxBatchBytes      int64
	MaxEventBytes      int64
	MaxBatchEvents     int
	ClockSkewTolerance time.Duration
}

// Load 从环境变量解析配置。
func Load() (*Config, error) {
	c := &Config{
		ListenAddr: env("LUMEN_LISTEN_ADDR", "127.0.0.1:8787"),
		DBPath:     env("LUMEN_DB_PATH", "./data/lumen.db"),
		BackupDir:  env("LUMEN_BACKUP_DIR", "./data/backups"),
		Timezone:   env("LUMEN_TIMEZONE", "Asia/Shanghai"),
		LogLevel:   env("LUMEN_LOG_LEVEL", "info"),

		EnrollmentToken: os.Getenv("LUMEN_ENROLLMENT_TOKEN"),
		AdminToken:      os.Getenv("LUMEN_ADMIN_TOKEN"),

		DeepSeekBaseURL: env("LUMEN_DEEPSEEK_BASE_URL", "https://api.deepseek.com"),
		DeepSeekAPIKey:  os.Getenv("LUMEN_DEEPSEEK_API_KEY"),
		DeepSeekModel:   env("LUMEN_DEEPSEEK_MODEL", "deepseek-chat"),

		FeishuAppID:          os.Getenv("LUMEN_FEISHU_APP_ID"),
		FeishuAppSecret:      os.Getenv("LUMEN_FEISHU_APP_SECRET"),
		FeishuConnectionMode: env("LUMEN_FEISHU_CONNECTION_MODE", "long_connection"),
	}

	var err error
	if c.EventRetentionDays, err = envInt("LUMEN_EVENT_RETENTION_DAYS", 30); err != nil {
		return nil, err
	}
	if c.DeepSeekTimeout, err = envDurationSeconds("LUMEN_DEEPSEEK_TIMEOUT_SECONDS", 60); err != nil {
		return nil, err
	}
	if c.SummaryDailyLimit, err = envInt("LUMEN_AI_SUMMARY_DAILY_LIMIT", 2); err != nil {
		return nil, err
	}
	if c.QueryDailyLimit, err = envInt("LUMEN_AI_QUERY_DAILY_LIMIT", 20); err != nil {
		return nil, err
	}
	if c.DailyTokenLimit, err = envInt("LUMEN_AI_DAILY_TOKEN_LIMIT", 200000); err != nil {
		return nil, err
	}
	if c.SummaryHour, err = envInt("LUMEN_SUMMARY_HOUR", 22); err != nil {
		return nil, err
	}
	if c.SummaryMinute, err = envInt("LUMEN_SUMMARY_MINUTE", 30); err != nil {
		return nil, err
	}
	if c.MaxBatchBytes, err = envInt64("LUMEN_MAX_BATCH_BYTES", 512*1024); err != nil {
		return nil, err
	}
	if c.MaxEventBytes, err = envInt64("LUMEN_MAX_EVENT_BYTES", 64*1024); err != nil {
		return nil, err
	}
	if c.MaxBatchEvents, err = envInt("LUMEN_MAX_BATCH_EVENTS", 500); err != nil {
		return nil, err
	}
	if c.ClockSkewTolerance, err = envDurationSeconds("LUMEN_CLOCK_SKEW_TOLERANCE_SECONDS", 300); err != nil {
		return nil, err
	}

	c.FeishuAllowedUserIDs = splitIDs(os.Getenv("LUMEN_FEISHU_ALLOWED_USER_IDS"))
	c.FeishuPairingMode = strings.EqualFold(strings.TrimSpace(os.Getenv("LUMEN_FEISHU_PAIRING_MODE")), "true")
	// 配对模式下即使白名单为空也要启动机器人，否则拿不到 open_id。
	c.FeishuEnabled = c.FeishuAppID != "" && c.FeishuAppSecret != "" &&
		(len(c.FeishuAllowedUserIDs) > 0 || c.FeishuPairingMode)

	if c.SummaryHour < 0 || c.SummaryHour > 23 {
		return nil, fmt.Errorf("LUMEN_SUMMARY_HOUR 必须在 0-23 之间，当前 %d", c.SummaryHour)
	}
	if c.SummaryMinute < 0 || c.SummaryMinute > 59 {
		return nil, fmt.Errorf("LUMEN_SUMMARY_MINUTE 必须在 0-59 之间，当前 %d", c.SummaryMinute)
	}
	return c, nil
}

// Location 返回配置的展示时区，解析失败时退回 UTC。
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// AIEnabled 表示 DeepSeek 调用是否具备条件。
func (c *Config) AIEnabled() bool {
	return c.DeepSeekAPIKey != "" && c.DeepSeekModel != "" && c.DeepSeekBaseURL != ""
}

// Redacted 返回可安全写入日志的配置摘要，不含任何密钥。
func (c *Config) Redacted() map[string]any {
	return map[string]any{
		"listen_addr":      c.ListenAddr,
		"db_path":          c.DBPath,
		"timezone":         c.Timezone,
		"deepseek_base":    c.DeepSeekBaseURL,
		"deepseek_model":   c.DeepSeekModel,
		"deepseek_key_set": c.DeepSeekAPIKey != "",
		"ai_enabled":       c.AIEnabled(),
		"feishu_enabled":   c.FeishuEnabled,
		"feishu_mode":      c.FeishuConnectionMode,
		"feishu_pairing":   c.FeishuPairingMode,
		"feishu_allowed":   len(c.FeishuAllowedUserIDs),
		"enroll_enabled":   c.EnrollmentToken != "",
		"admin_enabled":    c.AdminToken != "",
		"summary_at":       fmt.Sprintf("%02d:%02d", c.SummaryHour, c.SummaryMinute),
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s 不是合法整数: %q", key, raw)
	}
	return v, nil
}

func envInt64(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s 不是合法整数: %q", key, raw)
	}
	return v, nil
}

func envDurationSeconds(key string, fallbackSeconds int) (time.Duration, error) {
	v, err := envInt(key, fallbackSeconds)
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s 必须为正数，当前 %d", key, v)
	}
	return time.Duration(v) * time.Second, nil
}

// splitIDs 解析逗号分隔的用户 ID 列表，自动去空格和空项。
func splitIDs(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
