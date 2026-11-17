package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

var ErrXiequOperationPending = errors.New("谐趣账号存在尚未确认完成的远端操作，已阻止后续变更")
var ErrXiequLegacyAccountUnknown = errors.New("旧谐趣操作屏障的账号归属无法证明，已保留原记录并阻止变更")

// XiequOperation 的账号键只含身份摘要，不含 UID、UKEY；配置换代不清除。
// 完成记录仍保留精确 token，只有发起调用持有完整响应并核验结果才可转换。
type XiequOperation struct {
	Token     string `json:"token"`
	Scope     string `json:"scope"`
	Kind      string `json:"kind"`
	IP        string `json:"ip"`
	Memo      string `json:"memo"`
	Account   string `json:"account,omitempty"`
	Completed bool   `json:"completed,omitempty"`
}

func xiequOperationKey(c XiequConfig) string {
	return "integration.xiequ.operation.v2:" + c.accountIdentity()
}

func readXiequOperation(raw string) (*XiequOperation, error) {
	var op XiequOperation
	if err := json.Unmarshal([]byte(raw), &op); err != nil || op.Token == "" || op.Scope == "" || (op.Kind != "add" && op.Kind != "del") || op.IP == "" || op.Memo == "" {
		return nil, errors.New("谐趣远端操作屏障无法读取，已阻止变更")
	}
	return &op, nil
}

func (d *DB) XiequOperation(ctx context.Context, c XiequConfig) (*XiequOperation, error) {
	legacy, err := readXiequLegacyOperations(ctx, d, c)
	if err != nil {
		return nil, err
	}
	var raw string
	err = d.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", xiequOperationKey(c)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return legacy, nil
	}
	if err != nil {
		return nil, err
	}
	op, err := readXiequOperation(raw)
	if err != nil {
		return nil, err
	}
	if op.Account != "" && op.Account != c.accountIdentity() {
		return nil, ErrXiequLegacyAccountUnknown
	}
	if !op.Completed {
		return op, err
	}
	return legacy, nil
}

type xiequOperationReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type xiequLegacyOperation struct {
	key string
	op  XiequOperation
}

// 旧摘要无法反推 URL。只允许当前配置的旧/规范摘要证明绑定，其余未知记录保守阻断。
// 已证明绑定的旧记录仅补充账号摘要，原 token、scope、completed 均不改变。
func loadXiequLegacyOperations(ctx context.Context, q xiequOperationReader) ([]xiequLegacyOperation, error) {
	rows, err := q.QueryContext(ctx, "SELECT key,value FROM settings WHERE key LIKE 'integration.xiequ.operation.v1:%'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var legacy []xiequLegacyOperation
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		op, err := readXiequOperation(raw)
		if err != nil {
			return nil, err
		}
		if !op.Completed {
			legacy = append(legacy, xiequLegacyOperation{key: key, op: *op})
		}
	}
	return legacy, rows.Err()
}

func xiequLegacyAccount(record xiequLegacyOperation, c XiequConfig) (string, error) {
	hash := strings.TrimPrefix(record.key, "integration.xiequ.operation.v1:")
	scopeHash, _, ok := strings.Cut(record.op.Scope, ":")
	if !ok || hash != scopeHash {
		return "", ErrXiequLegacyAccountUnknown
	}
	if record.op.Account != "" {
		if len(record.op.Account) != 64 {
			return "", ErrXiequLegacyAccountUnknown
		}
		return record.op.Account, nil
	}
	if hash == c.legacyAccountIdentity() || hash == c.accountIdentity() {
		return c.accountIdentity(), nil
	}
	return "", ErrXiequLegacyAccountUnknown
}

func readXiequLegacyOperations(ctx context.Context, q xiequOperationReader, c XiequConfig) (*XiequOperation, error) {
	records, err := loadXiequLegacyOperations(ctx, q)
	if err != nil {
		return nil, err
	}
	var active *XiequOperation
	for _, record := range records {
		account, err := xiequLegacyAccount(record, c)
		if err != nil {
			return nil, err
		}
		if account == c.accountIdentity() {
			op := record.op
			_, revision, _ := strings.Cut(op.Scope, ":")
			op.Scope = account + ":" + revision
			if active != nil && active.Token != op.Token {
				return nil, ErrXiequOperationPending
			}
			active = &op
		}
	}
	return active, nil
}

func bindXiequLegacyOperations(ctx context.Context, tx *sql.Tx, c XiequConfig) error {
	records, err := loadXiequLegacyOperations(ctx, tx)
	if err != nil {
		return err
	}
	for _, record := range records {
		hash := strings.TrimPrefix(record.key, "integration.xiequ.operation.v1:")
		if hash != c.legacyAccountIdentity() && hash != c.accountIdentity() {
			continue
		}
		account, err := xiequLegacyAccount(record, c)
		if err != nil || account != c.accountIdentity() {
			return ErrXiequLegacyAccountUnknown
		}
		record.op.Account = account
		payload, err := json.Marshal(record.op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE settings SET value=$2,updated_at=now() WHERE key=$1", record.key, string(payload)); err != nil {
			return err
		}
	}
	return nil
}

