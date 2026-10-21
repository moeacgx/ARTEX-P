package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

const settingFOFA = "integration.fofa.v1"

var ErrFOFAConfig = errors.New("FOFA 配置无效：请填写有效的 API Key 后启用")

// FOFAConfig 的明文令牌仅供受信服务端调用，禁止通用 JSON 序列化返回。
type FOFAConfig struct {
	Enabled bool   `json:"enabled"`
	APIKey  string `json:"-"`
}

type FOFAConfigPatch struct {
	Enabled *bool   `json:"enabled"`
	APIKey  *string `json:"api_key"`
}

type fofaStoredConfig struct {
	Enabled bool   `json:"enabled"`
	Cipher  string `json:"api_key_cipher"`
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
	var stored fofaStoredConfig
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return FOFAConfig{}, errors.New("FOFA 配置无法读取")
	}
	c := FOFAConfig{Enabled: stored.Enabled}
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
		return FOFAConfig{}, err
	}
	return openFOFAConfig(raw)
}

func ValidFOFAKey(key string) bool {
	return len(key) > 0 && len(key) <= 512 && !strings.ContainsFunc(key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// 一个配置行内串行合并，避免并发修改开关时覆盖另一请求刚更换的令牌。
func (d *DB) SetFOFAConfig(ctx context.Context, patch FOFAConfigPatch) (FOFAConfig, error) {
	if patch.APIKey != nil {
		key := strings.TrimSpace(*patch.APIKey)
		if key != "" && !ValidFOFAKey(key) {
			return FOFAConfig{}, ErrFOFAConfig
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
		c = FOFAConfig{}
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
		return FOFAConfig{}, ErrFOFAConfig
	}
	cipher, err := sealFOFAKey(c.APIKey)
	if err != nil {
		return FOFAConfig{}, err
	}
	b, err := json.Marshal(fofaStoredConfig{Enabled: c.Enabled, Cipher: cipher})
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
