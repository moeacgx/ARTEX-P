package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/egress"
)

func TestWarpHealthControllerProbeNowRunsFreshProbe(t *testing.T) {
	c, d, f, id := healthControllerFixture(t)
	d.SetMaxOpenConns(1)
	if err := c.RunOnce(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	before, err := d.WarpHealth(t.Context(), id)
	if err != nil || before.NextProbeAt == nil || !before.NextProbeAt.After(time.Now()) || before.LastCheckedAt == nil {
		t.Fatalf("未建立有效周期门槛: %+v %v", before, err)
	}
	if err := c.RunOnce(t.Context(), id); err != nil || f.probes != 1 {
		t.Fatalf("后台探测没有保留周期门槛: probes=%d err=%v", f.probes, err)
	}
	policy, err := d.GlobalWarpHealthPolicy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.SetGlobalWarpHealthPolicy(t.Context(), policy.Revision, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		current, err := d.GlobalWarpHealthPolicy(context.Background())
		if err == nil {
			_, err = d.SetGlobalWarpHealthPolicy(context.Background(), current.Revision, true)
		}
		if err != nil {
			t.Error(err)
		}
	})
	// 关闭自动恢复仍可立即探测；出口变化必须来自本次真实 Probe 边界。
	f.resultIP = "1.1.1.1"
	if err = c.ProbeNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	after, err := d.WarpHealth(t.Context(), id)
	if err != nil || f.probes != 2 || f.repairs != 0 || after.EgressIP != "1.1.1.1" || after.LastCheckedAt == nil || !after.LastCheckedAt.After(*before.LastCheckedAt) {
		t.Fatalf("本次立即探测未真实执行或误触发恢复: %+v probes=%d repairs=%d err=%v", after, f.probes, f.repairs, err)
	}
}

func TestWarpHealthControllerProbeNowPreservesUnknownClaim(t *testing.T) {
	c, d, f, id := healthControllerFixture(t)
	claim, err := d.BeginWarpHealthProbe(t.Context(), id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = d.MarkWarpHealthUnknown(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if err = c.ProbeNow(t.Context(), id); !errors.Is(err, db.ErrWarpHealthBlocked) {
		t.Fatalf("立即检测未拒绝未知执行: %v", err)
	}
	state, err := d.WarpHealth(t.Context(), id)
	if err != nil || state.Claim.Token != claim.Token || state.ClaimState != "unknown" || f.stops != 0 || f.probes != 0 || f.repairs != 0 {
		t.Fatalf("立即检测接管了遗留屏障: %+v stop=%d probes=%d repairs=%d err=%v", state, f.stops, f.probes, f.repairs, err)
	}
}

func TestWarpHealthControllerProxyTestRejectsStaleReady(t *testing.T) {
	for _, boundary := range []string{"disabled", "execution-busy", "runtime-stopped"} {
		t.Run(boundary, func(t *testing.T) {
			c, d, f, id := healthControllerFixture(t)
			if err := c.RunOnce(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			// 实际管理运行器已停止，数据库仍保留上次 ready；检测不能据此返回成功。
			dir := t.TempDir()
			profileDir := filepath.Join(dir, warpProfileID(id))
			if err := os.MkdirAll(profileDir, 0700); err != nil {
				t.Fatal(err)
			}
			profile := egress.WarpProfile{ID: warpProfileID(id), Phase: "stopped", Desired: false}
			data, err := json.Marshal(profile)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(profileDir, "meta.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			warp, err := egress.NewWarpManager(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = warp.Close() })
			m := &Manager{pg: d}
			proxyRuntimes.Store(m, &proxyRuntimeState{warp: warp, health: c})
			t.Cleanup(func() { proxyRuntimes.Delete(m) })
			wantStatus, wantProbes := http.StatusConflict, 1
			switch boundary {
			case "disabled":
				if _, err := d.Exec(`UPDATE outbound_proxies SET enabled=false WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			case "execution-busy":
				execution, err := d.TryWarpHealthExecution(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				defer execution.Close()
			case "runtime-stopped":
				wantStatus, wantProbes = http.StatusBadGateway, 2
			}
			r := httptest.NewRequest(http.MethodPost, "/api/proxies/"+i64s(id)+"/test", nil)
			r.SetPathValue("id", i64s(id))
			w := httptest.NewRecorder()
			(&Server{m: m}).testProxy(w, r)
			if w.Code != wantStatus || f.probes != wantProbes {
				t.Fatalf("旧 ready 被当作本次检测成功：status=%d want=%d probes=%d want=%d body=%s", w.Code, wantStatus, f.probes, wantProbes, w.Body.String())
			}
		})
	}
}
