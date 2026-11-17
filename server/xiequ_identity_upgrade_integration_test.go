package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/integrations/xiequ"
)

func TestXiequRound3EquivalentEscapedURLCannotBypassUnknownBarrier(t *testing.T) {
	m := xiequIsolatedManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var adds atomic.Int32
	var pathsMu sync.Mutex
	var paths []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		if r.URL.Query().Get("act") == "getjson" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if adds.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = w.Write([]byte("not confirmed"))
	}))
	defer func() { once.Do(func() { close(release) }); upstream.Close() }()
	c := xiequConfigureFixture(t, m, upstream)
	base := upstream.URL + "/api"
	var err error
	c, err = m.pg.SetXiequConfig(t.Context(), db.XiequConfigPatch{BaseURL: &base})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := m.refreshXiequWhitelist(t.Context(), true); done <- e }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("首Add未进入")
	}
	alias := upstream.URL + "/%61pi"
	changed, err := m.pg.SetXiequConfig(t.Context(), db.XiequConfigPatch{BaseURL: &alias})
	if err != nil {
		t.Fatal(err)
	}
	oldBarrier, err := m.pg.XiequOperation(t.Context(), c)
	if err != nil || oldBarrier == nil {
		t.Fatalf("原屏障未保留: %+v %v", oldBarrier, err)
	}
	newBarrier, err := m.pg.XiequOperation(t.Context(), changed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("旧刷新未退出")
	}
	pathsMu.Lock()
	t.Logf("两个URL的解码Path=%v", paths)
	pathsMu.Unlock()
	t.Logf("alias看见的屏障=%+v second=%+v err=%v adds=%d", newBarrier, second, err, adds.Load())
	if err != nil || adds.Load() != 1 || second.Reason != "operation_unknown" {
		t.Fatal("同一HTTPS主机/端口/解码路径仅编码拼写变化就绕过账号屏障")
	}
	for _, patch := range []db.XiequConfigPatch{{BaseURL: xiequStringPtr(upstream.URL + "/a%70i")}, {UKEY: xiequStringPtr("rotated-key")}, {UID: xiequStringPtr("account-b")}, {UID: &c.UID, BaseURL: &base}} {
		current, err := m.pg.SetXiequConfig(t.Context(), patch)
		if err != nil {
			t.Fatal(err)
		}
		if current.UID != c.UID {
			continue
		}
		restarted, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
		original, e := m.pg.XiequOperation(t.Context(), current)
		if err != nil || e != nil || restarted.OK || restarted.Reason != "operation_unknown" || adds.Load() != 1 || original == nil || original.Token != oldBarrier.Token {
			t.Fatalf("编码/重启/换密钥/ABA 绕过原 token: %+v %v op=%+v %v", restarted, err, original, e)
		}
	}
}

