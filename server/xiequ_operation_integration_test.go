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
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/integrations/xiequ"
)

// 合成远端允许首次 Add 在后续空列表之后落地，不依赖重发来触发。
func TestXiequReworkDelayedAddMustRetainOriginalProvenance(t *testing.T) {
	m := xiequIsolatedManager(t)
	entries := []xiequ.Entry{}
	var originalMemo string
	adds, failLists := 0, 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("act") {
		case "getjson":
			if failLists > 0 {
				failLists--
				http.Error(w, "synthetic unavailable", 503)
				return
			}
			_ = json.NewEncoder(w).Encode(entries)
		case "add":
			adds++
			if adds == 1 {
				originalMemo = r.URL.Query().Get("meno")
				failLists = 2
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			// 原请求此时才落地；同 IP 的新 Add 因重复而不改写原备注。
			entries = []xiequ.Entry{{IP: "203.0.113.99", Memo: originalMemo}}
			_, _ = w.Write([]byte("duplicate IP"))
		}
	}))
	defer upstream.Close()
	// 与生产 DirectHTTPClient 一致，关闭连接复用，避免 GET 自动重试掩盖断连分支。
	upstream.Client().Transport.(*http.Transport).DisableKeepAlives = true
	xiequConfigureFixture(t, m, upstream)
	first, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || first.Reason != "add_unconfirmed" {
		t.Fatalf("首次=%+v err=%v", first, err)
	}
	second, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	// 原请求独立延迟落地；未知期间既不能换 marker，也不能重发。
	entries = []xiequ.Entry{{IP: "203.0.113.99", Memo: originalMemo}}
	third, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.pg.XiequWhitelistState()
	if err != nil {
		t.Fatal(err)
	}
	if second.OK || second.Reason != "operation_unknown" || third.OK || third.Reason != "operation_unknown" || state.PendingAdd == nil || state.PendingAdd.Memo != originalMemo {
		t.Fatalf("未知操作被伪成功或原标记丢失: second=%+v third=%+v state=%+v", second, third, state)
	}
	t.Logf("首次=%s 第二次=%s 第三次=%s adds=%d managed=%v pending=%+v remote=%+v", first.Reason, second.Reason, third.Reason, adds, state.ManagedIPs, state.PendingAdd, entries)
	if adds != 1 || len(state.ManagedIPs) != 1 || state.ManagedIPs[0] != "203.0.113.99" {
		t.Fatal("原 Add 最终落地，但原 marker 已被丢弃，无法恢复本安装添加的 IP")
	}
}

