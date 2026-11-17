package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	settingXiequ                = "integration.xiequ.v1"
	settingXiequWhitelistState  = "integration.xiequ.whitelist.v1"
	DefaultXiequBaseURL         = "https://op.xiequ.cn/IpWhiteList.aspx"
	DefaultXiequMemo            = "ARTEX auto"
	DefaultXiequRefreshInterval = 300
	minXiequRefreshInterval     = 30
	maxXiequRefreshInterval     = 3600
	maxXiequManagedStateIPs     = 32
	maxXiequProxyDomains        = 16
	maxXiequLastErrorBytes      = 512
)

var (
	ErrXiequConfig          = errors.New("谐趣白名单配置无效")
	ErrXiequConfigChanged   = errors.New("谐趣白名单配置已变更，拒绝旧刷新状态")
	ErrXiequStateChanged    = errors.New("谐趣白名单状态已变更，拒绝并发刷新状态")
	ErrXiequStateUnreadable = errors.New("谐趣白名单状态无法读取")
	ErrXiequUID             = fmt.Errorf("%w：请填写有效的 UID", ErrXiequConfig)
	ErrXiequUKEY            = fmt.Errorf("%w：请填写有效的 UKEY", ErrXiequConfig)
	ErrXiequBaseURL         = fmt.Errorf("%w：接口地址无效", ErrXiequConfig)
	ErrXiequMemo            = fmt.Errorf("%w：备注无效", ErrXiequConfig)
	ErrXiequRefreshInterval = fmt.Errorf("%w：刷新间隔必须为 30–3600 秒", ErrXiequConfig)
	ErrXiequProxyDomains    = fmt.Errorf("%w：代理域名列表无效", ErrXiequConfig)
)

// XiequConfig's UID and UKEY are server-side values and must not be serialized
// into generic API responses. The settings handler builds an explicit DTO.
type XiequConfig struct {
	Revision               string   `json:"-"`
	Enabled                bool     `json:"enabled"`
	UID                    string   `json:"-"`
	UKEY                   string   `json:"-"`
	BaseURL                string   `json:"base_url"`
	Memo                   string   `json:"memo"`
	RefreshIntervalSeconds int      `json:"refresh_interval_seconds"`
	ProxyDomains           []string `json:"proxy_domains"`
}

type XiequConfigPatch struct {
	Enabled                *bool     `json:"enabled"`
	UID                    *string   `json:"uid"`
	UKEY                   *string   `json:"ukey"`
	BaseURL                *string   `json:"base_url"`
	Memo                   *string   `json:"memo"`
	RefreshIntervalSeconds *int      `json:"refresh_interval_seconds"`
	ProxyDomains           *[]string `json:"proxy_domains"`
}

type xiequStoredConfig struct {
	Revision               string   `json:"revision,omitempty"`
	Enabled                bool     `json:"enabled"`
	UID                    string   `json:"uid"`
	UKEYCipher             string   `json:"ukey_cipher"`
	BaseURL                string   `json:"base_url"`
	Memo                   string   `json:"memo"`
	RefreshIntervalSeconds int      `json:"refresh_interval_seconds"`
	ProxyDomains           []string `json:"proxy_domains"`
}

// ManagedIPs 用于展示；自动删除必须另有 ManagedRecords 中的完整归属证据。
type XiequWhitelistState struct {
	Revision       uint64               `json:"state_revision,omitempty"`
	Scope          string               `json:"scope,omitempty"`
	ManagedIPs     []string             `json:"managed_ips,omitempty"`
	ManagedRecords []XiequManagedRecord `json:"managed_records,omitempty"`
	PendingAdd     *XiequPendingAdd     `json:"pending_add,omitempty"`
	LastIP         string               `json:"last_ip,omitempty"`
	LastStatus     string               `json:"last_status,omitempty"`
	LastError      string               `json:"last_error,omitempty"`
	WhitelistCount int                  `json:"whitelist_count,omitempty"`
	LastAttemptAt  *time.Time           `json:"last_attempt_at,omitempty"`
	LastSuccessAt  *time.Time           `json:"last_success_at,omitempty"`
}