func TestXiequRound3DeleteAfterFreshListCanDeleteManualReplacement(t *testing.T) {
	m := xiequIsolatedManager(t)
	var mu sync.Mutex
	entries := []xiequ.Entry{{IP: "203.0.113.1", Memo: "owned-marker"}, {IP: "203.0.113.2", Memo: "manual"}, {IP: "203.0.113.3", Memo: "manual"}, {IP: "203.0.113.4", Memo: "manual"}, {IP: "203.0.113.5", Memo: "manual"}}
	lists := 0
	deletedManual := false
	changes := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Query().Get("act") {
		case "getjson":
			lists++
			_ = json.NewEncoder(w).Encode(entries)
			if lists == 2 {
				entries[0].Memo = "operator-created-after-fresh-list"
			}
		case "del":
			changes++
			ip := r.URL.Query().Get("ip")
			for i, e := range entries {
				if e.IP == ip {
					deletedManual = e.Memo == "operator-created-after-fresh-list"
					entries = append(entries[:i], entries[i+1:]...)
					break
				}
			}
			_, _ = w.Write([]byte("ok"))
		case "add":
			changes++
			entries = append(entries, xiequ.Entry{IP: r.URL.Query().Get("ip"), Memo: r.URL.Query().Get("meno")})
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstream.Close()
	c := xiequConfigureFixture(t, m, upstream)
	s := db.XiequWhitelistState{Scope: c.WhitelistScope(), ManagedIPs: []string{"203.0.113.1"}, ManagedRecords: []db.XiequManagedRecord{{IP: "203.0.113.1", Memo: "owned-marker", Scope: c.WhitelistScope()}}}
	if err := m.pg.SetXiequWhitelistState(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	r, err := m.refreshXiequWhitelist(t.Context(), true)
	mu.Lock()
	defer mu.Unlock()
	t.Logf("fresh列表返回旧marker后替换，deletedManual=%v result=%+v err=%v", deletedManual, r, err)
	if deletedManual {
		t.Fatal("fresh List只缩短窗口，自动按IP删除仍误删重建的手工条目")
	}
	if err != nil || r.OK || r.Reason != "quota_full" || len(r.Removed) != 0 || changes != 0 {
		t.Fatalf("禁止自动删除后不得伪成功: %+v %v", r, err)
	}
}

// 旧用例落地绑定第二Add，无法检验零重发；本例由独立通道触发迟到事件。
func TestXiequRound3DelayedCommitPreservesMarkerWithoutSecondAdd(t *testing.T) {
	m := xiequIsolatedManager(t)
	var mu sync.Mutex
	entries := []xiequ.Entry{}
	var memo string
	failLists := 0
	adds := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Query().Get("act") == "getjson" {
			if failLists > 0 {
				failLists--
				http.Error(w, "synthetic", 503)
				return
			}
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		adds++
		memo = r.URL.Query().Get("meno")
		failLists = 2
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer upstream.Close()
	upstream.Client().Transport.(*http.Transport).DisableKeepAlives = true
	c := xiequConfigureFixture(t, m, upstream)
	first, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || first.OK {
		t.Fatalf("首次=%+v %v", first, err)
	}
	op, err := m.pg.XiequOperation(t.Context(), c)
	if err != nil || op == nil {
		t.Fatalf("无屏障: %+v %v", op, err)
	}
	second, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil || second.OK || second.Reason != "operation_unknown" {
		t.Fatalf("暂空列表=%+v %v", second, err)
	}
	mu.Lock()
	original := memo
	entries = append(entries, xiequ.Entry{IP: "203.0.113.99", Memo: original})
	mu.Unlock()
	third, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil || third.OK || third.Reason != "operation_unknown" || len(third.ManagedIPs) != 1 {
		t.Fatalf("迟到后=%+v %v", third, err)
	}
	persisted, err := m.pg.XiequOperation(t.Context(), c)
	mu.Lock()
	defer mu.Unlock()
	if err != nil || persisted == nil || persisted.Token != op.Token || persisted.Memo != original || adds != 1 {
		t.Fatalf("归属/屏障变化: %+v err=%v adds=%d", persisted, err, adds)
	}
	t.Logf("first=%s second=%s third=%s adds=%d 原token保留，marker阳性仍未知", first.Reason, second.Reason, third.Reason, adds)
}

func TestXiequRound3NullListCannotReleaseDeleteBarrier(t *testing.T) {
	m := xiequIsolatedManager(t)
	var deletes atomic.Int32
	var mutations atomic.Int32
	var emptyList atomic.Bool
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("act") == "del" {
			mutations.Add(1)
			deletes.Add(1)
			_, _ = w.Write([]byte("ok"))
			return
		}
		if deletes.Load() > 0 {
			_, _ = w.Write([]byte(`{"data":null}`))
			return
		}
		if emptyList.Load() {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"IP":"203.0.113.1","MEMO":"owned"},{"IP":"203.0.113.2"},{"IP":"203.0.113.3"},{"IP":"203.0.113.4"},{"IP":"203.0.113.5"}]`))
	}))
	defer upstream.Close()
	c := xiequConfigureFixture(t, m, upstream)
	s := db.XiequWhitelistState{Scope: c.WhitelistScope(), ManagedRecords: []db.XiequManagedRecord{{IP: "203.0.113.1", Memo: "owned", Scope: c.WhitelistScope()}}}
	if err := m.pg.SetXiequWhitelistState(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	// 模拟旧版本已发送但结果未知的 Delete，禁删升级不能清掉该 journal。
	seedXiequUnknownLegacyDelete(t, m, c)
	deletes.Store(1)
	r, err := m.refreshXiequWhitelist(t.Context(), true)
	op, e := m.pg.XiequOperation(context.Background(), c)
	if err != nil || r.OK || op == nil || e != nil || op.Kind != "del" || deletes.Load() != 1 || mutations.Load() != 0 {
		t.Fatalf("null列表释放屏障: %+v err=%v op=%+v e=%v del=%d", r, err, op, e, deletes.Load())
	}
	// 普通空数组、状态只读和重启同样不能解除已有未知 Delete。
	deletes.Store(0)
	emptyList.Store(true)
	r, err = (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	state, stateErr := m.pg.XiequWhitelistState()
	op, e = m.pg.XiequOperation(t.Context(), c)
	if err != nil || stateErr != nil || e != nil || r.OK || r.Reason != "operation_unknown" || state.LastStatus != "operation_unknown" || op == nil || op.Token != "legacy-delete-token" || mutations.Load() != 0 {
		t.Fatalf("读状态/重启解除未知 Delete: %+v %v state=%+v %v op=%+v %v", r, err, state, stateErr, op, e)
	}
}

func seedXiequUnknownLegacyDelete(t *testing.T, m *Manager, c db.XiequConfig) {
	t.Helper()
	account, _, _ := strings.Cut(c.WhitelistScope(), ":")
	op := db.XiequOperation{Token: "legacy-delete-token", Scope: c.WhitelistScope(), Kind: "del", IP: "203.0.113.1", Memo: "owned"}
	payload, _ := json.Marshal(op)
	if _, err := m.pg.Exec("INSERT INTO settings(key,value) VALUES($1,$2)", "integration.xiequ.operation.v1:"+account, string(payload)); err != nil {
		t.Fatal(err)
	}
}