func TestXiequReworkLostRefreshLockMustFenceOldRemoteOperation(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "observer-saves-state"
		if readOnly {
			name = "observer-only-reads-barrier"
		}
		t.Run(name, func(t *testing.T) {
			m := xiequIsolatedManager(t)
			var mu sync.Mutex
			entries := []xiequ.Entry{}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var adds, deletes, verifiedLists atomic.Int32
			var completeResponse atomic.Bool
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Query().Get("act") {
				case "getjson":
					mu.Lock()
					defer mu.Unlock()
					if len(entries) != 0 {
						verifiedLists.Add(1)
					}
					_ = json.NewEncoder(w).Encode(entries)
				case "add":
					if adds.Add(1) == 1 {
						close(entered)
						<-release
					}
					mu.Lock()
					if len(entries) == 0 {
						entries = append(entries, xiequ.Entry{IP: r.URL.Query().Get("ip"), Memo: r.URL.Query().Get("meno")})
					}
					mu.Unlock()
					n, err := w.Write([]byte("ok"))
					completeResponse.Store(err == nil && n == 2)
				case "del":
					deletes.Add(1)
				}
			}))
			c := xiequConfigureFixture(t, m, upstream)
			type outcome struct {
				result xiequRefreshResult
				err    error
			}
			refreshCtx, cancelRefresh := context.WithCancel(t.Context())
			done := make(chan outcome, 1)
			joined := false
			defer func() {
				once.Do(func() { close(release) })
				cancelRefresh()
				if !joined {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("清理时旧刷新未有界退出")
					}
				}
				upstream.Close()
			}()
			go func() {
				r, err := m.refreshXiequWhitelist(refreshCtx, true)
				done <- outcome{r, err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("首次 Add 未进入")
			}
			before, err := m.pg.XiequWhitelistState()
			if err != nil {
				t.Fatal(err)
			}
			original, err := m.pg.XiequOperation(t.Context(), c)
			if err != nil || original == nil || original.Completed || original.Token == "" || original.Memo == "" {
				t.Fatalf("首次 Add 缺少持久未知屏障: %+v %v", original, err)
			}
			lockCtx, cancelLock := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancelLock()
			var pid int
			if err := m.pg.QueryRowContext(lockCtx, `SELECT min(pid) FROM pg_stat_activity WHERE datname=current_database() AND query LIKE 'SELECT pg_try_advisory_lock%' AND pid<>pg_backend_pid() HAVING count(*)=1`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			// 第二参数让 PostgreSQL 有界等待实际退出，不能把已接受终止请求当作已释放锁。
			var terminated, alive bool
			if err := m.pg.QueryRowContext(lockCtx, `SELECT pg_terminate_backend($1,3000)`, pid).Scan(&terminated); err != nil || !terminated {
				t.Fatalf("独占 fixture 锁后端未确认退出: pid=%d err=%v", pid, err)
			}
			if err := m.pg.QueryRowContext(lockCtx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, pid).Scan(&alive); err != nil || alive {
				t.Fatalf("终止后旧后端仍存在: pid=%d alive=%v err=%v", pid, alive, err)
			}
			unlock, locked, err := m.pg.TryXiequRefreshLock(lockCtx, c.WhitelistScope())
			if err != nil || !locked {
				t.Fatalf("旧后端退出后锁仍不可取得: %v", err)
			}
			var unlockOnce sync.Once
			releaseObserverLock := func() { unlockOnce.Do(unlock) }
			defer releaseObserverLock()
			if !readOnly {
				releaseObserverLock()
			}
			// 持有锁时观察者只能读屏障；释放锁时观察者必须取得锁并保存状态。
			second, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
			releaseObserverLock()
			if err != nil || second.OK || second.Reason != "operation_unknown" || adds.Load() != 1 || deletes.Load() != 0 || completeResponse.Load() {
				t.Fatalf("旧 HTTP 仍阻塞时未按屏障拒绝变更: %+v err=%v adds=%d deletes=%d", second, err, adds.Load(), deletes.Load())
			}
			select {
			case first := <-done:
				joined = true
				t.Fatalf("尚未释放 HTTP 就提前结束: %+v", first)
			default:
			}
			afterObserver, err := m.pg.XiequWhitelistState()
			if err != nil || (readOnly && afterObserver.Revision != before.Revision) || (!readOnly && afterObserver.Revision <= before.Revision) {
				t.Fatalf("观察者未进入预期版本分支: readOnly=%v revision=%d->%d err=%v", readOnly, before.Revision, afterObserver.Revision, err)
			}
			observed, err := m.pg.XiequOperation(t.Context(), c)
			if err != nil || observed == nil || *observed != *original {
				t.Fatalf("未知期间原 token/marker/scope 被更换或完成: original=%+v observed=%+v err=%v", original, observed, err)
			}
			once.Do(func() { close(release) })
			var first outcome
			select {
			case first = <-done:
				joined = true
			case <-time.After(5 * time.Second):
				t.Fatal("原调用未在完整响应后退出")
			}
			active, err := m.pg.XiequOperation(t.Context(), c)
			mu.Lock()
			remoteMatches := len(entries) == 1 && entries[0].IP == original.IP && entries[0].Memo == original.Memo
			mu.Unlock()
			if err != nil || first.err != nil || !completeResponse.Load() || verifiedLists.Load() != 1 || !remoteMatches || adds.Load() != 1 || deletes.Load() != 0 {
				t.Fatalf("原调用缺少完整响应与精确结果核验: first=%+v active=%+v err=%v response=%v lists=%d matches=%v adds=%d deletes=%d", first, active, err, completeResponse.Load(), verifiedLists.Load(), remoteMatches, adds.Load(), deletes.Load())
			}
			if readOnly {
				if !first.result.OK || !first.result.Added || first.result.Reason != "added" || active != nil {
					t.Fatalf("完整响应、精确核验且 CAS 未变却未正常完成: first=%+v active=%+v", first, active)
				}
				account, _, _ := strings.Cut(original.Scope, ":")
				raw, ok, err := m.pg.GetSetting("integration.xiequ.operation.v2:" + account)
				var completed db.XiequOperation
				wantCompleted := *original
				wantCompleted.Completed = true
				if err != nil || !ok || json.Unmarshal([]byte(raw), &completed) != nil || completed != wantCompleted {
					t.Fatalf("正常完成未保留原 token/marker/scope 的精确转换: completed=%+v want=%+v present=%v err=%v", completed, wantCompleted, ok, err)
				}
			} else {
				if first.result.OK || first.result.Added || first.result.Reason != "operation_unknown" || active == nil || *active != *original {
					t.Fatalf("观察者推进 revision 后旧 CAS 完成了屏障: first=%+v original=%+v active=%+v", first, original, active)
				}
				// marker 阳性及原调用迟到均不能替代 CAS；换出口仍不得重发。
				discoverXiequPublicIP = func(context.Context, *xiequ.Client) (string, error) { return "203.0.113.100", nil }
				after, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
				if err != nil || after.OK || after.Reason != "operation_unknown" || adds.Load() != 1 || deletes.Load() != 0 {
					t.Fatalf("marker 阳性或迟到响应释放了屏障: %+v err=%v adds=%d deletes=%d", after, err, adds.Load(), deletes.Load())
				}
				active, err = m.pg.XiequOperation(t.Context(), c)
				if err != nil || active == nil || *active != *original {
					t.Fatalf("后续观察丢失原屏障: original=%+v active=%+v err=%v", original, active, err)
				}
			}
			t.Logf("readOnly=%v pid=%d 已退出且锁可取得 revision=%d->%d response=%v verifiedLists=%d originalOK=%v barrier=%v Add=%d Delete=%d", readOnly, pid, before.Revision, afterObserver.Revision, completeResponse.Load(), verifiedLists.Load(), first.result.OK, active != nil, adds.Load(), deletes.Load())
		})
	}
}

