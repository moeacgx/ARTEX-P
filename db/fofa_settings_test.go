package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// 每个用例使用独立 schema 和临时根密钥，支持多个连接验证并发写入。
func fofaSettingsTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	dsn, _, err := DSN()
	if err != nil {
		t.Skipf("未配置隔离 PostgreSQL: %v", err)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	schema := fmt.Sprintf("fofa_settings_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	cfg.RuntimeParams["search_path"] = schema
	pool := stdlib.OpenDB(*cfg)
	pool.SetMaxOpenConns(4)
	d := &DB{DB: pool}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Exec(`CREATE TABLE settings (key text PRIMARY KEY, value text NOT NULL, updated_at timestamptz DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	proxySecretsMu.Lock()
	previous := proxySecretsPath
	proxySecretsMu.Unlock()
	t.Cleanup(func() {
		proxySecretsMu.Lock()
		proxySecretsPath = previous
		proxySecretsMu.Unlock()
	})
	path := filepath.Join(t.TempDir(), "proxy-secrets.key")
	if err := ConfigureProxySecrets(path); err != nil {
		t.Fatal(err)
	}
	return d, path
}

func fofaStoredRaw(t *testing.T, d *DB) string {
	t.Helper()
	raw, ok, err := d.GetSetting(settingFOFA)
	if err != nil || !ok {
		t.Fatalf("读取持久配置失败: present=%v err=%v", ok, err)
	}
	return raw
}

func TestFOFAConfigEncryptedStorageAndJSON(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	key, enabled := "synthetic-fofa-never-echo", true
	c, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &key, Enabled: &enabled})
	if err != nil || !c.Enabled || c.APIKey != key {
		t.Fatalf("保存结果不正确: enabled=%v keyMatch=%v err=%v", c.Enabled, c.APIKey == key, err)
	}
	raw := fofaStoredRaw(t, d)
	if strings.Contains(raw, key) {
		t.Fatal("数据库明文出现 API Key")
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 4 || stored["enabled"] != true || stored["request_interval_seconds"] != float64(2) || stored["rate_limit_cooldown_seconds"] != float64(60) {
		t.Fatal("持久配置应包含开关、密文和节流设置")
	}
	if cipher, ok := stored["api_key_cipher"].(string); !ok || !strings.HasPrefix(cipher, "v1:") {
		t.Fatal("数据库未存储版本化密文")
	}
	encoded, err := json.Marshal(c)
	if err != nil || string(encoded) != `{"enabled":true,"request_interval_seconds":2,"rate_limit_cooldown_seconds":60}` {
		t.Fatalf("通用序列化不得携带令牌字段: err=%v", err)
	}
	reloaded, err := d.FOFAConfig()
	if err != nil || reloaded != c {
		t.Fatalf("配置持久化恢复失败: matches=%v err=%v", reloaded == c, err)
	}
}

func fofaConfigNumbers(t *testing.T, config FOFAConfig) map[string]any {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFOFAThrottleDefaultsAndLegacyConfig(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	for _, raw := range []string{"", `{}`, `{"enabled":false,"api_key_cipher":""}`} {
		if raw != "" {
			if err := d.SetSetting(settingFOFA, raw); err != nil {
				t.Fatal(err)
			}
		}
		config, err := d.FOFAConfig()
		if err != nil {
			t.Fatal(err)
		}
		got := fofaConfigNumbers(t, config)
		if got["request_interval_seconds"] != float64(2) || got["rate_limit_cooldown_seconds"] != float64(60) {
			t.Fatalf("缺省/旧配置未补齐节流默认值: %+v", got)
		}
	}
}

func TestFOFAThrottlePatchPreservesOtherFields(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	key, enabled := "synthetic-throttle-key", true
	if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &key, Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		patch              string
		interval, cooldown int
		enabled            bool
	}{
		{`{"request_interval_seconds":7}`, 7, 60, true},
		{`{"rate_limit_cooldown_seconds":180}`, 7, 180, true},
		{`{"enabled":false}`, 7, 180, false},
		{`{"api_key":"replacement-throttle-key"}`, 7, 180, false},
		{`{"enabled":true}`, 7, 180, true},
		{`{"request_interval_seconds":1,"rate_limit_cooldown_seconds":1}`, 1, 1, true},
		{`{"request_interval_seconds":60,"rate_limit_cooldown_seconds":3600}`, 60, 3600, true},
	} {
		var patch FOFAConfigPatch
		if err := json.Unmarshal([]byte(step.patch), &patch); err != nil {
			t.Fatal(err)
		}
		config, err := d.SetFOFAConfig(t.Context(), patch)
		if err != nil {
			t.Fatal(err)
		}
		got := fofaConfigNumbers(t, config)
		if config.APIKey == "" || config.Enabled != step.enabled || got["request_interval_seconds"] != float64(step.interval) || got["rate_limit_cooldown_seconds"] != float64(step.cooldown) {
			t.Fatalf("局部保存覆盖了其它字段: patch=%s got=%+v hasKey=%v", step.patch, got, config.APIKey != "")
		}
	}
}

func TestFOFAThrottleRejectInvalidInputWithoutWriting(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	key := "synthetic-unchanged-throttle-key"
	if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &key}); err != nil {
		t.Fatal(err)
	}
	before := fofaStoredRaw(t, d)
	for _, field := range []struct {
		name, label string
		max         int
	}{
		{"request_interval_seconds", "请求间隔", 60},
		{"rate_limit_cooldown_seconds", "限流冷却", 3600},
	} {
		for _, raw := range []string{"null", "0", "-1", "1.5", `"2"`, "true", fmt.Sprint(field.max + 1)} {
			var patch FOFAConfigPatch
			err := json.Unmarshal([]byte(fmt.Sprintf(`{"%s":%s}`, field.name, raw)), &patch)
			if err == nil {
				_, err = d.SetFOFAConfig(t.Context(), patch)
			}
			if err == nil || !errors.Is(err, ErrFOFAConfig) || !strings.Contains(err.Error(), field.label) {
				t.Fatalf("无效节流值未准确拒绝: field=%s input=%s err=%v", field.name, raw, err)
			}
			if fofaStoredRaw(t, d) != before {
				t.Fatal("无效参数改变了已有配置")
			}
		}
	}
}

func TestFOFAThrottleDirectPatchValidationAndConcurrentMerge(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	key, enabled := "synthetic-concurrent-throttle-key", true
	if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &key, Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	before := fofaStoredRaw(t, d)
	for _, value := range []int{0, -1, 61} {
		if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{RequestIntervalSeconds: &value}); !errors.Is(err, ErrFOFARequestInterval) {
			t.Fatalf("直接写入未拒绝无效请求间隔: %v", err)
		}
	}
	for _, value := range []int{0, -1, 3601} {
		if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{RateLimitCooldownSeconds: &value}); !errors.Is(err, ErrFOFARateLimitCooldown) {
			t.Fatalf("直接写入未拒绝无效限流冷却: %v", err)
		}
	}
	if fofaStoredRaw(t, d) != before {
		t.Fatal("无效直接更新改变了原配置")
	}
	interval, cooldown := 9, 240
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, patch := range []FOFAConfigPatch{{RequestIntervalSeconds: &interval}, {RateLimitCooldownSeconds: &cooldown}} {
		go func(p FOFAConfigPatch) { <-start; _, err := d.SetFOFAConfig(t.Context(), p); results <- err }(patch)
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.FOFAConfig()
	if err != nil || got.RequestIntervalSeconds != interval || got.RateLimitCooldownSeconds != cooldown || !got.Enabled || got.APIKey != key {
		t.Fatalf("并发局部更新丢失字段: err=%v", err)
	}
}

func TestFOFAThrottleLegacyEncryptedKeyAndReplacementPreserveSettings(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	key := "synthetic-legacy-fofa-key"
	cipher, err := sealFOFAKey(key)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(map[string]any{"enabled": true, "api_key_cipher": cipher})
	if err := d.SetSetting(settingFOFA, string(legacy)); err != nil {
		t.Fatal(err)
	}
	got, err := d.FOFAConfig()
	if err != nil || got.APIKey != key || !got.Enabled || got.RequestIntervalSeconds != 2 || got.RateLimitCooldownSeconds != 60 {
		t.Fatalf("旧密文配置未保留Key并补齐默认节流: %v", err)
	}
	if err := d.SetSetting(settingFOFA, `{"enabled":true,"api_key_cipher":"v1:broken-cipher","request_interval_seconds":11,"rate_limit_cooldown_seconds":300}`); err != nil {
		t.Fatal(err)
	}
	replacement := "synthetic-replacement-fofa-key"
	got, err = d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &replacement})
	if err != nil || got.RequestIntervalSeconds != 11 || got.RateLimitCooldownSeconds != 300 {
		t.Fatalf("替换损坏密文时重置了有效节流设置: %v", err)
	}
}

func TestFOFAConfigConcurrentKeyAndEnabledUpdates(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// 每轮先锁定配置行，让两个独立请求都在该行等待，再一起放行。
	// 无论哪一项先获锁，后续请求都必须读取更新后的配置再合并。
	for i := 0; i < 12; i++ {
		oldKey, oldEnabled := "synthetic-initial", false
		if _, err := d.SetFOFAConfig(ctx, FOFAConfigPatch{APIKey: &oldKey, Enabled: &oldEnabled}); err != nil {
			t.Fatal(err)
		}
		blocker, err := d.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := blocker.ExecContext(ctx, `SELECT value FROM settings WHERE key=$1 FOR UPDATE`, settingFOFA); err != nil {
			_ = blocker.Rollback()
			t.Fatal(err)
		}
		newKey, enabled := fmt.Sprintf("synthetic-replaced-%d", i), true
		patches := []FOFAConfigPatch{{APIKey: &newKey}, {Enabled: &enabled}}
		var started sync.WaitGroup
		started.Add(len(patches))
		errs := make(chan error, len(patches))
		for _, patch := range patches {
			go func(p FOFAConfigPatch) {
				started.Done()
				_, err := d.SetFOFAConfig(ctx, p)
				errs <- err
			}(patch)
		}
		started.Wait()
		if err := blocker.Commit(); err != nil {
			t.Fatal(err)
		}
		for range patches {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		got, err := d.FOFAConfig()
		if err != nil || !got.Enabled || got.APIKey != newKey {
			t.Fatalf("并发修改丢失: round=%d enabled=%v keyMatch=%v err=%v", i, got.Enabled, got.APIKey == newKey, err)
		}
	}
}

func TestFOFAConfigLostRootKeyFailsClosedAndCanReplace(t *testing.T) {
	d, path := fofaSettingsTestDB(t)
	key, enabled := "synthetic-before-key-loss", true
	if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &key, Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	raw := fofaStoredRaw(t, d)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := d.FOFAConfig(); err == nil || got.Enabled || got.APIKey != "" {
		t.Fatal("根密钥丢失后旧配置未失败关闭")
	}
	if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{Enabled: &enabled}); err == nil {
		t.Fatal("根密钥丢失后不能只改开关恢复旧密文")
	}
	if fofaStoredRaw(t, d) != raw {
		t.Fatal("失败操作改变了原密文")
	}
	replacement := "synthetic-after-key-loss"
	got, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &replacement, Enabled: &enabled})
	if err != nil || !got.Enabled || got.APIKey != replacement {
		t.Fatalf("显式替换失败: enabled=%v keyMatch=%v err=%v", got.Enabled, got.APIKey == replacement, err)
	}
}

func TestFOFAConfigDamagedCipherCanReplaceOrClear(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear_%v", clear), func(t *testing.T) {
			d, _ := fofaSettingsTestDB(t)
			const damaged = `{"enabled":true,"api_key_cipher":"v1:broken-cipher"}`
			if err := d.SetSetting(settingFOFA, damaged); err != nil {
				t.Fatal(err)
			}
			if got, err := d.FOFAConfig(); err == nil || got.Enabled || got.APIKey != "" {
				t.Fatal("损坏密文未失败关闭")
			}
			enabled := true
			if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{Enabled: &enabled}); err == nil || fofaStoredRaw(t, d) != damaged {
				t.Fatal("损坏密文允许无令牌开关修改或覆盖原配置")
			}
			key := "synthetic-replacement"
			if clear {
				key = ""
			}
			got, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &key, Enabled: &enabled})
			if err != nil || got.APIKey != key || got.Enabled != !clear {
				t.Fatalf("损坏密文显式修复失败: clear=%v enabled=%v keyMatch=%v err=%v", clear, got.Enabled, got.APIKey == key, err)
			}
		})
	}
}

func TestFOFAConfigDamagedRootKeyAllowsClearOnly(t *testing.T) {
	d, path := fofaSettingsTestDB(t)
	key, enabled := "synthetic-before-damaged-root", true
	if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &key, Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	raw := fofaStoredRaw(t, d)
	if err := os.WriteFile(path, []byte("damaged-root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := d.FOFAConfig(); err == nil || got.Enabled || got.APIKey != "" {
		t.Fatal("损坏根密钥未失败关闭")
	}
	replacement := "synthetic-replacement"
	if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &replacement}); err == nil || fofaStoredRaw(t, d) != raw {
		t.Fatal("损坏根密钥不应允许假成功替换 API Key")
	}
	empty := ""
	if got, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &empty}); err != nil || got.Enabled || got.APIKey != "" {
		t.Fatalf("清除不应依赖损坏根密钥: enabled=%v hasKey=%v err=%v", got.Enabled, got.APIKey != "", err)
	}
	if got, err := d.FOFAConfig(); err != nil || got.Enabled || got.APIKey != "" {
		t.Fatalf("清除后配置不可读: enabled=%v hasKey=%v err=%v", got.Enabled, got.APIKey != "", err)
	}
}

func TestFOFAConfigInvalidKeyPreservesPreviousValue(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	key, enabled := "synthetic-valid-key", true
	if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &key, Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	raw := fofaStoredRaw(t, d)
	for _, invalid := range []string{"a b", "a\nb", "a\tb", "a\x00b", strings.Repeat("x", 513)} {
		if _, err := d.SetFOFAConfig(t.Context(), FOFAConfigPatch{APIKey: &invalid}); err == nil {
			t.Fatal("无效令牌写入未被拒绝")
		}
		if fofaStoredRaw(t, d) != raw {
			t.Fatal("无效令牌修改了原持久配置")
		}
	}
}