// XiequPendingAdd 仅记录当前安装、账号及配置代次发出的待确认添加。
// Memo 包含随机操作标记，恢复时不能仅凭同一个 IP 认领手工记录。
type XiequPendingAdd struct {
	IP       string `json:"ip"`
	Memo     string `json:"memo"`
	Prepared bool   `json:"prepared,omitempty"`
}

// XiequManagedRecord 保留本安装在该配置代次确认的完整归属证据。
// 旧版只有 IP 的记录不具有自动删除权限。
type XiequManagedRecord struct {
	IP    string `json:"ip"`
	Memo  string `json:"memo"`
	Scope string `json:"scope"`
}

func defaultXiequProxyDomains() []string {
	return []string{"xiequ.cn"}
}

func defaultXiequConfig() XiequConfig {
	return XiequConfig{
		BaseURL:                DefaultXiequBaseURL,
		Memo:                   DefaultXiequMemo,
		RefreshIntervalSeconds: DefaultXiequRefreshInterval,
		ProxyDomains:           defaultXiequProxyDomains(),
	}
}

func readXiequStored(raw string) (xiequStoredConfig, error) {
	stored := xiequStoredConfig{
		BaseURL:                DefaultXiequBaseURL,
		Memo:                   DefaultXiequMemo,
		RefreshIntervalSeconds: DefaultXiequRefreshInterval,
		ProxyDomains:           defaultXiequProxyDomains(),
	}
	if strings.TrimSpace(raw) == "" {
		return stored, nil
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return stored, errors.New("谐趣白名单配置无法读取")
	}
	if stored.BaseURL == "" {
		stored.BaseURL = DefaultXiequBaseURL
	}
	if stored.Memo == "" {
		stored.Memo = DefaultXiequMemo
	}
	if stored.RefreshIntervalSeconds == 0 {
		stored.RefreshIntervalSeconds = DefaultXiequRefreshInterval
	}
	domains, err := normalizeXiequProxyDomains(stored.ProxyDomains)
	if err != nil {
		return stored, err
	}
	stored.ProxyDomains = domains
	return stored, nil
}

func sealXiequUKEY(key string) (string, error) {
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
	return "v1:" + hex.EncodeToString(a.Seal(nonce, nonce, []byte(key), []byte(settingXiequ))), nil
}

func openXiequConfig(raw string) (XiequConfig, error) {
	stored, err := readXiequStored(raw)
	if err != nil {
		return XiequConfig{}, err
	}
	c := XiequConfig{
		Revision:               stored.Revision,
		Enabled:                stored.Enabled,
		UID:                    stored.UID,
		BaseURL:                stored.BaseURL,
		Memo:                   stored.Memo,
		RefreshIntervalSeconds: stored.RefreshIntervalSeconds,
		ProxyDomains:           stored.ProxyDomains,
	}
	if stored.UKEYCipher == "" {
		return c, nil
	}
	if !strings.HasPrefix(stored.UKEYCipher, "v1:") {
		return XiequConfig{}, errors.New("谐趣 UKEY 密文格式无效")
	}
	data, err := hex.DecodeString(strings.TrimPrefix(stored.UKEYCipher, "v1:"))
	if err != nil {
		return XiequConfig{}, errors.New("谐趣 UKEY 密文格式无效")
	}
	a, err := proxyCipher()
	if err != nil {
		return XiequConfig{}, errors.New("谐趣 UKEY 密钥不可用")
	}
	if len(data) < a.NonceSize()+a.Overhead() {
		return XiequConfig{}, errors.New("谐趣 UKEY 密钥不可用")
	}
	plain, err := a.Open(nil, data[:a.NonceSize()], data[a.NonceSize():], []byte(settingXiequ))
	if err != nil {
		return XiequConfig{}, errors.New("谐趣 UKEY 密钥不可用")
	}
	c.UKEY = string(plain)
	return c, nil
}

func (d *DB) XiequConfig() (XiequConfig, error) {
	raw, ok, err := d.GetSetting(settingXiequ)
	if err != nil || !ok {
		return defaultXiequConfig(), err
	}
	return openXiequConfig(raw)
}

