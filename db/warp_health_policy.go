package db

import (
	"context"
	"database/sql"
)

type GlobalWarpHealthPolicy struct {
	AutoRecoverEnabled    bool
	Revision              int64
	MigratedDisabledNodes int
}

func (d *DB) GlobalWarpHealthPolicy(ctx context.Context) (GlobalWarpHealthPolicy, error) {
	var p GlobalWarpHealthPolicy
	err := d.QueryRowContext(ctx, `SELECT auto_recover_enabled,revision,migrated_disabled_nodes FROM warp_health_policy WHERE singleton`).Scan(&p.AutoRecoverEnabled, &p.Revision, &p.MigratedDisabledNodes)
	return p, err
}

// 与恢复领取共用生命周期锁，只改变策略，不清执行令牌、运行意图和真实资源占用。
func (d *DB) SetGlobalWarpHealthPolicy(ctx context.Context, expectedRevision int64, enabled bool) (GlobalWarpHealthPolicy, error) {
	var out GlobalWarpHealthPolicy
	err := d.withWarpHealth(ctx, func(tx *sql.Tx) error {
		if expectedRevision <= 0 || expectedRevision >= 9007199254740991 {
			return ErrWarpHealthStale
		}
		err := tx.QueryRowContext(ctx, `UPDATE warp_health_policy SET auto_recover_enabled=$2,revision=revision+1,updated_at=now() WHERE singleton AND revision=$1 RETURNING auto_recover_enabled,revision,migrated_disabled_nodes`, expectedRevision, enabled).Scan(&out.AutoRecoverEnabled, &out.Revision, &out.MigratedDisabledNodes)
		if err == sql.ErrNoRows {
			return ErrWarpHealthStale
		}
		return err
	})
	return out, err
}

// CheckGlobalWarpRecoveryEnabledTx 仅允许已显式启用的全局策略，读取错误不降级为允许。
func CheckGlobalWarpRecoveryEnabledTx(ctx context.Context, tx *sql.Tx) error {
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT auto_recover_enabled FROM warp_health_policy WHERE singleton`).Scan(&enabled); err != nil {
		return err
	}
	if !enabled {
		return ErrWarpHealthBlocked
	}
	return nil
}

// CheckWarpHealthAutoRecoveryEligibleTx 在同一生命周期事务中检查自动恢复准入。
func CheckWarpHealthAutoRecoveryEligibleTx(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := healthEligibleTx(ctx, tx, id); err != nil {
		return err
	}
	return CheckGlobalWarpRecoveryEnabledTx(ctx, tx)
}