// ClaimXiequOperation 在网络变更前，原子校验配置、状态 CAS 并占用账号屏障。
// 屏障一旦写入就按可能已发送处理，崩溃、空列表、超时和 marker 阳性均不解除。
func (d *DB) ClaimXiequOperation(ctx context.Context, c XiequConfig, state XiequWhitelistState, op XiequOperation) error {
	return d.transitionXiequOperation(ctx, c, state, op, false)
}

func (d *DB) CompleteXiequOperation(ctx context.Context, c XiequConfig, state XiequWhitelistState, op XiequOperation) error {
	return d.transitionXiequOperation(ctx, c, state, op, true)
}

func (d *DB) transitionXiequOperation(ctx context.Context, c XiequConfig, state XiequWhitelistState, op XiequOperation, complete bool) error {
	if op.Token == "" || op.Scope != c.WhitelistScope() || state.Scope != op.Scope || (op.Kind != "add" && op.Kind != "del") || op.IP == "" || op.Memo == "" {
		return ErrXiequConfigChanged
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", settingXiequ).Scan(&raw); err != nil {
		return err
	}
	current, err := openXiequConfig(raw)
	if err != nil {
		return err
	}
	if current.WhitelistScope() != c.WhitelistScope() {
		return ErrXiequConfigChanged
	}
	legacy, err := readXiequLegacyOperations(ctx, tx, current)
	if err != nil {
		return err
	}
	if legacy != nil {
		return ErrXiequOperationPending
	}
	var previous XiequWhitelistState
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", settingXiequWhitelistState).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal([]byte(raw), &previous); err != nil {
			return errors.New("谐趣白名单状态无法读取")
		}
	}
	previous = previous.ForConfig(current)
	if previous.Revision != state.Revision {
		return ErrXiequStateChanged
	}
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", xiequOperationKey(c)).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var existing *XiequOperation
	if err == nil {
		existing, err = readXiequOperation(raw)
		if err != nil {
			return err
		}
	}
	if complete {
		if existing == nil || existing.Completed || existing.Token != op.Token || existing.Scope != op.Scope || existing.Kind != op.Kind || existing.IP != op.IP || existing.Memo != op.Memo {
			return ErrXiequOperationPending
		}
	} else if existing != nil && !existing.Completed {
		return ErrXiequOperationPending
	}
	op.Completed = complete
	op.Account = c.accountIdentity()
	if !complete && op.Kind == "add" {
		if state.PendingAdd == nil || !state.PendingAdd.Prepared || state.PendingAdd.IP != op.IP || state.PendingAdd.Memo != op.Memo {
			return ErrXiequStateChanged
		}
		pending := *state.PendingAdd
		pending.Prepared = false
		state.PendingAdd = &pending
	}
	state.Revision++
	state = normalizeXiequState(state)
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()", settingXiequWhitelistState, string(payload)); err != nil {
		return err
	}
	payload, err = json.Marshal(op)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()", xiequOperationKey(c), string(payload)); err != nil {
		return err
	}
	return tx.Commit()
}

// 兼容已有 pending：没有 prepared 证明的一律视为可能已发送。
func preserveXiequLegacyPending(ctx context.Context, tx *sql.Tx, c XiequConfig) error {
	if err := bindXiequLegacyOperations(ctx, tx, c); err != nil {
		return err
	}
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", settingXiequWhitelistState).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var state XiequWhitelistState
	if json.Unmarshal([]byte(raw), &state) != nil {
		return ErrXiequStateUnreadable
	}
	if state.Scope != c.WhitelistScope() && state.Scope != c.legacyAccountIdentity()+":"+c.Revision {
		return nil
	}
	originalScope := state.Scope
	state = state.ForConfig(c)
	if originalScope != state.Scope {
		payload, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE settings SET value=$2,updated_at=now() WHERE key=$1", settingXiequWhitelistState, string(payload)); err != nil {
			return err
		}
	}
	if state.PendingAdd == nil || state.PendingAdd.Prepared {
		return nil
	}
	legacy, err := readXiequLegacyOperations(ctx, tx, c)
	if err != nil {
		// 无法证明的旧账号只阻止远端变更，不凭空清除或改写其 journal。
		if errors.Is(err, ErrXiequLegacyAccountUnknown) || errors.Is(err, ErrXiequOperationPending) {
			return nil
		}
		return err
	}
	if legacy != nil {
		return nil
	}
	// 已完成的历史 journal 不能遮蔽后来的遗留 pending；未知 journal 则必须保留。
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", xiequOperationKey(c)).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		existing, err := readXiequOperation(raw)
		if err != nil {
			return err
		}
		if !existing.Completed {
			return nil
		}
	}
	op := XiequOperation{Token: state.PendingAdd.Memo, Scope: state.Scope, Kind: "add", IP: state.PendingAdd.IP, Memo: state.PendingAdd.Memo, Account: c.accountIdentity()}
	payload, err := json.Marshal(op)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()", xiequOperationKey(c), string(payload))
	return err
}

// PreserveXiequPending 在旧状态首次恢复时建立屏障，不能先清除原 marker。
func (d *DB) PreserveXiequPending(ctx context.Context, c XiequConfig) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", settingXiequ).Scan(&raw); err != nil {
		return err
	}
	current, err := openXiequConfig(raw)
	if err != nil {
		return err
	}
	if current.WhitelistScope() != c.WhitelistScope() {
		return ErrXiequConfigChanged
	}
	if err := preserveXiequLegacyPending(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit()
}