func TestXiequOperationCompleteResponseAllowsNormalNextRefresh(t *testing.T) {
	m := xiequIsolatedManager(t)
	entries := []xiequ.Entry{}
	adds, deletes := 0, 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("act") {
		case "getjson":
			_ = json.NewEncoder(w).Encode(entries)
		case "add":
			adds++
			entries = append(entries, xiequ.Entry{IP: r.URL.Query().Get("ip"), Memo: r.URL.Query().Get("meno")})
			_, _ = w.Write([]byte("ok"))
		case "del":
			deletes++
			ip := r.URL.Query().Get("ip")
			for i, e := range entries {
				if e.IP == ip {
					entries = append(entries[:i], entries[i+1:]...)
					break
				}
			}
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstream.Close()
	c := xiequConfigureFixture(t, m, upstream)
	first, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || !first.OK {
		t.Fatalf("正常添加: %+v %v", first, err)
	}
	op, err := m.pg.XiequOperation(t.Context(), c)
	if err != nil || op != nil {
		t.Fatalf("正常完成屏障未释放: %+v %v", op, err)
	}
	for _, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4"} {
		entries = append(entries, xiequ.Entry{IP: ip, Memo: "manual"})
	}
	discoverXiequPublicIP = func(context.Context, *xiequ.Client) (string, error) { return "203.0.113.100", nil }
	next, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil || next.OK || next.Reason != "quota_full" || adds != 1 || deletes != 0 || len(next.Removed) != 0 {
		t.Fatalf("满额时不应自动回收: %+v %v adds=%d deletes=%d", next, err, adds, deletes)
	}
	// 用户在供应商人工管理腾出配额后，正常完整响应允许后续添加。
	entries = entries[1:]
	next, err = (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil || !next.OK || adds != 2 || deletes != 0 {
		t.Fatalf("人工处理后正常添加失败: %+v %v adds=%d deletes=%d", next, err, adds, deletes)
	}
	op, err = m.pg.XiequOperation(t.Context(), c)
	if err != nil || op != nil {
		t.Fatalf("正常 Add 未释放: %+v %v", op, err)
	}
}

func TestXiequUnknownDeleteBlocksRestartAndCredentialRotation(t *testing.T) {
	m := xiequIsolatedManager(t)
	entries := []xiequ.Entry{}
	adds, deletes := 0, 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("act") {
		case "getjson":
			_ = json.NewEncoder(w).Encode(entries)
		case "add":
			adds++
			entries = append(entries, xiequ.Entry{IP: r.URL.Query().Get("ip"), Memo: r.URL.Query().Get("meno")})
			_, _ = w.Write([]byte("ok"))
		case "del":
			deletes++
			ip := r.URL.Query().Get("ip")
			for i, e := range entries {
				if e.IP == ip {
					entries = append(entries[:i], entries[i+1:]...)
					break
				}
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		}
	}))
	defer upstream.Close()
	upstream.Client().Transport.(*http.Transport).DisableKeepAlives = true
	c := xiequConfigureFixture(t, m, upstream)
	first, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || !first.OK {
		t.Fatalf("首次: %+v %v", first, err)
	}
	for _, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4"} {
		entries = append(entries, xiequ.Entry{IP: ip, Memo: "manual"})
	}
	discoverXiequPublicIP = func(context.Context, *xiequ.Client) (string, error) { return "203.0.113.100", nil }
	seedXiequUnknownLegacyDelete(t, m, c)
	second, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || second.Reason != "operation_unknown" || deletes != 0 || adds != 1 {
		t.Fatalf("未知删除继续添加: %+v %v", second, err)
	}
	for _, patch := range []db.XiequConfigPatch{{}, {UKEY: xiequStringPtr("rotated-key")}, {UID: xiequStringPtr("other-account")}, {UID: &c.UID}} {
		current, err := m.pg.SetXiequConfig(t.Context(), patch)
		if err != nil {
			t.Fatal(err)
		}
		if current.UID == c.UID {
			r, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
			if err != nil || r.Reason != "operation_unknown" || deletes != 0 || adds != 1 {
				t.Fatalf("重启/换密钥/ABA绕过删除屏障: %+v %v", r, err)
			}
		}
	}
}