func ValidXiequUID(uid string) bool {
	if len(uid) == 0 || len(uid) > 64 || strings.TrimSpace(uid) != uid {
		return false
	}
	for _, r := range uid {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func ValidXiequUKEY(key string) bool {
	return len(key) > 0 && len(key) <= 512 && !strings.ContainsFunc(key, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

func validXiequBaseURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 2048 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	return true
}

func validXiequMemo(memo string) bool {
	return len(memo) <= 80 && !strings.ContainsFunc(memo, unicode.IsControl)
}

func normalizeXiequProxyDomains(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return defaultXiequProxyDomains(), nil
	}
	if len(raw) > maxXiequProxyDomains {
		return nil, ErrXiequProxyDomains
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		domain := strings.ToLower(strings.TrimSpace(value))
		domain = strings.TrimPrefix(domain, "*.")
		domain = strings.TrimPrefix(domain, ".")
		domain = strings.TrimSuffix(domain, ".")
		if !validXiequProxyDomain(domain) {
			return nil, ErrXiequProxyDomains
		}
		if seen[domain] {
			continue
		}
		seen[domain] = true
		out = append(out, domain)
	}
	if len(out) == 0 {
		return defaultXiequProxyDomains(), nil
	}
	sort.Strings(out)
	return out, nil
}

func validXiequProxyDomain(domain string) bool {
	if len(domain) == 0 || len(domain) > 253 {
		return false
	}
	if net.ParseIP(domain) != nil {
		return true
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// XiequProxyDomainMatches reports whether host is one of the configured
// domains or one of their subdomains. Callers pass a parsed hostname.
func XiequProxyDomainMatches(host string, domains []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = strings.Trim(h, "[]")
	}
	for _, value := range domains {
		domain := strings.ToLower(strings.TrimSpace(value))
		domain = strings.TrimPrefix(domain, "*.")
		domain = strings.TrimPrefix(domain, ".")
		domain = strings.TrimSuffix(domain, ".")
		if domain == "" {
			continue
		}
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func normalizeXiequState(state XiequWhitelistState) XiequWhitelistState {
	seen := map[string]bool{}
	ips := make([]string, 0, len(state.ManagedIPs))
	for _, raw := range state.ManagedIPs {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if ip == nil || ip.To4() == nil {
			continue
		}
		normalized := ip.String()
		if !seen[normalized] {
			seen[normalized] = true
			ips = append(ips, normalized)
		}
	}
	sort.Strings(ips)
	if len(ips) > maxXiequManagedStateIPs {
		ips = ips[:maxXiequManagedStateIPs]
	}
	state.ManagedIPs = ips
	if pending := state.PendingAdd; pending != nil {
		ip := net.ParseIP(pending.IP)
		if ip == nil || ip.To4() == nil || pending.Memo == "" || !validXiequMemo(pending.Memo) {
			state.PendingAdd = nil
		} else {
			state.PendingAdd = &XiequPendingAdd{IP: ip.String(), Memo: pending.Memo, Prepared: pending.Prepared}
		}
	}
	records := make([]XiequManagedRecord, 0, len(state.ManagedRecords))
	owned := map[string]bool{}
	for _, record := range state.ManagedRecords {
		ip := net.ParseIP(record.IP)
		if ip == nil || ip.To4() == nil || record.Scope != state.Scope || record.Memo == "" || !validXiequMemo(record.Memo) || owned[ip.String()] {
			continue
		}
		record.IP = ip.String()
		owned[record.IP] = true
		records = append(records, record)
		if len(records) == maxXiequManagedStateIPs {
			break
		}
	}
	state.ManagedRecords = records
	if ip := net.ParseIP(strings.TrimSpace(state.LastIP)); ip != nil && ip.To4() != nil {
		state.LastIP = ip.String()
	} else {
		state.LastIP = ""
	}
	if len(state.LastError) > maxXiequLastErrorBytes {
		state.LastError = state.LastError[:maxXiequLastErrorBytes]
	}
	return state
}

func (d *DB) XiequWhitelistState() (XiequWhitelistState, error) {
	raw, ok, err := d.GetSetting(settingXiequWhitelistState)
	if err != nil {
		return XiequWhitelistState{}, err
	}
	var state XiequWhitelistState
	if ok {
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return XiequWhitelistState{}, ErrXiequStateUnreadable
		}
	}
	config, err := d.XiequConfig()
	if err != nil {
		return XiequWhitelistState{}, err
	}
	state = normalizeXiequState(state).ForConfig(config)
	readCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	op, err := d.XiequOperation(readCtx, config)
	if err != nil {
		return XiequWhitelistState{}, err
	}
	if op != nil {
		state.LastStatus = "operation_unknown"
		state.LastError = "账号存在未确认完成的远端操作；已阻止后续变更，需可验证的完成证据"
	}
	return state, nil
}

func (d *DB) SetXiequWhitelistState(ctx context.Context, state XiequWhitelistState) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", settingXiequ).Scan(&raw); err != nil {
		return err
	}
	config, err := openXiequConfig(raw)
	if err != nil {
		return err
	}
	if state.Scope == "" || state.Scope != config.WhitelistScope() {
		return ErrXiequConfigChanged
	}
	var previousRaw string
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", settingXiequWhitelistState).Scan(&previousRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var previous XiequWhitelistState
	if err == nil {
		if err := json.Unmarshal([]byte(previousRaw), &previous); err != nil {
			return errors.New("谐趣白名单状态无法读取")
		}
	}
	previous = previous.ForConfig(config)
	if state.Revision != previous.Revision {
		return ErrXiequStateChanged
	}
	state.Revision++
	state = normalizeXiequState(state)
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO settings(key,value) VALUES($1,$2)
ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()`, settingXiequWhitelistState, string(b))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (c XiequConfig) accountIdentity() string {
	endpoint, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err == nil {
		endpoint.Scheme = strings.ToLower(endpoint.Scheme)
		endpoint.Host = strings.ToLower(endpoint.Host)
		if (endpoint.Scheme == "http" && endpoint.Port() == "80") || (endpoint.Scheme == "https" && endpoint.Port() == "443") {
			endpoint.Host = endpoint.Hostname()
			if strings.Contains(endpoint.Host, ":") {
				endpoint.Host = "[" + endpoint.Host + "]"
			}
		}
		if endpoint.Path == "" {
			endpoint.Path = "/"
		}
	}
	address := strings.TrimSpace(c.BaseURL)
	if err == nil {
		address = endpoint.String()
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(c.UID) + "\n" + address))
	return hex.EncodeToString(digest[:])
}

func (c XiequConfig) WhitelistScope() string {
	return c.accountIdentity() + ":" + c.Revision
}

// TryXiequRefreshLock 仅优化同一身份代次的刷新互斥，不能证明远端操作已退出。
// 真正的远端变更准入必须另外取得持久账号操作屏障。
// 会话锁使用独占连接，释放失败时丢弃连接，禁止把残留锁放回连接池。
func (d *DB) TryXiequRefreshLock(ctx context.Context, scope string) (func(), bool, error) {
	conn, err := d.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	key := "xiequ-refresh:" + scope
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, key).Scan(&locked); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		conn.Close()
		return nil, false, err
	}
	if !locked {
		conn.Close()
		return nil, false, nil
	}
	return func() {
		defer conn.Close()
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key).Scan(&unlocked); err != nil || !unlocked {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}, true, nil
}

func (s XiequWhitelistState) ForConfig(c XiequConfig) XiequWhitelistState {
	if s.Scope != c.WhitelistScope() {
		return XiequWhitelistState{Scope: c.WhitelistScope()}
	}
	return s
}

// SetXiequConfig merges a patch into one locked settings row so changing the
// enable switch cannot overwrite a concurrently replaced UKEY.
func (d *DB) SetXiequConfig(ctx context.Context, patch XiequConfigPatch) (XiequConfig, error) {
	if patch.RefreshIntervalSeconds != nil && (*patch.RefreshIntervalSeconds < minXiequRefreshInterval || *patch.RefreshIntervalSeconds > maxXiequRefreshInterval) {
		return XiequConfig{}, ErrXiequRefreshInterval
	}
	if patch.UID != nil && *patch.UID != "" && !ValidXiequUID(strings.TrimSpace(*patch.UID)) {
		return XiequConfig{}, ErrXiequUID
	}
	if patch.UKEY != nil && *patch.UKEY != "" && !ValidXiequUKEY(strings.TrimSpace(*patch.UKEY)) {
		return XiequConfig{}, ErrXiequUKEY
	}
	var domains []string
	var err error
	if patch.ProxyDomains != nil {
		domains, err = normalizeXiequProxyDomains(*patch.ProxyDomains)
		if err != nil {
			return XiequConfig{}, err
		}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return XiequConfig{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,'{}') ON CONFLICT(key) DO NOTHING`, settingXiequ); err != nil {
		return XiequConfig{}, err
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1 FOR UPDATE`, settingXiequ).Scan(&raw); err != nil {
		return XiequConfig{}, err
	}
	c, err := openXiequConfig(raw)
	if err != nil {
		if patch.UKEY == nil {
			return XiequConfig{}, err
		}
		c = defaultXiequConfig()
	}
	previous := c
	// 配置变更前把旧版已发送 pending 纳入账号屏障，不能靠换代次丢弃。
	if err := preserveXiequLegacyPending(ctx, tx, previous); err != nil {
		return XiequConfig{}, err
	}
	if patch.Enabled != nil {
		c.Enabled = *patch.Enabled
	}
	if patch.UID != nil {
		c.UID = strings.TrimSpace(*patch.UID)
	}
	if patch.UKEY != nil {
		c.UKEY = strings.TrimSpace(*patch.UKEY)
		if c.UKEY == "" {
			c.Enabled = false
		}
	}
	if patch.BaseURL != nil {
		c.BaseURL = strings.TrimSpace(*patch.BaseURL)
		if c.BaseURL == "" {
			c.BaseURL = DefaultXiequBaseURL
		}
	}
	if patch.Memo != nil {
		c.Memo = strings.TrimSpace(*patch.Memo)
		if c.Memo == "" {
			c.Memo = DefaultXiequMemo
		}
	}
	if patch.RefreshIntervalSeconds != nil {
		c.RefreshIntervalSeconds = *patch.RefreshIntervalSeconds
	}
	if patch.ProxyDomains != nil {
		c.ProxyDomains = domains
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultXiequBaseURL
	}
	if c.Memo == "" {
		c.Memo = DefaultXiequMemo
	}
	if c.RefreshIntervalSeconds == 0 {
		c.RefreshIntervalSeconds = DefaultXiequRefreshInterval
	}
	if len(c.ProxyDomains) == 0 {
		c.ProxyDomains = defaultXiequProxyDomains()
	}
	if !validXiequBaseURL(c.BaseURL) {
		return XiequConfig{}, ErrXiequBaseURL
	}
	if !validXiequMemo(c.Memo) {
		return XiequConfig{}, ErrXiequMemo
	}
	if c.UID != "" && !ValidXiequUID(c.UID) {
		return XiequConfig{}, ErrXiequUID
	}
	if c.UKEY != "" && !ValidXiequUKEY(c.UKEY) {
		return XiequConfig{}, ErrXiequUKEY
	}
	if c.Enabled && (c.UID == "" || c.UKEY == "") {
		return XiequConfig{}, ErrXiequConfig
	}
	identityChanged := previous.accountIdentity() != c.accountIdentity() || previous.UKEY != c.UKEY || c.Revision == ""
	if identityChanged {
		generation := make([]byte, 16)
		if _, err := rand.Read(generation); err != nil {
			return XiequConfig{}, err
		}
		c.Revision = hex.EncodeToString(generation)
	}
	cipher, err := sealXiequUKEY(c.UKEY)
	if err != nil {
		return XiequConfig{}, err
	}
	b, err := json.Marshal(xiequStoredConfig{
		Revision:               c.Revision,
		Enabled:                c.Enabled,
		UID:                    c.UID,
		UKEYCipher:             cipher,
		BaseURL:                c.BaseURL,
		Memo:                   c.Memo,
		RefreshIntervalSeconds: c.RefreshIntervalSeconds,
		ProxyDomains:           c.ProxyDomains,
	})
	if err != nil {
		return XiequConfig{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settings SET value=$2,updated_at=now() WHERE key=$1`, settingXiequ, string(b)); err != nil {
		return XiequConfig{}, err
	}
	if identityChanged {
		state, err := json.Marshal(XiequWhitelistState{Scope: c.WhitelistScope()})
		if err != nil {
			return XiequConfig{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()", settingXiequWhitelistState, string(state)); err != nil {
			return XiequConfig{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return XiequConfig{}, err
	}
	return c, nil
}
