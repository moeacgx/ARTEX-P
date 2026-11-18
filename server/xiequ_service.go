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

const xiequQuotaMessage = "谐趣账户最多添加 5 个白名单，且对账户下所有产品生效。配额已满，请由用户在供应商后台人工处理后重试；本工具不会自动删除或回收任何远端条目。账户下仅存在按次计费或流量计费产品时，可向客服免费申请增加白名单配额；账户下存在包时、包日或包月计费产品时无法增加白名单限额，可以注册新账户并购买同类型产品来增加配额。"

type xiequRefreshResult struct {
	OK             bool       `json:"ok"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	PublicIP       string     `json:"public_ip,omitempty"`
	WhitelistCount int        `json:"whitelist_count"`
	Added          bool       `json:"added"`
	Removed        []string   `json:"removed"`
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
		// 同一账号的请求可能已发送；即使互斥锁仍在，也应准确显示持久未知屏障。
		readCtx, cancelRead := context.WithTimeout(ctx, 3*time.Second)
		op, readErr := m.pg.XiequOperation(readCtx, config)
		cancelRead()
		if readErr != nil {
			return result, readErr
		}
		if op != nil {
			result.Reason = "operation_unknown"
			result.Message = "账号存在未确认完成的远端操作；已阻止后续变更，需可验证的完成证据"
			return result, nil
		}
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
	// 旧版 pending 迁移和账号屏障不依赖 advisory 连接的存活。
	guardCtx, cancelGuard := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	err = m.pg.PreserveXiequPending(guardCtx, config)
	cancelGuard()
	if errors.Is(err, db.ErrXiequConfigChanged) {
		return m.finishXiequRefreshError(ctx, state, result, "config_changed", "配置已变更，旧刷新停止后续操作", nil)
	}
	if err != nil {
		return result, err
	}
	op, err := m.pg.XiequOperation(ctx, config)
	if err != nil {
		return result, err
	}
	pruneXiequManagedRecords(&state, entries)
	if op != nil {
		// marker 阳性只证明归属，不证明失联调用已退出，不能释放屏障。
		if op.Kind == "add" && op.Scope == state.Scope {
			state.PendingAdd = &db.XiequPendingAdd{IP: op.IP, Memo: op.Memo}
			if xiequPendingEntryPresent(state.PendingAdd, entries) {
				confirmXiequManagedRecord(&state, *state.PendingAdd)
			}
		}
		result.WhitelistCount = len(entries)
		return m.finishXiequRefreshError(ctx, state, result, "operation_unknown", "账号存在未确认完成的远端操作；仅核验归属，已阻止后续变更，需可验证的完成证据", nil)
	}
	// prepared 表示尚未取得发送许可。清除和发送许可都受状态 CAS 约束。
	if state.PendingAdd != nil && state.PendingAdd.Prepared {
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
	pruneXiequManagedRecords(&state, entries)

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

	// 供应商仅按 IP 删除，没有原子身份条件；重新读列表仍无法保护手工重建条目。
	// 保留归属跟踪，但满额时不再发出任何 Add/Del。
	if len(entries) >= 5 {
		return m.finishXiequRefreshError(ctx, state, result, "quota_full", xiequQuotaMessage, nil)
	}
	if err := m.prepareXiequAdd(ctx, &state, ip, config.Memo); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	token, err := xiequOperationToken()
	if err != nil {
		return result, err
	}
	operation := db.XiequOperation{Token: token, Scope: config.WhitelistScope(), Kind: "add", IP: ip, Memo: state.PendingAdd.Memo}
	if err := m.pg.ClaimXiequOperation(ctx, config, state, operation); err != nil {
		return result, err
	}
	state.Revision++
	state.PendingAdd.Prepared = false
	addErr := client.Add(ctx, ip, operation.Memo)
	entriesAfterAdd, verifyErr := listXiequAfterAdd(ctx, client)
	confirmed := verifyErr == nil && xiequPendingEntryPresent(state.PendingAdd, entriesAfterAdd)
	if verifyErr == nil {
		result.WhitelistCount = len(entriesAfterAdd)
		state.WhitelistCount = len(entriesAfterAdd)
		pruneXiequManagedRecords(&state, entriesAfterAdd)
	}
	if confirmed {
		confirmXiequManagedRecord(&state, *state.PendingAdd)
	}
	if addErr == nil && confirmed {
		completed := state
		completed.PendingAdd = nil
		now := time.Now().UTC()
		completed.LastStatus, completed.LastError, completed.LastSuccessAt = "added", "", &now
		if err := m.completeXiequOperation(ctx, config, &completed, operation); err != nil {
			result.Warning = "远端条目已核验，但操作完成状态保存失败；屏障继续保留"
			result.Reason, result.Message = "operation_unknown", result.Warning
			result.ManagedIPs = append([]string{}, state.ManagedIPs...)
			return result, nil
		}
		return m.finishXiequRefreshSuccess(ctx, completed, result, ip, true, "添加已取得完整成功响应并核验，操作完成状态已保存")
	}
	// 负列表和连接结束均不是旧请求永不落地的证明，不改 marker、不重发。
	cause := verifyErr
	if cause == nil {
		cause = addErr
	}
	return m.finishXiequRefreshError(ctx, state, result, "add_unconfirmed", "添加操作未确认完成；已保留原标记和账号屏障，后续只读核验，不自动重试", cause)
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
	if state.PendingAdd != nil {
		return db.ErrXiequOperationPending
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	marker := " [artex:" + hex.EncodeToString(token) + "]"
	for len(memo)+len(marker) > 80 {
		runes := []rune(memo)
		memo = string(runes[:len(runes)-1])
	}
	state.PendingAdd = &db.XiequPendingAdd{IP: ip, Memo: strings.TrimSpace(memo) + marker, Prepared: true}
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

func xiequOperationToken() (string, error) {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	return hex.EncodeToString(token), nil
}

func (m *Manager) completeXiequOperation(ctx context.Context, config db.XiequConfig, state *db.XiequWhitelistState, op db.XiequOperation) error {
	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := m.pg.CompleteXiequOperation(completeCtx, config, *state, op); err != nil {
		return err
	}
	state.Revision++
	return nil
}

func confirmXiequManagedRecord(state *db.XiequWhitelistState, pending db.XiequPendingAdd) {
	for _, record := range state.ManagedRecords {
		if record.IP == pending.IP && record.Memo == pending.Memo && record.Scope == state.Scope {
			return
		}
	}
	state.ManagedRecords = append(state.ManagedRecords, db.XiequManagedRecord{IP: pending.IP, Memo: pending.Memo, Scope: state.Scope})
	state.ManagedIPs = appendXiequManagedIP(state.ManagedIPs, pending.IP)
}

func pruneXiequManagedRecords(state *db.XiequWhitelistState, entries []xiequ.Entry) {
	records := make([]db.XiequManagedRecord, 0, len(state.ManagedRecords))
	ips := []string{}
	for _, record := range state.ManagedRecords {
		if record.Scope == state.Scope && xiequPendingEntryPresent(&db.XiequPendingAdd{IP: record.IP, Memo: record.Memo}, entries) {
			records = append(records, record)
			ips = appendXiequManagedIP(ips, record.IP)
		}
	}
	state.ManagedRecords, state.ManagedIPs = records, ips
}

func entryPresent(entries []xiequ.Entry, ip string) bool {
	for _, entry := range entries {
		if entry.IP == ip {
			return true
		}
	}
	return false
}

func appendXiequManagedIP(managed []string, ip string) []string {
	for _, existing := range managed {
		if existing == ip {
			return managed
		}
	}
	return append(managed, ip)
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