func xiequStringPtr(value string) *string { return &value }

func TestXiequQuotaDoesNotDeleteOwnedOrLegacyIPOnly(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-list-replacement", true: "legacy-ip-only"}[legacy], func(t *testing.T) {
			m := xiequIsolatedManager(t)
			entries := []xiequ.Entry{{IP: "203.0.113.1", Memo: "owned"}, {IP: "203.0.113.2"}, {IP: "203.0.113.3"}, {IP: "203.0.113.4"}, {IP: "203.0.113.5"}}
			lists, changes := 0, 0
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("act") != "getjson" {
					changes++
					_, _ = w.Write([]byte("ok"))
					return
				}
				lists++
				if lists == 2 {
					entries[0].Memo = "operator-manual"
				}
				_ = json.NewEncoder(w).Encode(entries)
			}))
			defer upstream.Close()
			c := xiequConfigureFixture(t, m, upstream)
			s := db.XiequWhitelistState{Scope: c.WhitelistScope(), ManagedIPs: []string{"203.0.113.1"}}
			if !legacy {
				s.ManagedRecords = []db.XiequManagedRecord{{IP: "203.0.113.1", Memo: "owned", Scope: c.WhitelistScope()}}
			}
			if err := m.pg.SetXiequWhitelistState(t.Context(), s); err != nil {
				t.Fatal(err)
			}
			r, err := m.refreshXiequWhitelist(t.Context(), true)
			if err != nil || r.Reason != "quota_full" || changes != 0 {
				t.Fatalf("手工条目/旧 IP-only 被改动: %+v %v changes=%d", r, err, changes)
			}
		})
	}
}

func TestXiequMarkerWithoutAcknowledgementNeverReleasesBarrier(t *testing.T) {
	for _, response := range []string{"", "quota full", "<html>ok</html>"} {
		t.Run(response, func(t *testing.T) {
			m := xiequIsolatedManager(t)
			entries := []xiequ.Entry{}
			changes := 0
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("act") == "getjson" {
					_ = json.NewEncoder(w).Encode(entries)
					return
				}
				changes++
				entries = append(entries, xiequ.Entry{IP: r.URL.Query().Get("ip"), Memo: r.URL.Query().Get("meno")})
				_, _ = w.Write([]byte(response))
			}))
			defer upstream.Close()
			c := xiequConfigureFixture(t, m, upstream)
			first, err := m.refreshXiequWhitelist(t.Context(), true)
			if err != nil || first.OK || first.Reason != "add_unconfirmed" {
				t.Fatalf("含糊响应被当成功: %+v %v", first, err)
			}
			if op, err := m.pg.XiequOperation(t.Context(), c); err != nil || op == nil {
				t.Fatalf("marker 阳性释放: %+v %v", op, err)
			}
			second, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
			if err != nil || second.OK || second.Reason != "operation_unknown" || len(second.ManagedIPs) != 1 || changes != 1 {
				t.Fatalf("未知期间继续修改或丢失归属: %+v %v changes=%d", second, err, changes)
			}
		})
	}
}

