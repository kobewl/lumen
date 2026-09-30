// Package auth 实现设备注册与令牌校验。
//
// 安全要点：
//   - enrollment token 是一次性的，注册成功后由运维轮换或清空；
//   - device token 只在注册响应中出现一次，服务端只保存 SHA-256 哈希；
//   - 所有比较使用 constant-time 比较，避免时序侧信道。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"lumen/server/internal/storage"
)

// ErrUnauthorized 表示凭证无效。
var ErrUnauthorized = errors.New("unauthorized")

// ErrEnrollmentDisabled 表示注册接口未开启。
var ErrEnrollmentDisabled = errors.New("enrollment disabled")

// ErrDeviceLimit 表示已达单用户设备上限。
var ErrDeviceLimit = errors.New("device limit reached")

// Service 提供注册与鉴权能力。
type Service struct {
	store           *storage.Store
	enrollmentToken string
	maxDevices      int
}

// NewService 创建鉴权服务。maxDevices 为 0 时按单用户单设备处理。
func NewService(store *storage.Store, enrollmentToken string, maxDevices int) *Service {
	if maxDevices <= 0 {
		maxDevices = 1
	}
	return &Service{store: store, enrollmentToken: enrollmentToken, maxDevices: maxDevices}
}

// EnrollmentEnabled 表示注册接口是否可用。
func (s *Service) EnrollmentEnabled() bool { return s.enrollmentToken != "" }

// HashToken 计算令牌的 SHA-256 十六进制摘要。
// 使用摘要而不是加密，是因为服务端只需要“验证是否相等”，不需要还原原文。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// GenerateToken 生成 32 字节随机令牌，Base64 URL 编码便于放入 HTTP 头。
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// RegisterResult 是注册成功后的返回内容。
type RegisterResult struct {
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
}

// Register 校验一次性 enrollment token 并签发 device token。
func (s *Service) Register(ctx context.Context, enrollmentToken, deviceName string) (RegisterResult, error) {
	if !s.EnrollmentEnabled() {
		return RegisterResult{}, ErrEnrollmentDisabled
	}
	// constant-time 比较，避免通过响应时间猜测令牌。
	if subtle.ConstantTimeCompare([]byte(enrollmentToken), []byte(s.enrollmentToken)) != 1 {
		_ = s.store.RecordSecurityEvent(ctx, "enrollment_rejected", "enrollment token 不匹配", "")
		return RegisterResult{}, ErrUnauthorized
	}

	count, err := s.store.CountActiveDevices(ctx)
	if err != nil {
		return RegisterResult{}, err
	}
	if int(count) >= s.maxDevices {
		_ = s.store.RecordSecurityEvent(ctx, "enrollment_rejected", "已达设备上限", "")
		return RegisterResult{}, ErrDeviceLimit
	}

	deviceID := deviceIDFromName(deviceName)
	token, err := GenerateToken()
	if err != nil {
		return RegisterResult{}, err
	}
	if err := s.store.CreateDevice(ctx, deviceID, deviceName, HashToken(token)); err != nil {
		return RegisterResult{}, err
	}
	return RegisterResult{DeviceID: deviceID, DeviceToken: token}, nil
}

// Authenticate 校验 device token，返回对应设备。
// 无论令牌格式错误还是查不到设备，都返回统一的 ErrUnauthorized，
// 避免通过错误信息区分“令牌不存在”和“令牌错误”。
func (s *Service) Authenticate(ctx context.Context, token string) (storage.Device, error) {
	if strings.TrimSpace(token) == "" {
		return storage.Device{}, ErrUnauthorized
	}
	device, err := s.store.DeviceByTokenHash(ctx, HashToken(token))
	if err != nil {
		if errors.Is(err, storage.ErrDeviceNotFound) {
			return storage.Device{}, ErrUnauthorized
		}
		return storage.Device{}, err
	}
	return device, nil
}

// deviceIDFromName 由设备名派生稳定 ID，非法字符替换为短横线。
func deviceIDFromName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "desktop-mac-01"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" {
		id = "desktop-mac-01"
	}
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}
