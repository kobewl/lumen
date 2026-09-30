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

	"lumen/server/internal/identity"
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

	// 助手身份（AssistantProfile）
	//
	// 这些值决定助手怎么称呼自己、怎么称呼用户、用什么语言与语气。
	// 身份必须是配置而不是代码常量：换一种人格、换一个部署环境应该只改
	// 环境变量，而不是改代码重新编译。
	AssistantName      string
	AssistantRole      string
	OwnerDisplayName   string
	AssistantLanguage  string
	AssistantTone      string
	AssistantProactive string

	// 限制
	MaxBatchBytes      int64
	MaxEventBytes      int64
	MaxBatchEvents     int
	ClockSkewTolerance time.Duration

	// 主动关怀（Initiative）
	//
	// 两道开关默认都朝"不发"的方向：Enabled 默认 false（功能整体关闭），
	// DryRun 默认 true（即使启用也只写 outbox 不真发）。
	// 真实发送需要同时显式配置 Enabled=true 且 DryRun=false。
	InitiativeEnabled bool
	InitiativeDryRun  bool

	// 上下文装配预算（Context Assembler）。
	//
	// 0 表示用代码里的默认值（DefaultBudget）。只开放整包上限与近期轮次
	// 两个旋钮：段级上限属于产品口径，改它们应该改代码并过测试。
	ContextBudgetTotal int
	ContextRecentTurns int

	// DevFakeClock 是本地验收用的固定时钟（RFC3339，如 2026-09-18T13:19:00+08:00）。
	// 留空即真实时间。名字带 Dev：它会让"今天/现在"整体冻结，
	// 只应该出现在本地验收环境；设了它启动日志会大声警告。
	DevFakeClock string
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

		// 身份默认值是产品名 Lumen + 助手/伙伴定位；留空即使用默认。
		AssistantName:      env("LUMEN_ASSISTANT_NAME", "Lumen"),
		AssistantRole:      env("LUMEN_ASSISTANT_ROLE", "个人助手/伙伴"),
		OwnerDisplayName:   os.Getenv("LUMEN_OWNER_DISPLAY_NAME"),
		AssistantLanguage:  env("LUMEN_ASSISTANT_LANGUAGE", "zh-CN"),
		AssistantTone:      env("LUMEN_ASSISTANT_TONE", "友好、简洁、像朋友"),
		AssistantProactive: env("LUMEN_ASSISTANT_PROACTIVITY", "低：只在被问到或明确需要时回应，不主动打扰"),
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

	c.InitiativeEnabled = strings.EqualFold(strings.TrimSpace(os.Getenv("LUMEN_INITIATIVE_ENABLED")), "true")
	// DryRun 默认 true：只有显式设为 false 才真实发送（fail-safe）。
	c.InitiativeDryRun = !strings.EqualFold(strings.TrimSpace(os.Getenv("LUMEN_INITIATIVE_DRY_RUN")), "false")
	c.DevFakeClock = strings.TrimSpace(os.Getenv("LUMEN_DEV_FAKE_CLOCK"))
	if c.DevFakeClock != "" {
		if _, err := time.Parse(time.RFC3339, c.DevFakeClock); err != nil {
			return nil, fmt.Errorf("LUMEN_DEV_FAKE_CLOCK 必须是 RFC3339 时间（如 2026-09-18T13:19:00+08:00）: %v", err)
		}
	}

	if c.ContextBudgetTotal, err = envInt("LUMEN_CONTEXT_BUDGET_TOTAL", 0); err != nil {
		return nil, err
	}
	if c.ContextRecentTurns, err = envInt("LUMEN_CONTEXT_RECENT_TURNS", 0); err != nil {
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

// Profile 把身份配置转成 Agent 使用的助手身份。
//
// 放在这里而不是 main.go：身份是配置的一部分，转换逻辑只有一处，
// 测试也可以在不启动服务的情况下验证"改配置是否真的换了身份"。
//
// 总结时间被拼进「主动性」描述：这是模型判断"几点会收到总结"的唯一来源。
// 之前这个时间写死在「我下班了」的固定回复里，改配置就会骗人。
func (c *Config) Profile() identity.Profile {
	proactive := c.AssistantProactive
	if c.SummaryHour >= 0 && c.SummaryHour <= 23 && c.SummaryMinute >= 0 && c.SummaryMinute <= 59 {
		proactive = fmt.Sprintf("%s；每天 %02d:%02d 会用当天的完整记录主动发一份总结"+
			"（在此之前不要替用户提前生成总结）", proactive, c.SummaryHour, c.SummaryMinute)
	}
	return identity.Profile{
		Name:             c.AssistantName,
		Role:             c.AssistantRole,
		OwnerDisplayName: c.OwnerDisplayName,
		Language:         c.AssistantLanguage,
		Tone:             c.AssistantTone,
		Proactivity:      proactive,
	}.Normalize()
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
		"assistant_name":   c.AssistantName,
		"assistant_role":   c.AssistantRole,
		"initiative":       c.InitiativeEnabled,
		"initiative_dry":   c.InitiativeDryRun,
		"dev_fake_clock":   c.DevFakeClock != "",
		"ctx_budget_total": c.ContextBudgetTotal,
		"ctx_recent_turns": c.ContextRecentTurns,
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
