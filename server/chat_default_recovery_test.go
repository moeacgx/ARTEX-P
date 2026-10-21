package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 默认继承不能被启动时出口故障留下的全局实例快照永久拦住。
func TestChatDefaultRecoversAfterStartupProxyFailure(t *testing.T) {
	s := newModelExecutionServer(t)
	oldAugment, oldResolve, oldPrompt := agent.ToolAugment, agent.ToolResolve, agent.PromptOverride
	agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = nil, nil, nil
	t.Cleanup(func() {
		waitDefaultChatIdle(t, s)
		agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = oldAugment, oldResolve, oldPrompt
	})
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		seen = append(seen, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "默认恢复成功：" + body.Model}, "finish_reason": "stop",
		}}})
	}))
	t.Cleanup(upstream.Close)
	profileID, _ := executionModels(t, s, upstream.URL)
	otherID, _ := executionModels(t, s, upstream.URL)
	if _, err := s.m.pg.ReplaceLLMModels(otherID, []db.LLMModelInput{{UpstreamModelID: "other-default", DisplayName: "另一个默认模型", Enabled: true, Default: true}}); err != nil {
		t.Fatal(err)
	}
	oldActive, err := s.m.pg.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if oldActive != nil {
			_ = s.m.pg.SetActiveProfile(oldActive.ID)
		} else {
			_, _ = s.m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id IN ($1,$2)`, profileID, otherID)
		}
	})
	if err := s.m.pg.SetActiveProfile(profileID); err != nil {
		t.Fatal(err)
	}
	oldProxy, err := s.m.pg.GlobalProxyID()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.SetGlobalProxyID(oldProxy) })
	// 关闭轮询以便准确核对默认与显式渠道请求的是同一个上游模型。
	for _, key := range []string{settingLLMPoolOn, settingLLMPoolBindFallback} {
		previous, exists, err := s.m.pg.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if exists {
				_ = s.m.pg.SetSetting(key, previous)
			} else {
				_, _ = s.m.pg.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		})
		if err := s.m.pg.SetSetting(key, "false"); err != nil {
			t.Fatal(err)
		}
	}
	proxy, err := s.m.pg.CreateProxy(&db.Proxy{Name: "默认对话启动出口回归", Kind: db.ProxyKindWarp, URL: "socks5://127.0.0.1:19119", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM outbound_proxies WHERE id=$1`, proxy.ID) })
	// 模拟数据库记着上次健康，但新进程尚未恢复本地 WARP 的启动窗口。
	if _, err := s.m.pg.Exec(`UPDATE outbound_proxies SET status='healthy' WHERE id=$1`, proxy.ID); err != nil {
		t.Fatal(err)
	}
	// 会话原先已经存在；不能在故障出口下创建，否则会先被创建准入拒绝。
	if err := s.m.pg.SetGlobalProxyID(nil); err != nil {
		t.Fatal(err)
	}
	key := "default_chat_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := s.m.pg.CreateAgent(key, "默认渠道恢复回归", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		waitDefaultChatIdle(t, s)
		_, _ = s.m.pg.Exec(`DELETE FROM conversations WHERE agent_key=$1`, key)
		_ = s.m.pg.DeleteAgent(key)
	})
	api := modelHTTPClient(t, s)
	create := func(selection string) int64 {
		t.Helper()
		conv := api("POST", "/api/conversations", fmt.Sprintf(`{"agent_key":%q,"profile_id":%s,"model_mode":"channel-default","model_id":null}`, key, selection), 200)
		return int64(conv["id"].(float64))
	}
	cid := create("null")
	if _, err := s.m.pg.Exec(`INSERT INTO settings(key,value) VALUES('global_proxy_id',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, strconv.FormatInt(proxy.ID, 10)); err != nil {
		t.Fatal(err)
	}
	cfg, ok := s.loadLLMConfig()
	if !ok {
		t.Fatal("测试默认渠道无效")
	}
	if err := s.applyLLM(cfg); !errors.Is(err, db.ErrProxyUnavailable) {
		t.Fatalf("未复现启动出口故障：%v", err)
	}
	assertStartupChatAbsent(t, s)
	messagePath := fmt.Sprintf("/api/conversations/%d/messages", cid)
	api("POST", messagePath, `{"message":"出口未恢复不能发送"}`, 503)
	mu.Lock()
	called := len(seen)
	mu.Unlock()
	if called != 0 {
		t.Fatal("出口未恢复时偷偷请求了上游")
	}
	// 用正式配置入口解除故障出口，保持启动失败留下的实例与标志原样。
	if err := s.m.pg.SetGlobalProxyID(nil); err != nil {
		t.Fatal(err)
	}
	api("POST", messagePath, `{"message":"默认渠道恢复后直接发送"}`, 202)
	waitDefaultChatResult(t, s, cid, "默认恢复成功：real-a")
	assertStartupChatAbsent(t, s)
	pinnedID := create(strconv.Quote(strconv.FormatInt(profileID, 10)))
	api("POST", fmt.Sprintf("/api/conversations/%d/messages", pinnedID), `{"message":"显式选择同一渠道"}`, 202)
	waitDefaultChatResult(t, s, pinnedID, "默认恢复成功：real-a")
	// 同一默认会话后续轮次必须跟随新的激活渠道，不能把恢复变成固定绑定。
	if err := s.m.pg.SetActiveProfile(otherID); err != nil {
		t.Fatal(err)
	}
	api("POST", messagePath, `{"message":"跟随新的默认渠道"}`, 202)
	waitDefaultChatResult(t, s, cid, "默认恢复成功：other-default")
	assertStartupChatAbsent(t, s)
	mu.Lock()
	got := fmt.Sprint(seen)
	mu.Unlock()
	if got != "[real-a real-a other-default]" {
		t.Fatalf("默认/显式/切换渠道的真实模型请求错误：%s", got)
	}
	c, err := s.m.pg.GetConversation(cid)
	if err != nil || c.LLMProfileID != nil {
		t.Fatalf("默认会话被写成固定覆盖：%+v，%v", c, err)
	}
}

func assertStartupChatAbsent(t *testing.T, s *Server) {
	t.Helper()
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.llmOn || s.chatAgent != nil {
		t.Fatal("测试意外修补了启动快照，未验证动态恢复路径")
	}
}

func waitDefaultChatIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.chatMu.Lock()
		busy := len(s.chatBusy) != 0
		s.chatMu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("回归测试对话未及时退出")
}

func waitDefaultChatResult(t *testing.T, s *Server, cid int64, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items, _, err := s.m.pg.ConvActivityList(cid, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.Kind == "result" && !item.IsError && item.Summary == want {
				waitDefaultChatIdle(t, s)
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	items, _, _ := s.m.pg.ConvActivityList(cid, 0, 100)
	t.Fatalf("真实上游响应未落入会话活动，期望 %q，实际 %+v", want, items)
}
