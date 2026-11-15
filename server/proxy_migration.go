package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/db"
)

func (m *Manager) migrateProxySubject(ctx context.Context, subject db.ProxySubject, expected, from, target int64) error {
	x, err := m.pg.AcquireProxyPolicyExecutor(ctx, subject)
	if err != nil {
		return err
	}
	defer x.Close()
	old, token, err := x.BeginMigration(expected, from, target)
	if err != nil {
		return err
	}
	if err = m.ReleaseTaskProxy(ctx, proxySubjectRuntimeTask(old)); err != nil {
		return db.ErrProxySwitchRecovery
	}
	p, err := x.Complete(expected, token)
	if err != nil {
		return errors.Join(db.ErrProxySwitchRecovery, err)
	}
	if subject.Kind == "task" {
		if task, ok := m.Task(i64s(subject.ID)); ok {
			return m.refreshTaskProxyState(task, p.State)
		}
	}
	return nil
}

type proxyMigrationResult struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) proxyMigrationPreview(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "代理编号无效")
		return
	}
	preview, err := s.m.pg.ProxyMigrationPreview(r.Context(), id)
	if err != nil {
		writeErr(w, 409, "无法读取代理引用，请刷新代理列表")
		return
	}
	writeJSON(w, 200, preview)
}

func proxyMigrationError(err error) string {
	switch {
	case errors.Is(err, db.ErrProxyRevisionConflict), errors.Is(err, db.ErrProxyGroupRevisionConflict):
		return "引用已被其他操作修改，请刷新后重试"
	case errors.Is(err, db.ErrProxyInUse), errors.Is(err, db.ErrProxySwitchRecovery):
		return "主体仍在运行、出口未退出或原切换待恢复；请暂停并核对后重试"
	case errors.Is(err, db.ErrWarpHealthBlocked):
		return "WARP 健康操作或活动屏障尚未释放，请稍后刷新"
	case errors.Is(err, db.ErrProxyUnavailable):
		return "目标代理不可用，请重新检测并选择健康代理"
	default:
		return "操作未完成，请刷新引用并核对原代理状态"
	}
}

// 逐主体复用执行器；配置 CAS 及最终删除重新检查引用。任何失败均保留源代理。
func (s *Server) migrateAndDeleteProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req struct {
		Target *proxyIDInput `json:"target_proxy_id"`
		Token  string        `json:"preview_token"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err != nil || id <= 0 || dec.Decode(&req) != nil || req.Target == nil || req.Token == "" {
		writeErr(w, 400, "需要替代代理和最新引用预览")
		return
	}
	target := req.Target.Int64()
	if target == nil || *target <= 0 {
		writeErr(w, 400, "目标代理编号无效")
		return
	}
	preview, err := s.m.pg.ProxyMigrationPreview(r.Context(), id)
	if err != nil {
		writeErr(w, 409, "无法读取代理引用，请刷新")
		return
	}
	if preview.Token != req.Token {
		writeErr(w, 409, "引用已变化，请刷新预览后确认")
		return
	}
	if len(preview.Blockers) > 0 {
		writeJSON(w, 409, map[string]any{"error": "请先处理阻塞项", "preview": preview})
		return
	}
	if err = s.m.pg.ValidateProxyMigrationTarget(r.Context(), id, *target); err != nil {
		writeErr(w, 409, proxyMigrationError(err))
		return
	}
	results := []proxyMigrationResult{}
	fail := func(kind, refID string, err error) {
		results = append(results, proxyMigrationResult{Kind: kind, ID: refID, Error: proxyMigrationError(err)})
		writeJSON(w, 409, map[string]any{"error": "迁移未全部完成，旧代理已保留；已成功的迁移不会回滚，请刷新后核对", "results": results, "deleted": false})
	}
	for _, ref := range preview.Subjects {
		if err = r.Context().Err(); err != nil {
			fail(ref.Kind, i64s(ref.ID), err)
			return
		}
		policy, readErr := s.m.pg.GetProxyPolicy(db.ProxySubject{Kind: ref.Kind, ID: ref.ID})
		if readErr != nil {
			fail(ref.Kind, i64s(ref.ID), readErr)
			return
		}
		if policy.State.ID == nil || *policy.State.ID != id || policy.State.Revision != ref.Revision {
			fail(ref.Kind, i64s(ref.ID), db.ErrProxyRevisionConflict)
			return
		}
		// 用户明确确认固定到所选出口；保留其余故障切换授权配置。
		err = s.m.migrateProxySubject(r.Context(), policy.Subject, ref.Revision, id, *target)
		if err != nil {
			fail(ref.Kind, i64s(ref.ID), err)
			return
		}
		results = append(results, proxyMigrationResult{Kind: ref.Kind, ID: i64s(ref.ID), OK: true})
	}
	if err = s.m.pg.MigrateProxyConfigs(r.Context(), id, *target, preview.Configs); err != nil {
		fail("config", i64s(id), err)
		return
	}
	for _, ref := range preview.Configs {
		results = append(results, proxyMigrationResult{Kind: ref.Kind, ID: i64s(ref.ID), OK: true})
	}
	p, err := s.m.pg.GetProxy(id)
	if err == nil {
		if p.Kind == db.ProxyKindWarp {
			err = s.deleteWarpProxy(r.Context(), id)
		} else {
			err = s.m.pg.DeleteProxy(id)
		}
	}
	if err != nil {
		fail("delete", i64s(id), err)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true, "id": i64s(id), "results": results})
}
