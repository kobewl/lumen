package config

import (
	"strings"
	"testing"
)

// TestProfileReadsIdentityFromEnv 覆盖身份完全由环境变量驱动。
//
// 这条测试是"身份不是代码常量"的可执行证明：改环境变量就换名字，
// 不需要改任何 Go 代码。
func TestProfileReadsIdentityFromEnv(t *testing.T) {
	t.Setenv("LUMEN_ASSISTANT_NAME", "小灯")
	t.Setenv("LUMEN_ASSISTANT_ROLE", "学习助手")
	t.Setenv("LUMEN_OWNER_DISPLAY_NAME", "liang")
	t.Setenv("LUMEN_ASSISTANT_TONE", "温和、耐心")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	profile := cfg.Profile()

	if profile.Name != "小灯" {
		t.Fatalf("名字应来自环境变量，实际 %q", profile.Name)
	}
	if profile.Role != "学习助手" {
		t.Fatalf("定位应来自环境变量，实际 %q", profile.Role)
	}
	if profile.OwnerDisplayName != "liang" {
		t.Fatalf("称呼应来自环境变量，实际 %q", profile.OwnerDisplayName)
	}
	if profile.Tone != "温和、耐心" {
		t.Fatalf("语气应来自环境变量，实际 %q", profile.Tone)
	}
}

// TestProfileDefaultsToProductName 覆盖未配置时的默认身份。
func TestProfileDefaultsToProductName(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	profile := cfg.Profile()

	if profile.Name != "Lumen" {
		t.Fatalf("默认名字应为产品名 Lumen，实际 %q", profile.Name)
	}
	if !strings.Contains(profile.Role, "助手") {
		t.Fatalf("默认定位应是助手而不是记录工具，实际 %q", profile.Role)
	}
	// 默认配置不该凭空给用户起名字。
	if profile.OwnerDisplayName != "" {
		t.Fatalf("未配置称呼时不应有默认称呼，实际 %q", profile.OwnerDisplayName)
	}
}

// TestProfileCarriesSummaryTime 覆盖总结时间进入主动性描述。
//
// 模型需要知道"几点会主动发总结"，否则"我下班了"这类话只能靠代码里的
// 固定文案回答——那正是要摆脱的东西。
func TestProfileCarriesSummaryTime(t *testing.T) {
	t.Setenv("LUMEN_SUMMARY_HOUR", "21")
	t.Setenv("LUMEN_SUMMARY_MINUTE", "5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	profile := cfg.Profile()

	if !strings.Contains(profile.Proactivity, "21:05") {
		t.Fatalf("主动性描述应包含配置的总结时间 21:05，实际 %q", profile.Proactivity)
	}
	// 提示词块是模型真正看到的内容，时间必须出现在那里。
	if !strings.Contains(profile.PromptBlock(), "21:05") {
		t.Fatalf("提示词块应包含总结时间，实际: %s", profile.PromptBlock())
	}
}

// TestProfileBlankNameFallsBack 覆盖空值回退，避免出现无名的助手。
func TestProfileBlankNameFallsBack(t *testing.T) {
	t.Setenv("LUMEN_ASSISTANT_NAME", "   ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Profile().Name != "Lumen" {
		t.Fatalf("空白名字应回退到默认值，实际 %q", cfg.Profile().Name)
	}
}

// TestRedactedHasNoAssistantSecrets 覆盖配置摘要不含密钥。
func TestRedactedHasNoAssistantSecrets(t *testing.T) {
	t.Setenv("LUMEN_DEEPSEEK_API_KEY", "sk-test-should-not-leak")
	t.Setenv("LUMEN_FEISHU_APP_SECRET", "secret-should-not-leak")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	redacted := cfg.Redacted()

	for key, value := range redacted {
		s := strings.TrimSpace(toString(value))
		if strings.Contains(s, "sk-test-should-not-leak") || strings.Contains(s, "secret-should-not-leak") {
			t.Fatalf("配置摘要 %q 泄露了密钥: %s", key, s)
		}
	}
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}
