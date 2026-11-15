package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestProxyPGMigrationDelete(t *testing.T) {
	s, mux := proxyIntegration(t)
	old := createRuntimeProxy(t, s, "http://synthetic:do-not-expose@127.0.0.1:18080")
	target := createRuntimeProxy(t, s, "http://127.0.0.1:18081")
	if err := s.m.pg.SetProxyStatus(target.ID, db.ProxyStatusHealthy, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	task := createRuntimeTask(t, s, db.TaskProxyModeFixed, &old.ID)
	if _, err := s.m.pg.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.SetGlobalProxyID(&old.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.SetGlobalProxyID(nil) })
	path := fmt.Sprintf("/api/proxies/%d", old.ID)
	code, out, raw := proxyAPI(t, mux, "GET", path+"/references", "")
	if code != 200 || strings.Contains(raw, "do-not-expose") || len(out["subjects"].([]any)) != 1 {
		t.Fatalf("preview: %d %s", code, raw)
	}
	body := fmt.Sprintf(`{"target_proxy_id":"%d","preview_token":"%s"}`, target.ID, out["token"])
	// 原快照之后出现新引用时拒绝，不能悄悄改变用户尚未确认的引用。
	g, err := s.m.pg.CreateProxyGroup("migration-http", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM proxy_groups WHERE id=$1`, g.ID) })
	if _, err = s.m.pg.SetProxyGroupMembers(g.ID, g.Revision, []int64{old.ID}); err != nil {
		t.Fatal(err)
	}
	code, _, raw = proxyAPI(t, mux, "POST", path+"/migrate-delete", body)
	if code != 409 {
		t.Fatalf("stale preview: %d %s", code, raw)
	}
	// 已迁移主体后配置事务失败：保留源代理并明确逐项结果，刷新可续作。
	if _, err = s.m.pg.Exec(`UPDATE proxy_groups SET revision=9007199254740991 WHERE id=$1`, g.ID); err != nil {
		t.Fatal(err)
	}
	code, out, raw = proxyAPI(t, mux, "GET", path+"/references", "")
	if code != 200 {
		t.Fatal(raw)
	}
	body = fmt.Sprintf(`{"target_proxy_id":"%d","preview_token":"%s"}`, target.ID, out["token"])
	code, out, raw = proxyAPI(t, mux, "POST", path+"/migrate-delete", body)
	if code != 409 || out["deleted"] != false || len(out["results"].([]any)) != 2 {
		t.Fatalf("partial result: %d %s", code, raw)
	}
	if _, err = s.m.pg.GetProxy(old.ID); err != nil {
		t.Fatalf("partial removed source: %v", err)
	}
	if _, err = s.m.pg.Exec(`UPDATE proxy_groups SET revision=revision-1 WHERE id=$1`, g.ID); err != nil {
		t.Fatal(err)
	}
	code, out, raw = proxyAPI(t, mux, "GET", path+"/references", "")
	if code != 200 {
		t.Fatal(raw)
	}
	body = fmt.Sprintf(`{"target_proxy_id":"%d","preview_token":"%s"}`, target.ID, out["token"])
	code, out, raw = proxyAPI(t, mux, "POST", path+"/migrate-delete", body)
	if code != 200 || out["deleted"] != true {
		t.Fatalf("migration: %d %s", code, raw)
	}
	if _, err = s.m.pg.GetProxy(old.ID); err == nil {
		t.Fatal("source retained after success")
	}
	row, err := s.m.pg.GetTask(mustTaskID(t, task.ID))
	if err != nil || row.ProxyID == nil || *row.ProxyID != target.ID || !row.Paused {
		t.Fatalf("task state: %+v %v", row, err)
	}
}
