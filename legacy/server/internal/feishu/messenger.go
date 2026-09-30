// Package feishu 实现飞书机器人：总结推送、用户白名单与三类最小问答。
//
// 安全约束：
//   - App ID / Secret 只从环境变量读取，绝不写入日志或数据库；
//   - 只响应白名单中的用户，其他来源记录安全事件且不查询数据、不调用模型；
//   - 优先使用出站长连接接收消息，不开放公网回调端口。
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Messenger 通过飞书开放平台 API 发送消息。
type Messenger struct {
	appID     string
	appSecret string
	baseURL   string
	http      *http.Client

	mu          sync.Mutex
	tenantToken string
	tokenExpiry time.Time
}

// NewMessenger 创建发消息客户端。
func NewMessenger(appID, appSecret string) *Messenger {
	return &Messenger{
		appID:     appID,
		appSecret: appSecret,
		baseURL:   "https://open.feishu.cn",
		http:      &http.Client{Timeout: 20 * time.Second},
	}
}

// Name 实现 notification.Sender。
func (m *Messenger) Name() string { return "feishu" }

// Enabled 表示是否具备发送条件。
func (m *Messenger) Enabled() bool {
	return m != nil && m.appID != "" && m.appSecret != ""
}

// tenantAccessToken 获取并缓存 tenant access token。
// token 只保存在内存中，不落库、不写日志。
func (m *Messenger) tenantAccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	if m.tenantToken != "" && time.Now().Before(m.tokenExpiry) {
		token := m.tenantToken
		m.mu.Unlock()
		return token, nil
	}
	m.mu.Unlock()

	payload, err := json.Marshal(map[string]string{
		"app_id":     m.appID,
		"app_secret": m.appSecret,
	})
	if err != nil {
		return "", fmt.Errorf("构造 token 请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.baseURL+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("构造 token 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取 tenant token 失败: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	var parsed struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return "", fmt.Errorf("解析 token 响应失败: %w", err)
	}
	if parsed.Code != 0 || parsed.TenantAccessToken == "" {
		// 只记录错误码，不回显上游消息，避免凭证相关细节进入日志。
		return "", fmt.Errorf("获取 tenant token 被拒绝, code=%d", parsed.Code)
	}

	expire := parsed.Expire
	if expire <= 60 {
		expire = 7200
	}
	m.mu.Lock()
	m.tenantToken = parsed.TenantAccessToken
	m.tokenExpiry = time.Now().Add(time.Duration(expire-60) * time.Second)
	m.mu.Unlock()
	return parsed.TenantAccessToken, nil
}

// SendText 向指定用户发送纯文本消息。
func (m *Messenger) SendText(ctx context.Context, userID, text string) error {
	if !m.Enabled() {
		return fmt.Errorf("飞书未配置")
	}
	token, err := m.tenantAccessToken(ctx)
	if err != nil {
		return err
	}

	// 消息体使用 text 类型，避免富文本注入。
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("构造消息失败: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"receive_id": userID,
		"msg_type":   "text",
		"content":    string(content),
	})
	if err != nil {
		return fmt.Errorf("构造消息失败: %w", err)
	}

	url := m.baseURL + "/open-apis/im/v1/messages?receive_id_type=open_id"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("构造消息请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return fmt.Errorf("解析发送响应失败: %w", err)
	}
	if parsed.Code != 0 {
		return fmt.Errorf("发送消息被拒绝, code=%d", parsed.Code)
	}
	return nil
}
