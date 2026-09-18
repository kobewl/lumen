// Package ai 封装对 DeepSeek 的调用。
//
// 调用边界（与产品文档一致）：
//   - 只发送聚合后的 Session Context，绝不发送原始事件、路径、标题、凭证；
//   - 超时 60 秒，对 429 / 5xx 最多重试 2 次并指数退避；
//   - 响应必须是合法 JSON，校验失败按失败处理，不把半截文本当成功；
//   - 日志只记录请求 id、模型、耗时、token 用量和错误码，绝不记录完整 prompt。
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client 是 DeepSeek Chat Completions 客户端。
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
	maxRetries int
}

// Usage 是一次调用的 token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response 是一次成功调用的结果。
type Response struct {
	Content string
	Usage   Usage
	Model   string
	Elapsed time.Duration
}

// APIError 表示调用失败，Code 是可写入日志与数据库的短错误码。
type APIError struct {
	Code       string
	HTTPStatus int
	Message    string
	Retryable  bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (http=%d): %s", e.Code, e.HTTPStatus, e.Message)
}

// NewClient 创建 DeepSeek 客户端。
func NewClient(baseURL, apiKey, model string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		maxRetries: 2,
	}
}

// Enabled 表示客户端是否具备调用条件。
func (c *Client) Enabled() bool {
	return c != nil && c.apiKey != "" && c.model != "" && c.baseURL != ""
}

// Model 返回当前配置的模型名。
func (c *Client) Model() string { return c.model }

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Stream         bool            `json:"stream"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// CompleteJSON 请求模型返回 JSON 内容，并做基础结构校验。
func (c *Client) CompleteJSON(ctx context.Context, systemPrompt, userPrompt string, maxTokens int) (Response, error) {
	if !c.Enabled() {
		return Response{}, &APIError{Code: "ai_disabled", Message: "DeepSeek 未配置", Retryable: false}
	}

	body := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		// JSON 模式要求 prompt 中出现 json 字样，调用方的 prompt 已包含该说明。
		ResponseFormat: &responseFormat{Type: "json_object"},
		Temperature:    0.2,
		MaxTokens:      maxTokens,
		Stream:         false,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, &APIError{Code: "marshal_failed", Message: "请求序列化失败", Retryable: false}
	}

	var lastErr error
	backoff := 2 * time.Second
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Response{}, &APIError{Code: "timeout", Message: "调用超时", Retryable: true}
			case <-time.After(backoff):
			}
			backoff *= 3
		}

		resp, err := c.doOnce(ctx, payload)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		var apiErr *APIError
		if errors.As(err, &apiErr) && !apiErr.Retryable {
			return Response{}, err
		}
	}
	return Response{}, lastErr
}

func (c *Client) doOnce(ctx context.Context, payload []byte) (Response, error) {
	start := time.Now()
	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return Response{}, &APIError{Code: "request_build_failed", Message: "构造请求失败", Retryable: false}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, &APIError{Code: "timeout", Message: "调用超时", Retryable: true}
		}
		return Response{}, &APIError{Code: "network_error", Message: "网络请求失败", Retryable: true}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Response{}, &APIError{Code: "read_failed", Message: "读取响应失败", Retryable: true}
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return Response{}, &APIError{Code: "rate_limited", HTTPStatus: resp.StatusCode, Message: "触发限流", Retryable: true}
	}
	if resp.StatusCode >= 500 {
		return Response{}, &APIError{Code: "server_error", HTTPStatus: resp.StatusCode, Message: "上游服务错误", Retryable: true}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// 不回显响应正文，避免把上游提示中的信息写进日志。
		return Response{}, &APIError{Code: "auth_failed", HTTPStatus: resp.StatusCode, Message: "API Key 无效或无权限", Retryable: false}
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, &APIError{Code: "http_error", HTTPStatus: resp.StatusCode, Message: "非预期状态码", Retryable: false}
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, &APIError{Code: "invalid_response", Message: "响应不是合法 JSON", Retryable: false}
	}
	if parsed.Error != nil {
		return Response{}, &APIError{Code: "upstream_error", Message: "上游返回错误", Retryable: false}
	}
	if len(parsed.Choices) == 0 {
		return Response{}, &APIError{Code: "empty_choices", Message: "响应没有内容", Retryable: false}
	}

	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if content == "" {
		return Response{}, &APIError{Code: "empty_content", Message: "响应内容为空", Retryable: false}
	}
	if parsed.Choices[0].FinishReason == "length" {
		return Response{}, &APIError{Code: "truncated", Message: "响应被长度截断", Retryable: false}
	}

	return Response{
		Content: content,
		Model:   parsed.Model,
		Elapsed: time.Since(start),
		Usage: Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			TotalTokens:      parsed.Usage.TotalTokens,
		},
	}, nil
}

// ExtractJSON 从模型输出中提取 JSON 对象。
// 即使使用了 JSON 模式，模型仍可能包裹 ```json 代码块，因此这里做一次容错。
func ExtractJSON(content string) ([]byte, error) {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, errors.New("响应中找不到 JSON 对象")
	}
	return []byte(s[start : end+1]), nil
}
