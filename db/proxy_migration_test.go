package db

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
)

func TestProxyMigrationConfigCAS(t *testing.T) {
	d := warpHealthDB(t)
	if err := ConfigureProxySecrets(filepath.Join(t.TempDir(), "key")); err != nil {
		t.Fatal(err)
	}
	if err := InitializeProxySecrets(); err != nil {
		t.Fatal(err)
	}
	encoded, err := encryptProxySecret("socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	old, target := healthProxy(t, d), healthProxy(t, d)
	// 使用本地静态代理配置，避免把 WARP 的未验证材料当作健康出口。
	for _, id := range []int64{old, target} {
		if _, err := d.Exec(`UPDATE outbound_proxies SET kind='socks5',url=$2 WHERE id=$1`, id, encoded); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SetGlobalProxyID(&old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.SetGlobalProxyID(nil) })
	g, err := d.CreateProxyGroup("migration", true)
	if err != nil {
		t.Fatal(err)
	}
	g, err = d.SetProxyGroupMembers(g.ID, g.Revision, []int64{old, target})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := d.ProxyMigrationPreview(t.Context(), old)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Configs) != 2 {
		t.Fatalf("missing references: %+v", preview)
	}
	t.Run("subject target changes at admission and commit", func(t *testing.T) {
		row, err := d.CreateTaskWithOptions("migration race", "local fixture", TaskCreateOptions{ProxyMode: TaskProxyModeFixed, ProxyID: &old})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, row.ID); err != nil {
			t.Fatal(err)
		}
		x, err := d.AcquireProxyPolicyExecutor(t.Context(), ProxySubject{Kind: "task", ID: row.ID})
		if err != nil {
			t.Fatal(err)
		}
		defer x.Close()
		if _, err = d.Exec(`UPDATE outbound_proxies SET status='unknown' WHERE id=$1`, target); err != nil {
			t.Fatal(err)
		}
		if _, _, err = x.BeginMigration(row.ProxyRevision, old, target); !errors.Is(err, ErrProxyUnavailable) {
			t.Fatalf("unknown target admitted: %v", err)
		}
		if _, err = d.Exec(`UPDATE outbound_proxies SET status='healthy' WHERE id=$1`, target); err != nil {
			t.Fatal(err)
		}
		_, token, err := x.BeginMigration(row.ProxyRevision, old, target)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`UPDATE outbound_proxies SET status='unknown' WHERE id=$1`, target); err != nil {
			t.Fatal(err)
		}
		if _, err = x.Complete(row.ProxyRevision, token); !errors.Is(err, ErrProxyUnavailable) {
			t.Fatalf("unknown target committed: %v", err)
		}
		if _, err = d.Exec(`UPDATE outbound_proxies SET status='healthy',port=port+1 WHERE id=$1`, target); err != nil {
			t.Fatal(err)
		}
		if _, err = x.Complete(row.ProxyRevision, token); !errors.Is(err, ErrProxyRevisionConflict) {
			t.Fatalf("changed material committed: %v", err)
		}
		current, err := d.GetTask(row.ID)
		if err != nil || current.ProxyID == nil || *current.ProxyID != old || current.ProxyRevision != row.ProxyRevision {
			t.Fatalf("source changed: %+v %v", current, err)
		}
		// 测试内恢复原材料后才允许同一意图完成，不清除屏障。
		if _, err = d.Exec(`UPDATE outbound_proxies SET port=port-1 WHERE id=$1`, target); err != nil {
			t.Fatal(err)
		}
		if _, err = x.Complete(row.ProxyRevision, token); err != nil {
			t.Fatal(err)
		}
	})
	if _, err = d.Exec(`UPDATE outbound_proxies SET status='failed' WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	if err = d.MigrateProxyConfigs(t.Context(), old, target, preview.Configs); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("unhealthy target: %v", err)
	}
	if _, err = d.Exec(`UPDATE outbound_proxies SET status='healthy' WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	if err = d.MigrateProxyConfigs(t.Context(), old, old, preview.Configs); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("same target: %v", err)
	}
	if err = d.SetGlobalProxyID(nil); err != nil {
		t.Fatal(err)
	}
	if err = d.MigrateProxyConfigs(t.Context(), old, target, preview.Configs); !errors.Is(err, ErrProxyRevisionConflict) {
		t.Fatalf("stale snapshot: %v", err)
	}
	// 后续组变更失败必须回滚同事务中更早的全局默认变更。
	if err = d.SetGlobalProxyID(&old); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE proxy_groups SET revision=9007199254740991 WHERE id=$1`, g.ID); err != nil {
		t.Fatal(err)
	}
	preview, err = d.ProxyMigrationPreview(t.Context(), old)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.MigrateProxyConfigs(t.Context(), old, target, preview.Configs); !errors.Is(err, ErrProxyGroupRevisionConflict) {
		t.Fatalf("group conflict: %v", err)
	}
	var global string
	if err = d.QueryRow(`SELECT value FROM settings WHERE key='global_proxy_id'`).Scan(&global); err != nil || global != strconv.FormatInt(old, 10) {
		t.Fatalf("partial config committed: %s %v", global, err)
	}
	if _, err = d.Exec(`UPDATE proxy_groups SET revision=revision-1 WHERE id=$1`, g.ID); err != nil {
		t.Fatal(err)
	}
	preview, err = d.ProxyMigrationPreview(t.Context(), old)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.MigrateProxyConfigs(t.Context(), old, target, preview.Configs); err != nil {
		t.Fatal(err)
	}
	g, err = d.GetProxyGroup(g.ID)
	if err != nil || len(g.Members) != 1 || g.Members[0].ProxyID != target {
		t.Fatalf("dedup failed: %+v %v", g, err)
	}
}
