package db

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

const settingFOFA = "integration.fofa.v1"

const (
	DefaultFOFARequestIntervalSeconds   = 2
	DefaultFOFARateLimitCooldownSeconds = 60
)

var (
	ErrFOFAConfig            = errors.New("FOFA 配置无效")
	ErrFOFAAPIKey            = fmt.Errorf("%w：请填写有效的 API Key 后启用", ErrFOFAConfig)
	ErrFOFARequestInterval   = fmt.Errorf("%w：请求间隔必须为 1–60 秒的整数", ErrFOFAConfig)
	ErrFOFARateLimitCooldown = fmt.Errorf("%w：限流冷却必须为 1–3600 秒的整数", ErrFOFAConfig)
)

// FOFAConfig 的明文令牌仅供受信服务端调用，禁止通用 JSON 序列化返回。
type FOFAConfig struct {
	Enabled                  bool   `json:"enabled"`
	APIKey                   string `json:"-"`
	RequestIntervalSeconds   int    `json:"request_interval_seconds"`
	RateLimitCooldownSeconds int    `json:"rate_limit_cooldown_seconds"`
}

type FOFAConfigPatch struct {
	Enabled                  *bool   `json:"enabled"`
	APIKey                   *string `json:"api_key"`
	RequestIntervalSeconds   *int    `json:"request_interval_seconds"`
	RateLimitCooldownSeconds *int    `json:"rate_limit_cooldown_seconds"`
}

type fofaStoredConfig struct {
	Enabled                  bool   `json:"enabled"`
	Cipher                   string `json:"api_key_cipher"`
	RequestIntervalSeconds   int    `json:"request_interval_seconds"`
	RateLimitCooldownSeconds int    `json:"rate_limit_cooldown_seconds"`
}

func defaultFOFAConfig() FOFAConfig {
	return FOFAConfig{RequestIntervalSeconds: DefaultFOFARequestIntervalSeconds, RateLimitCooldownSeconds: DefaultFOFARateLimitCooldownSeconds}
}

// 指针区分省略与显式更新；JSON null 不是省略，必须拒绝，不能意外重置默认值。
func (p *FOFAConfigPatch) UnmarshalJSON(data []byte) error {
	if err := validateFOFAThrottleJSON(data); err != nil {
		return err
	}
	type patch FOFAConfigPatch
	var decoded patch
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return ErrFOFAConfig
	}
	*p = FOFAConfigPatch(decoded)
	return nil
}

func validateFOFAThrottleJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return ErrFOFAConfig
	}
	for _, field := range []struct {
		name string
		max  int
		err  error
	}{
		{"request_interval_seconds", 60, ErrFOFARequestInterval},
		{"rate_limit_cooldown_seconds", 3600, ErrFOFARateLimitCooldown},
	} {
		if raw, present := fields[field.name]; present {
			var value int
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || value < 1 || value > field.max {
				return field.err
			}
		}
	}
	return nil
}

func readFOFAStoredConfig(raw string) (fofaStoredConfig, error) {
	stored := fofaStoredConfig{RequestIntervalSeconds: DefaultFOFARequestIntervalSeconds, RateLimitCooldownSeconds: DefaultFOFARateLimitCooldownSeconds}
	if err := validateFOFAThrottleJSON([]byte(raw)); err != nil {
		return stored, err
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return stored, errors.New("FOFA 配置无法读取")
	}
	return stored, nil
}

// 使用现有持久秘密密钥及独立 AAD，备份/恢复无需增加新密钥路径。
func sealFOFAKey(key string) (string, error) {
	if key == "" {
		return "", nil
	}
	a, err := proxyCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return "v1:" + hex.EncodeToString(a.Seal(nonce, nonce, []byte(key), []byte(settingFOFA))), nil
}