func TestXiequCanceledAddRemainsBlockedAfterRemoteLateCommit(t *testing.T) {
	m := xiequIsolatedManager(t)
	entered, release, committed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	entries := map[string][]xiequ.Entry{}
	var changes atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.URL.Query().Get("uid")
		if r.URL.Query().Get("act") == "getjson" {
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(append([]xiequ.Entry{}, entries[uid]...))
			return
		}
		if changes.Add(1) == 1 {
			close(entered)
			<-release
		}
		mu.Lock()
		entries[uid] = append(entries[uid], xiequ.Entry{IP: r.URL.Query().Get("ip"), Memo: r.URL.Query().Get("meno")})
		mu.Unlock()
		if uid == "synthetic-account" {
			close(committed)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer func() { once.Do(func() { close(release) }); upstream.Close() }()
	c := xiequConfigureFixture(t, m, upstream)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan xiequRefreshResult, 1)
	go func() {
		result, err := m.refreshXiequWhitelist(ctx, true)
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("Add 未进入")
	}
	cancel()
	select {
	case r := <-done:
		if r.OK {
			t.Errorf("取消被当作成功: %+v", r)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("取消未有界返回")
	}
	blocked, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil || blocked.Reason != "operation_unknown" || changes.Load() != 1 {
		t.Fatalf("取消后旧操作未完成即重发: %+v %v", blocked, err)
	}
	once.Do(func() { close(release) })
	select {
	case <-committed:
	case <-time.After(3 * time.Second):
		t.Fatal("远端未延迟落地")
	}
	after, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil || after.OK || after.Reason != "operation_unknown" || len(after.ManagedIPs) != 1 || changes.Load() != 1 {
		t.Fatalf("迟到 marker 解除了屏障: %+v %v", after, err)
	}
	// 另一个明确 UID 的账号可正常操作，返回 A 后仍须保留未知屏障和代次隔离。
	b := "account-b"
	if _, err := m.pg.SetXiequConfig(t.Context(), db.XiequConfigPatch{UID: &b}); err != nil {
		t.Fatal(err)
	}
	bResult, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil || !bResult.OK || changes.Load() != 2 {
		t.Fatalf("独立 B 账号不可用: %+v %v", bResult, err)
	}
	if _, err := m.pg.SetXiequConfig(t.Context(), db.XiequConfigPatch{UID: &c.UID}); err != nil {
		t.Fatal(err)
	}
	back, err := (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil || back.Reason != "operation_unknown" || len(back.ManagedIPs) != 0 || changes.Load() != 2 {
		t.Fatalf("ABA 绕过屏障或认领旧代次: %+v %v", back, err)
	}
}

func TestXiequReworkMarkerUnicodeAndNeverSentPending(t *testing.T) {
	m := xiequIsolatedManager(t)
	var adds atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("act") == "add" {
			adds.Add(1)
		}
		_ = json.NewEncoder(w).Encode([]xiequ.Entry{{IP: "203.0.113.99", Memo: "manual"}})
	}))
	defer upstream.Close()
	c := xiequConfigureFixture(t, m, upstream)
	s := db.XiequWhitelistState{Scope: c.WhitelistScope()}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.prepareXiequAdd(ctx, &s, "203.0.113.99", strings.Repeat("测试", 13)); err != nil {
		t.Fatal(err)
	}
	if len(s.PendingAdd.Memo) > 80 || !utf8.ValidString(s.PendingAdd.Memo) {
		t.Fatalf("备注截短失效: %q", s.PendingAdd.Memo)
	}
	start := strings.Index(s.PendingAdd.Memo, "[artex:")
	if start < 0 || len(s.PendingAdd.Memo[start+7:len(s.PendingAdd.Memo)-1]) != 32 {
		t.Fatalf("随机标记不足128bit: %q", s.PendingAdd.Memo)
	}
	r, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || !r.OK || len(r.ManagedIPs) != 0 || adds.Load() != 0 {
		t.Fatalf("未发出的pending误认领手工IP: %+v err=%v adds=%d", r, err, adds.Load())
	}
	persisted, err := m.pg.XiequWhitelistState()
	if err != nil || persisted.PendingAdd != nil {
		t.Fatalf("未发送 pending 没有安全撤销: %+v %v", persisted, err)
	}
	if op, err := m.pg.XiequOperation(t.Context(), c); err != nil || op != nil {
		t.Fatalf("未发送操作错误建立屏障: %+v %v", op, err)
	}
}

