package db

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestConversationActivityRecoverySafety(t *testing.T) {
	d, second, task := plannerRunTestDBs(t)
	proxyID := healthProxy(t, d)
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM outbound_proxies WHERE id=$1`, proxyID) })
	conversation := func() ProxyPolicy {
		t.Helper()
		c, err := d.CreateConversation("auto", "孤立引用恢复测试", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = d.Exec(`DELETE FROM warp_proxy_health_activity w USING proxy_subject_activity a,proxy_subject_policies p WHERE w.activity_token::text=a.token AND a.policy_id=p.id AND p.conversation_id=$1`, c.ID)
			_, _ = d.Exec(`DELETE FROM proxy_subject_activity WHERE policy_id=(SELECT id FROM proxy_subject_policies WHERE conversation_id=$1)`, c.ID)
			_ = d.DeleteConversation(c.ID)
		})
		p, err := d.GetProxyPolicy(ProxySubject{Kind: "conversation", ID: c.ID})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	seed := func(p ProxyPolicy, pid int) string {
		t.Helper()
		token := uuid.NewString()
		if _, err := d.Exec(`INSERT INTO proxy_subject_activity(token,policy_id,revision,proxy_id,backend_pid) VALUES($1,$2,$3,$4,$5)`, token, p.ID, p.State.Revision, proxyID, pid); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`INSERT INTO warp_proxy_health_activity(activity_token,proxy_id) VALUES($1,$2)`, token, proxyID); err != nil {
			t.Fatal(err)
		}
		return token
	}
	clear := func(p ProxyPolicy, revision int64) (ProxyPolicy, error) {
		x, err := second.AcquireProxyPolicyExecutor(t.Context(), p.Subject)
		if err != nil {
			return ProxyPolicy{}, err
		}
		defer x.Close()
		return x.ClearStaleConversationActivities(revision)
	}
	p, other := conversation(), conversation()
	seed(p, 2147483647)
	otherToken := seed(other, 2147483647)
	standalone := uuid.NewString()
	if _, err := d.Exec(`INSERT INTO warp_proxy_health_activity(activity_token,proxy_id) VALUES($1,$2)`, standalone, proxyID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM warp_proxy_health_activity WHERE activity_token=$1`, standalone) })
	if _, err := clear(p, p.State.Revision+1); !errors.Is(err, ErrProxyRevisionConflict) {
		t.Fatalf("陈旧版本未拒绝: %v", err)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM proxy_subject_activity WHERE policy_id=$1`, p.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("失败修改了引用: %d %v", count, err)
	}
	after, err := clear(p, p.State.Revision)
	if err != nil || !reflect.DeepEqual(p, after) {
		t.Fatalf("恢复改变了对话策略: %+v %v", after, err)
	}
	stored, err := d.GetProxyPolicy(p.Subject)
	if err != nil || !reflect.DeepEqual(p, stored) {
		t.Fatalf("恢复改变了持久策略: %+v %v", stored, err)
	}
	if _, err = clear(p, p.State.Revision); err != nil {
		t.Fatalf("重复恢复不幂等: %v", err)
	}
	if err = d.QueryRow(`SELECT count(*) FROM warp_proxy_health_activity WHERE proxy_id=$1`, proxyID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("跨主体/独立引用被清理: %d %v", count, err)
	}
	if err = d.QueryRow(`SELECT count(*) FROM proxy_subject_activity WHERE token=$1`, otherToken).Scan(&count); err != nil || count != 1 {
		t.Fatalf("其他对话引用被清理: %d %v", count, err)
	}

	t.Run("活跃共享锁", func(t *testing.T) {
		live := conversation()
		_, token, e := d.AcquireConversationOperation(live.Subject.ID)
		if e != nil {
			t.Fatal(e)
		}
		defer d.ReleaseConversationOperation(live.Subject.ID, token)
		if _, e = clear(live, live.State.Revision); !errors.Is(e, ErrProxyInUse) {
			t.Fatalf("活跃操作被接管: %v", e)
		}
	})
	t.Run("owner会话活着但丢锁", func(t *testing.T) {
		live := conversation()
		conn, e := d.Conn(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		defer conn.Close()
		var pid int
		if e = conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); e != nil {
			t.Fatal(e)
		}
		seed(live, pid)
		if _, e = clear(live, live.State.Revision); !errors.Is(e, ErrProxyInUse) {
			t.Fatalf("尚存owner被清理: %v", e)
		}
	})
	for _, state := range []string{"pending", "draining", "applying", "recovery_required"} {
		t.Run("切换屏障"+state, func(t *testing.T) {
			blocked := conversation()
			seed(blocked, 2147483647)
			if _, e := d.Exec(`INSERT INTO proxy_switch_requests(id,policy_id,base_revision,authorization_revision,state,reason) VALUES($1,$2,$3,0,$4,'test')`, uuid.NewString(), blocked.ID, blocked.State.Revision, state); e != nil {
				t.Fatal(e)
			}
			if _, e := clear(blocked, blocked.State.Revision); !errors.Is(e, ErrProxySwitchRecovery) {
				t.Fatalf("切换中引用被清理: %v", e)
			}
		})
	}
	t.Run("手工切换令牌", func(t *testing.T) {
		blocked := conversation()
		seed(blocked, 2147483647)
		if _, e := d.Exec(`UPDATE proxy_subject_policies SET switch_token='uncertain' WHERE id=$1`, blocked.ID); e != nil {
			t.Fatal(e)
		}
		if _, e := clear(blocked, blocked.State.Revision); !errors.Is(e, ErrProxySwitchRecovery) {
			t.Fatalf("切换令牌被忽略: %v", e)
		}
	})
	t.Run("不支持任务", func(t *testing.T) {
		taskPolicy, e := d.GetProxyPolicy(ProxySubject{Kind: "task", ID: task.ID})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = clear(taskPolicy, taskPolicy.State.Revision); !errors.Is(e, ErrProxyInvalidMode) {
			t.Fatalf("越过任务恢复入口: %v", e)
		}
	})
	// 只有本对话的原 token 被删除后，WARP 保护仍由其他引用维持。
	if err = d.WithWarpHealthLifecycle(t.Context(), func(tx *sql.Tx) error { return WarpHealthIdleTx(t.Context(), tx, proxyID) }); !errors.Is(err, ErrWarpHealthBlocked) {
		t.Fatalf("WARP保护被绕过: %v", err)
	}
}
