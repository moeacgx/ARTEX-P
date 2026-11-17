package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/integrations/xiequ"
)

const xiequQuotaMessage = "谐趣账户最多添加 5 个白名单，且对账户下所有产品生效。账户下仅存在按次计费或流量计费产品时，可向客服免费申请增加白名单配额；账户下存在包时、包日或包月计费产品时无法增加白名单限额，可以注册新账户并购买同类型产品来增加配额。"

type xiequRefreshResult struct {
	OK             bool       `json:"ok"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	PublicIP       string     `json:"public_ip,omitempty"`
	WhitelistCount int        `json:"whitelist_count"`
	Added          bool       `json:"added"`
	Removed        []string   `json:"removed,omitempty"`
	ManagedIPs     []string   `json:"managed_ips,omitempty"`
	CheckedAt      time.Time  `json:"checked_at"`
	LastSuccessAt  *time.Time `json:"last_success_at,omitempty"`
	Warning        string     `json:"warning,omitempty"`
}

var (
	newXiequClient = func(baseURL, uid, ukey string) *xiequ.Client {
		return xiequ.New(baseURL, uid, ukey)
	}
	discoverXiequPublicIP = func(ctx context.Context, client *xiequ.Client) (string, error) {
		return client.DiscoverPublicIP(ctx)
	}
)

// refreshXiequWhitelist is the single implementation used by the explicit tool,
// the settings API, and best-effort proxy-path refresh. force=false is used on
// the proxy hot path so a failed upstream is not hammered every few seconds.
func (m *Manager) refreshXiequWhitelist(ctx context.Context, force bool) (xiequRefreshResult, error) {
	result := xiequRefreshResult{CheckedAt: time.Now().UTC(), ManagedIPs: []string{}, Removed: []string{}}
	if m == nil || m.pg == nil {
		return result, errors.New("谐趣白名单配置存储不可用")
	}
	if force {
		m.xiequWhitelistMu.Lock()
	} else if !m.xiequWhitelistMu.TryLock() {
		result.Reason = "busy"
		result.Message = "已有一次谐趣白名单刷新正在执行，本次自动刷新已跳过"
		return result, nil
	}
	defer m.xiequWhitelistMu.Unlock()

	config, err := m.pg.XiequConfig()
	if err != nil {
		return result, err
	}
	if !config.Enabled || config.UID == "" || config.UKEY == "" {
		result.Reason = "disabled"
		result.Message = "谐趣白名单自动刷新未启用或 UID/UKEY 未配置"
		return result, nil
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, 3*time.Second)
	unlock, locked, err := m.pg.TryXiequRefreshLock(lockCtx, config.WhitelistScope())
	cancelLock()
	if err != nil {
		return result, err
	}
	if !locked {
		result.Reason = "busy"
		result.Message = "已有一次谐趣白名单刷新正在执行，本次刷新已跳过"
		return result, nil
	}
	defer unlock()
	state, err := m.pg.XiequWhitelistState()
	if err != nil {
		return result, err
	}
	// 在途网络请求始终使用本次快照；配置切换只允许新身份保存后续状态。
	state = state.ForConfig(config)
	if !force && state.LastAttemptAt != nil {
		interval := time.Duration(config.RefreshIntervalSeconds) * time.Second
		if interval > 0 && time.Since(*state.LastAttemptAt) < interval {
			result.Reason = "cooldown"
			result.Message = "距离上次谐趣白名单刷新未满冷却时间，本次自动刷新已跳过"
			result.LastSuccessAt = state.LastSuccessAt
			return result, nil
		}
	}

	attemptAt := time.Now().UTC()
	result.CheckedAt = attemptAt
	state.LastAttemptAt = &attemptAt
	// 网络取消前先落地冷却记录；失败则不发出任何上游请求。
	if err := m.saveXiequRefreshState(ctx, &state); err != nil {
		return result, err
	}
	client := newXiequClient(config.BaseURL, config.UID, config.UKEY)
	entries, err := client.List(ctx)
	if err != nil {
		return m.finishXiequRefreshError(ctx, state, result, "list_failed", "读取谐趣白名单失败", err)
	}
	// 仅随机操作标记一致的远端记录能够恢复为本安装管理记录。
	state.ManagedIPs = pruneXiequManagedIPs(state.ManagedIPs, entries)
	if pending := state.PendingAdd; pending != nil {
		for _, entry := range entries {
			if entry.IP == pending.IP && entry.Memo == pending.Memo {
				state.ManagedIPs = appendXiequManagedIP(state.ManagedIPs, pending.IP)
			}
		}
		state.PendingAdd = nil
		if err := m.saveXiequRefreshState(ctx, &state); err != nil {
			return result, err
		}
	}
	ip, err := discoverXiequPublicIP(ctx, client)
	if err != nil {
		return m.finishXiequRefreshError(ctx, state, result, "ip_probe_failed", "无法识别当前公网 IPv4 出口", err)
	}
	result.PublicIP = ip
	result.WhitelistCount = len(entries)
	state.LastIP = ip
	state.LastAttemptAt = &attemptAt
	state.WhitelistCount = len(entries)
	state.ManagedIPs = pruneXiequManagedIPs(state.ManagedIPs, entries)

	if entryPresent(entries, ip) {
		now := time.Now().UTC()
		state.LastStatus = "current"
		state.LastError = ""
		state.LastSuccessAt = &now
		result.OK = true
		result.Reason = "current"
		result.Message = "当前公网 IPv4 已在谐趣白名单中，无需修改"
		result.ManagedIPs = append([]string(nil), state.ManagedIPs...)
		result.LastSuccessAt = state.LastSuccessAt
		if err := m.saveXiequRefreshState(ctx, &state); err != nil {
			result.Warning = "白名单已确认，但本地状态保存失败"
		}
		return result, nil
	}

	// First attempt. The add endpoint is not trusted on HTTP status alone; every
	// outcome is verified by reading the list again.
	if err := m.prepareXiequAdd(ctx, &state, ip, config.Memo); err != nil {
		return result, err
	}
	addErr := client.Add(ctx, ip, state.PendingAdd.Memo)
	entriesAfterAdd, verifyErr := listXiequAfterAdd(ctx, client)
	if verifyErr == nil {
		result.WhitelistCount = len(entriesAfterAdd)
		state.WhitelistCount = len(entriesAfterAdd)
		state.ManagedIPs = pruneXiequManagedIPs(state.ManagedIPs, entriesAfterAdd)
		if xiequPendingEntryPresent(state.PendingAdd, entriesAfterAdd) {
			state.ManagedIPs = appendXiequManagedIP(state.ManagedIPs, ip)
			state.PendingAdd = nil
			return m.finishXiequRefreshSuccess(ctx, state, result, ip, true, "已添加当前公网 IPv4，并读取白名单确认生效")
		}
	}
	// 添加结果未知时保留 pending，不继续回收或重试远端变更。
	if verifyErr != nil {
		return m.finishXiequRefreshError(ctx, state, result, "add_unconfirmed", "添加结果尚未确认，将在下次刷新核验", verifyErr)
	}

	// Only managed records may be reclaimed, and only when the five-record quota
	// is full. Manual records and the "all" sentinel are never touched.
	currentEntries := entries
	if verifyErr == nil {
		currentEntries = entriesAfterAdd
	}
	if len(currentEntries) >= 5 {
		removed, deleteErr := m.removeStaleXiequManagedIPs(ctx, client, state.ManagedIPs, currentEntries, ip)
		if len(removed) > 0 {
			result.Removed = removed
			state.ManagedIPs = removeXiequManagedIPs(state.ManagedIPs, removed)
			entriesAfterCleanup, listErr := client.List(ctx)
			if listErr == nil {
				currentEntries = entriesAfterCleanup
				result.WhitelistCount = len(entriesAfterCleanup)
				state.WhitelistCount = len(entriesAfterCleanup)
				state.ManagedIPs = pruneXiequManagedIPs(state.ManagedIPs, entriesAfterCleanup)
			} else if deleteErr == nil {
				deleteErr = listErr
			}
			if deleteErr == nil && len(currentEntries) < 5 {
				addErr = client.Add(ctx, ip, state.PendingAdd.Memo)
				entriesAfterRetry, retryErr := listXiequAfterAdd(ctx, client)
				if retryErr == nil {
					currentEntries = entriesAfterRetry
					result.WhitelistCount = len(entriesAfterRetry)
					state.WhitelistCount = len(entriesAfterRetry)
					state.ManagedIPs = pruneXiequManagedIPs(state.ManagedIPs, entriesAfterRetry)
					if xiequPendingEntryPresent(state.PendingAdd, entriesAfterRetry) {
						state.ManagedIPs = appendXiequManagedIP(state.ManagedIPs, ip)
						state.PendingAdd = nil
						return m.finishXiequRefreshSuccess(ctx, state, result, ip, true, "已回收本工具管理的旧白名单记录并添加当前公网 IPv4，读取白名单确认生效")
					}
				}
			}
		}
	}

	if len(currentEntries) >= 5 {
		return m.finishXiequRefreshError(ctx, state, result, "quota_full", xiequQuotaMessage, addErr)
	}
	if addErr != nil {
		return m.finishXiequRefreshError(ctx, state, result, "add_failed", "添加当前公网 IPv4 失败", addErr)
	}
	return m.finishXiequRefreshError(ctx, state, result, "add_unconfirmed", "添加请求已发送，但重新读取白名单仍未确认当前 IP，未判定为成功", nil)
}

func (m *Manager) finishXiequRefreshSuccess(ctx context.Context, state db.XiequWhitelistState, result xiequRefreshResult, ip string, added bool, message string) (xiequRefreshResult, error) {
	now := time.Now().UTC()
	state.LastIP = ip
	state.LastStatus = "added"
	state.LastError = ""
	state.LastSuccessAt = &now
	state.LastAttemptAt = &now
	result.OK = true
	result.Reason = "added"
	result.Message = message
	result.Added = added
	result.PublicIP = ip
	result.ManagedIPs = append([]string(nil), state.ManagedIPs...)
	result.LastSuccessAt = state.LastSuccessAt
	if err := m.saveXiequRefreshState(ctx, &state); err != nil {
		result.Warning = "白名单已确认，但本地状态保存失败"
	}
	return result, nil
}

func (m *Manager) finishXiequRefreshError(ctx context.Context, state db.XiequWhitelistState, result xiequRefreshResult, reason, message string, cause error) (xiequRefreshResult, error) {
	now := time.Now().UTC()
	state.LastAttemptAt = &now
	state.LastStatus = reason
	state.LastError = message
	if cause != nil {
		state.LastError = message + "：" + cause.Error()
	}
	if len(state.LastError) > 512 {
		state.LastError = state.LastError[:512]
	}
	result.OK = false
	result.Reason = reason
	result.Message = message
	result.ManagedIPs = append([]string(nil), state.ManagedIPs...)
	result.LastSuccessAt = state.LastSuccessAt
	if err := m.saveXiequRefreshState(ctx, &state); err != nil {
		result.Warning = "本地状态保存失败"
	}
	return result, nil
}

func (m *Manager) saveXiequRefreshState(ctx context.Context, state *db.XiequWhitelistState) error {
	// 取消网络请求不应丢失冷却或已发送操作的状态；写回仍须通过身份及状态 CAS。
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := m.pg.SetXiequWhitelistState(saveCtx, *state); err != nil {
		return err
	}
	state.Revision++
	return nil
}

func (m *Manager) prepareXiequAdd(ctx context.Context, state *db.XiequWhitelistState, ip, memo string) error {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	marker := " [artex:" + hex.EncodeToString(token) + "]"
	for len(memo)+len(marker) > 80 {
		runes := []rune(memo)
		memo = string(runes[:len(runes)-1])
	}
	state.PendingAdd = &db.XiequPendingAdd{IP: ip, Memo: strings.TrimSpace(memo) + marker}
	return m.saveXiequRefreshState(ctx, state)
}

func xiequPendingEntryPresent(pending *db.XiequPendingAdd, entries []xiequ.Entry) bool {
	if pending == nil {
		return false
	}
	for _, entry := range entries {
		if entry.IP == pending.IP && entry.Memo == pending.Memo {
			return true
		}
	}
	return false
}

func listXiequAfterAdd(ctx context.Context, client *xiequ.Client) ([]xiequ.Entry, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		entries, err := client.List(ctx)
		if err == nil {
			return entries, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (m *Manager) removeStaleXiequManagedIPs(ctx context.Context, client *xiequ.Client, managed []string, entries []xiequ.Entry, current string) ([]string, error) {
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		present[entry.IP] = true
	}
	managedSet := make(map[string]bool, len(managed))
	for _, ip := range managed {
		managedSet[ip] = true
	}
	removed := []string{}
	var firstErr error
	for _, entry := range entries {
		if entry.IP == current || !managedSet[entry.IP] {
			continue
		}
		if err := client.Delete(ctx, entry.IP); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed = append(removed, entry.IP)
		delete(present, entry.IP)
	}
	return removed, firstErr
}

func entryPresent(entries []xiequ.Entry, ip string) bool {
	for _, entry := range entries {
		if entry.IP == ip {
			return true
		}
	}
	return false
}

func pruneXiequManagedIPs(managed []string, entries []xiequ.Entry) []string {
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		present[entry.IP] = true
	}
	out := make([]string, 0, len(managed))
	for _, ip := range managed {
		if present[ip] {
			out = append(out, ip)
		}
	}
	return out
}

func appendXiequManagedIP(managed []string, ip string) []string {
	for _, existing := range managed {
		if existing == ip {
			return managed
		}
	}
	return append(managed, ip)
}

func removeXiequManagedIPs(managed, removed []string) []string {
	drop := make(map[string]bool, len(removed))
	for _, ip := range removed {
		drop[ip] = true
	}
	out := make([]string, 0, len(managed))
	for _, ip := range managed {
		if !drop[ip] {
			out = append(out, ip)
		}
	}
	return out
}

func (m *Manager) maybeAutoRefreshXiequWhitelist(ctx context.Context, snapshot db.ProxySnapshot) {
	if m == nil || m.pg == nil || snapshot.ID == 0 || ctx.Err() != nil {
		return
	}
	config, err := m.pg.XiequConfig()
	if err != nil || !config.Enabled || config.UID == "" || config.UKEY == "" {
		return
	}
	host := snapshot.Host
	if host == "" && snapshot.URL != "" {
		if u, err := url.Parse(snapshot.URL); err == nil {
			host = u.Hostname()
		}
	}
	if !db.XiequProxyDomainMatches(host, config.ProxyDomains) {
		return
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := m.refreshXiequWhitelist(refreshCtx, false)
	if err != nil {
		log.Printf("[xiequ] 自动刷新白名单失败: %v", err)
		return
	}
	if result.OK && result.Added {
		log.Printf("[xiequ] 自动刷新白名单成功: %s", result.PublicIP)
	}
}

func (m *Manager) xiequStateView() map[string]any {
	state, err := m.pg.XiequWhitelistState()
	if err != nil {
		return map[string]any{"error": "谐趣白名单状态读取失败"}
	}
	return map[string]any{
		"managed_ips":     state.ManagedIPs,
		"last_ip":         state.LastIP,
		"last_status":     state.LastStatus,
		"last_error":      state.LastError,
		"whitelist_count": state.WhitelistCount,
		"last_attempt_at": state.LastAttemptAt,
		"last_success_at": state.LastSuccessAt,
	}
}

func xiequQuotaHint() string {
	return fmt.Sprintf("配额说明：%s", xiequQuotaMessage)
}