func TestXiequReworkStateSaveTimeoutRetainsRecoverablePending(t *testing.T) {
	m := xiequIsolatedManager(t)
	entries := []xiequ.Entry{}
	var blocker interface{ Rollback() error }
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("act") {
		case "getjson":
			_ = json.NewEncoder(w).Encode(entries)
		case "add":
			entries = []xiequ.Entry{{IP: r.URL.Query().Get("ip"), Memo: r.URL.Query().Get("meno")}}
			tx, err := m.pg.BeginTx(context.Background(), nil)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err = tx.Exec(`SELECT value FROM settings FOR UPDATE`); err != nil {
				t.Error(err)
				_ = tx.Rollback()
				return
			}
			blocker = tx
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstream.Close()
	defer func() {
		if blocker != nil {
			_ = blocker.Rollback()
		}
	}()
	xiequConfigureFixture(t, m, upstream)
	start := time.Now()
	r, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || r.OK || r.Reason != "operation_unknown" || r.Warning == "" || time.Since(start) > 5*time.Second {
		t.Fatalf("保存应3s有界失败: r=%+v err=%v elapsed=%v", r, err, time.Since(start))
	}
	if blocker == nil {
		t.Fatal("fixture未取得阻塞事务")
	}
	_ = blocker.Rollback()
	blocker = nil
	s, err := m.pg.XiequWhitelistState()
	if err != nil || s.PendingAdd == nil || s.LastAttemptAt == nil {
		t.Fatalf("持久pending或attempt丢失: %+v err=%v", s, err)
	}
	r, err = (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	if err != nil || r.OK || r.Reason != "operation_unknown" || len(r.ManagedIPs) != 1 {
		t.Fatalf("保存失败后不能从pending恢复: %+v err=%v", r, err)
	}
}

func TestXiequReworkManualReplacementMustNotBeReclaimed(t *testing.T) {
	m := xiequIsolatedManager(t)
	entries := []xiequ.Entry{}
	var deleted []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("act") {
		case "getjson":
			_ = json.NewEncoder(w).Encode(entries)
		case "add":
			if len(entries) < 5 {
				entries = append(entries, xiequ.Entry{IP: r.URL.Query().Get("ip"), Memo: r.URL.Query().Get("meno")})
			}
			_, _ = w.Write([]byte("ok"))
		case "del":
			ip := r.URL.Query().Get("ip")
			deleted = append(deleted, ip)
			for i, e := range entries {
				if e.IP == ip {
					entries = append(entries[:i], entries[i+1:]...)
					break
				}
			}
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstream.Close()
	xiequConfigureFixture(t, m, upstream)
	r, err := m.refreshXiequWhitelist(t.Context(), true)
	if err != nil || !r.OK || len(r.ManagedIPs) != 1 {
		t.Fatalf("首次添加失败: %+v %v", r, err)
	}
	// 操作员已删除托管条目并以相同 IP 手工重建，期间本安装没有刷新。
	entries = []xiequ.Entry{{IP: "203.0.113.99", Memo: "operator-created-manual"}, {IP: "203.0.113.2", Memo: "manual"}, {IP: "203.0.113.3", Memo: "manual"}, {IP: "203.0.113.4", Memo: "manual"}, {IP: "203.0.113.5", Memo: "manual"}}
	discoverXiequPublicIP = func(context.Context, *xiequ.Client) (string, error) { return "203.0.113.100", nil }
	r, err = (&Manager{pg: m.pg}).refreshXiequWhitelist(t.Context(), true)
	t.Logf("手工重建同IP后删除=%v result=%+v err=%v", deleted, r, err)
	if len(deleted) != 0 {
		t.Fatal("丢弃已确认条目的marker后，仅按ManagedIPs匹配删除了手工重建记录")
	}
}
