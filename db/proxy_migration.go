package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strconv"
)

// ProxyMigrationReference 仅包含引用标识，不返回代理快照或凭据。
type ProxyMigrationReference struct {
	Kind     string `json:"kind"`
	ID       int64  `json:"id,string"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
	Mode     string `json:"mode,omitempty"`
}

type ProxyMigrationPreview struct {
	Subjects []ProxyMigrationReference `json:"subjects"`
	Configs  []ProxyMigrationReference `json:"configs"`
	Blockers []string                  `json:"blockers"`
	Token    string                    `json:"token"`
}

func proxyMigrationConfigsTx(ctx context.Context, tx *sql.Tx, id int64) ([]ProxyMigrationReference, error) {
	rows, err := tx.QueryContext(ctx, `SELECT 'global',0::bigint,'全局默认代理',0::bigint FROM settings WHERE key='global_proxy_id' AND value=($1::bigint)::text
UNION ALL SELECT 'profile',id,name,0 FROM llm_profiles WHERE proxy_id=$1::bigint
UNION ALL SELECT 'group',g.id,g.name,g.revision FROM proxy_groups g JOIN proxy_group_members m ON m.group_id=g.id WHERE m.proxy_id=$1::bigint
ORDER BY 1,2`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProxyMigrationReference{}
	for rows.Next() {
		var v ProxyMigrationReference
		if err := rows.Scan(&v.Kind, &v.ID, &v.Name, &v.Revision); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (d *DB) ProxyMigrationPreview(ctx context.Context, id int64) (ProxyMigrationPreview, error) {
	out := ProxyMigrationPreview{Subjects: []ProxyMigrationReference{}, Configs: []ProxyMigrationReference{}, Blockers: []string{}}
	err := d.withWarpHealth(ctx, func(tx *sql.Tx) error {
		p, err := getProxyTx(tx, id)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT 'task',id,description,proxy_revision,proxy_mode FROM tasks WHERE proxy_id=$1 AND deleted_at IS NULL
UNION ALL SELECT 'conversation',id,title,proxy_revision,proxy_mode FROM conversations WHERE proxy_id=$1 ORDER BY 1,2`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var v ProxyMigrationReference
			if err = rows.Scan(&v.Kind, &v.ID, &v.Name, &v.Revision, &v.Mode); err != nil {
				rows.Close()
				return err
			}
			out.Subjects = append(out.Subjects, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		out.Configs, err = proxyMigrationConfigsTx(ctx, tx, id)
		if err != nil {
			return err
		}
		var busy bool
		err = tx.QueryRowContext(ctx, `SELECT
EXISTS(SELECT 1 FROM task_proxy_activity WHERE proxy_id=$1 AND active_count>0)
OR EXISTS(SELECT 1 FROM task_operation_activity a JOIN tasks t ON t.id=a.task_id WHERE t.proxy_id=$1 AND (a.active_count>0 OR a.operation_owners<>'{}' OR a.switch_token<>''))
OR EXISTS(SELECT 1 FROM proxy_subject_activity WHERE proxy_id=$1)
OR EXISTS(SELECT 1 FROM proxy_subject_policies p LEFT JOIN tasks t ON t.id=p.task_id LEFT JOIN conversations c ON c.id=p.conversation_id WHERE p.switch_token<>'' AND (p.reserved_proxy_id=$1 OR t.proxy_id=$1 OR c.proxy_id=$1))
OR EXISTS(SELECT 1 FROM proxy_switch_requests WHERE state IN ('pending','draining','applying','recovery_required') AND (from_proxy_id=$1 OR to_proxy_id=$1))`, id).Scan(&busy)
		if err != nil {
			return err
		}
		if busy {
			out.Blockers = append(out.Blockers, "仍有真实操作或未完成的代理切换，请等待退出或完成原切换")
		}
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE proxy_id=$1 AND deleted_at IS NULL AND (queued OR (NOT paused AND status NOT IN ('paused','done','failed','timeout'))))`, id).Scan(&busy); err != nil {
			return err
		}
		if busy {
			out.Blockers = append(out.Blockers, "请先暂停引用任务并等待真实操作退出，再刷新引用")
		}
		pending, err := warpCreatePendingTx(tx, id)
		if err != nil {
			return err
		}
		if pending {
			out.Blockers = append(out.Blockers, "代理创建结果尚未确认，请核对原创建请求")
		}
		if p.Kind == ProxyKindWarp {
			state, err := healthTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if state.Claim.Token != "" {
				out.Blockers = append(out.Blockers, "WARP 健康操作尚未结束或退出未确认")
			}
		}
		return nil
	})
	if err == nil {
		raw, _ := json.Marshal(out)
		sum := sha256.Sum256(append([]byte(strconv.FormatInt(id, 10)+":"), raw...))
		out.Token = hex.EncodeToString(sum[:])
	}
	return out, err
}

func migrationTargetTx(ctx context.Context, tx *sql.Tx, from, target int64) error {
	if from == target {
		return ErrProxyUnavailable
	}
	if _, err := getProxyTx(tx, from); err != nil {
		return err
	}
	p, err := getProxyTx(tx, target)
	if err != nil {
		return err
	}
	// 渠道、代理组不支持独立 Resin 租约，统一替代节点只允许静态已验证出口。
	if p.Kind == ProxyKindResin || p.Status != ProxyStatusHealthy {
		return ErrProxyUnavailable
	}
	if err = usableProxy(p); err != nil {
		return err
	}
	return CheckWarpHealthAdmissionTx(ctx, tx, target)
}

// 指纹仅取数据库中的加密连接材料和 WARP 材料标识，不包含检测时间或探测计数。
func proxyMigrationFingerprintTx(tx *sql.Tx, id int64) (string, error) {
	var raw []byte
	err := tx.QueryRow(`SELECT jsonb_build_array(p.kind,p.protocol,p.url,p.host,p.port,p.username,p.password,COALESCE(h.material,''))::text FROM outbound_proxies p LEFT JOIN warp_proxy_health h ON h.proxy_id=p.id WHERE p.id=$1`, id).Scan(&raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (d *DB) ValidateProxyMigrationTarget(ctx context.Context, from, target int64) error {
	return d.withWarpHealth(ctx, func(tx *sql.Tx) error { return migrationTargetTx(ctx, tx, from, target) })
}

// MigrateProxyConfigs 比较预览中的完整配置引用集，在同一生命周期事务内替换。
// 主体运行时切换由服务层通过原执行器完成，不能在这里更新其快照或清活动。
func (d *DB) MigrateProxyConfigs(ctx context.Context, from, target int64, expected []ProxyMigrationReference) error {
	return d.withWarpHealth(ctx, func(tx *sql.Tx) error {
		if err := migrationTargetTx(ctx, tx, from, target); err != nil {
			return err
		}
		actual, err := proxyMigrationConfigsTx(ctx, tx, from)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, expected) {
			return ErrProxyRevisionConflict
		}
		for _, ref := range actual {
			switch ref.Kind {
			case "global":
				_, err = tx.ExecContext(ctx, `UPDATE settings SET value=$1,updated_at=now() WHERE key='global_proxy_id'`, strconv.FormatInt(target, 10))
			case "profile":
				_, err = tx.ExecContext(ctx, `UPDATE llm_profiles SET proxy_id=$1 WHERE id=$2 AND proxy_id=$3`, target, ref.ID, from)
			case "group":
				if err = groupMemberRemovalTx(tx, ref.ID, from); err != nil {
					return err
				}
				if ref.Revision >= 9007199254740991 {
					return ErrProxyGroupRevisionConflict
				}
				// 若目标已存在只移除旧成员，保留顺序及其余授权关系。
				_, err = tx.ExecContext(ctx, `DELETE FROM proxy_group_members WHERE group_id=$1 AND proxy_id=$2 AND EXISTS(SELECT 1 FROM proxy_group_members WHERE group_id=$1 AND proxy_id=$3)`, ref.ID, from, target)
				if err == nil {
					_, err = tx.ExecContext(ctx, `UPDATE proxy_group_members SET proxy_id=$3 WHERE group_id=$1 AND proxy_id=$2`, ref.ID, from, target)
				}
				if err == nil {
					_, err = tx.ExecContext(ctx, `UPDATE proxy_groups SET revision=revision+1 WHERE id=$1`, ref.ID)
				}
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}