func openFOFAConfig(raw string) (FOFAConfig, error) {
	stored, err := readFOFAStoredConfig(raw)
	if err != nil {
		return FOFAConfig{}, err
	}
	c := FOFAConfig{Enabled: stored.Enabled, RequestIntervalSeconds: stored.RequestIntervalSeconds, RateLimitCooldownSeconds: stored.RateLimitCooldownSeconds}
	if stored.Cipher == "" {
		return c, nil
	}
	if !strings.HasPrefix(stored.Cipher, "v1:") {
		return FOFAConfig{}, errors.New("FOFA 密钥格式无效")
	}
	data, err := hex.DecodeString(strings.TrimPrefix(stored.Cipher, "v1:"))
	if err != nil {
		return FOFAConfig{}, errors.New("FOFA 密钥格式无效")
	}
	a, err := proxyCipher()
	if err != nil {
		return FOFAConfig{}, errors.New("FOFA 密钥不可用")
	}
	if len(data) < a.NonceSize()+a.Overhead() {
		return FOFAConfig{}, errors.New("FOFA 密钥不可用")
	}
	plain, err := a.Open(nil, data[:a.NonceSize()], data[a.NonceSize():], []byte(settingFOFA))
	if err != nil {
		return FOFAConfig{}, errors.New("FOFA 密钥不可用")
	}
	c.APIKey = string(plain)
	return c, nil
}

func (d *DB) FOFAConfig() (FOFAConfig, error) {
	raw, ok, err := d.GetSetting(settingFOFA)
	if err != nil || !ok {
		return defaultFOFAConfig(), err
	}
	return openFOFAConfig(raw)
}

func ValidFOFAKey(key string) bool {
	return len(key) > 0 && len(key) <= 512 && !strings.ContainsFunc(key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// 一个配置行内串行合并，避免并发修改开关时覆盖另一请求刚更换的令牌。
func (d *DB) SetFOFAConfig(ctx context.Context, patch FOFAConfigPatch) (FOFAConfig, error) {
	if patch.RequestIntervalSeconds != nil && (*patch.RequestIntervalSeconds < 1 || *patch.RequestIntervalSeconds > 60) {
		return FOFAConfig{}, ErrFOFARequestInterval
	}
	if patch.RateLimitCooldownSeconds != nil && (*patch.RateLimitCooldownSeconds < 1 || *patch.RateLimitCooldownSeconds > 3600) {
		return FOFAConfig{}, ErrFOFARateLimitCooldown
	}
	if patch.APIKey != nil {
		key := strings.TrimSpace(*patch.APIKey)
		if key != "" && !ValidFOFAKey(key) {
			return FOFAConfig{}, ErrFOFAAPIKey
		}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return FOFAConfig{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,'{}') ON CONFLICT(key) DO NOTHING`, settingFOFA); err != nil {
		return FOFAConfig{}, err
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1 FOR UPDATE`, settingFOFA).Scan(&raw); err != nil {
		return FOFAConfig{}, err
	}
	c, err := openFOFAConfig(raw)
	if err != nil {
		// 旧密钥丢失/损坏时仍允许管理员显式更换或清除，不降级读取明文。
		if patch.APIKey == nil {
			return FOFAConfig{}, err
		}
		stored, readErr := readFOFAStoredConfig(raw)
		if readErr != nil {
			return FOFAConfig{}, readErr
		}
		c = FOFAConfig{RequestIntervalSeconds: stored.RequestIntervalSeconds, RateLimitCooldownSeconds: stored.RateLimitCooldownSeconds}
	}
	if patch.RequestIntervalSeconds != nil {
		c.RequestIntervalSeconds = *patch.RequestIntervalSeconds
	}
	if patch.RateLimitCooldownSeconds != nil {
		c.RateLimitCooldownSeconds = *patch.RateLimitCooldownSeconds
	}
	if patch.Enabled != nil {
		c.Enabled = *patch.Enabled
	}
	if patch.APIKey != nil {
		c.APIKey = strings.TrimSpace(*patch.APIKey)
		if c.APIKey == "" {
			c.Enabled = false
		}
	}
	if c.Enabled && c.APIKey == "" {
		return FOFAConfig{}, ErrFOFAAPIKey
	}
	cipher, err := sealFOFAKey(c.APIKey)
	if err != nil {
		return FOFAConfig{}, err
	}
	b, err := json.Marshal(fofaStoredConfig{Enabled: c.Enabled, Cipher: cipher, RequestIntervalSeconds: c.RequestIntervalSeconds, RateLimitCooldownSeconds: c.RateLimitCooldownSeconds})
	if err != nil {
		return FOFAConfig{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settings SET value=$2,updated_at=now() WHERE key=$1`, settingFOFA, string(b)); err != nil {
		return FOFAConfig{}, err
	}
	if err = tx.Commit(); err != nil {
		return FOFAConfig{}, err
	}
	return c, nil
}
