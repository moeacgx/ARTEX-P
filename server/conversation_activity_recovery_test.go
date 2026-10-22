package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/google/uuid"
)

func TestConversationActivityRecoveryRoute(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("未配置隔离PostgreSQL")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	s := &Server{m: &Manager{pg: pg}}
	mux := http.NewServeMux()
	s.registerProxyGroupRoutes(mux)
	c, err := pg.CreateConversation("auto", "正式孤立引用恢复", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM proxy_subject_activity WHERE policy_id=(SELECT id FROM proxy_subject_policies WHERE conversation_id=$1)`, c.ID)
		_ = pg.DeleteConversation(c.ID)
	})
	p, err := pg.GetProxyPolicy(db.ProxySubject{Kind: "conversation", ID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pg.Exec(`INSERT INTO proxy_subject_activity(token,policy_id,revision,backend_pid) VALUES($1,$2,$3,2147483647)`, uuid.NewString(), p.ID, p.State.Revision); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/conversations/%d/proxy/policy-recovery", c.ID)
	for _, body := range []string{
		`{"action":"clear_stale_activity","expected_revision":0}`,
		`{"action":"clear_stale_activity","expected_revision":0,"confirm_operations_exited":false}`,
		`{"action":"clear_stale_activity","expected_revision":0,"confirm_operations_exited":true,"proxy_mode":"direct"}`,
		`{"action":"retry","expected_revision":0,"confirm_operations_exited":true}`,
	} {
		code, _, response := proxyAPI(t, mux, "POST", path, body)
		if code != 400 {
			t.Fatalf("缺少退出确认或混入策略变更未拒绝: %d %s", code, response)
		}
	}
	body := `{"action":"clear_stale_activity","expected_revision":1,"confirm_operations_exited":true}`
	code, _, response := proxyAPI(t, mux, "POST", path, body)
	if code != 409 {
		t.Fatalf("CAS未复核: %d %s", code, response)
	}
	body = `{"action":"clear_stale_activity","expected_revision":0,"confirm_operations_exited":true}`
	for range 2 {
		code, out, response := proxyAPI(t, mux, "POST", path, body)
		if code != 200 || out["proxy_revision"] != float64(0) || out["subject_id"] != i64s(c.ID) {
			t.Fatalf("正式恢复未幂等成功: %d %s", code, response)
		}
	}
	var refs int
	if err = pg.QueryRow(`SELECT count(*) FROM proxy_subject_activity WHERE policy_id=$1`, p.ID).Scan(&refs); err != nil || refs != 0 {
		t.Fatalf("孤立引用未清理: %d %v", refs, err)
	}
	_, token, err := pg.AcquireConversationOperation(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.ReleaseConversationOperation(c.ID, token)
	code, _, response = proxyAPI(t, mux, "POST", path, body)
	if code != 409 {
		t.Fatalf("活对话被手工接管: %d %s", code, response)
	}
	code, _, response = proxyAPI(t, mux, "POST", "/api/tasks/1/proxy/policy-recovery", body)
	if code != 400 {
		t.Fatalf("任务入口接受了对话清理: %d %s", code, response)
	}
}
