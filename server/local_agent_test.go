package server

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func signedLocalAgentRequest(t *testing.T, private ed25519.PrivateKey, method, path string, body []byte) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	digest := sha256.Sum256(body)
	message := method + "\n" + r.URL.RequestURI() + "\n" + ts + "\n" + nonce + "\n" + hex.EncodeToString(digest[:])
	r.Header.Set("X-ARTEX-Timestamp", ts)
	r.Header.Set("X-ARTEX-Nonce", nonce)
	r.Header.Set("X-ARTEX-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(private, []byte(message))))
	return r
}

// 仅使用显式测试 DSN；临时表覆盖同名表，生命周期测试不会修改已有设备。
func TestLocalAgentLifecyclePG(t *testing.T) {
	dsn := os.Getenv("ARTEX_LOCAL_AGENT_TEST_DSN")
	if dsn == "" {
		t.Skip("未配置隔离的本地 Agent PostgreSQL 测试 DSN")
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	_, err = sqlDB.Exec(`CREATE TEMP TABLE local_agent_devices (
		id BIGSERIAL PRIMARY KEY,name TEXT NOT NULL,public_key TEXT UNIQUE NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',scopes TEXT[] NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),approved_at TIMESTAMPTZ,last_used_at TIMESTAMPTZ);
		CREATE TEMP TABLE local_agent_nonces(public_key TEXT,nonce TEXT,created_at TIMESTAMPTZ DEFAULT now(),PRIMARY KEY(public_key,nonce));
		CREATE TEMP TABLE local_agent_audit(id BIGSERIAL PRIMARY KEY,device_id BIGINT,method TEXT,path TEXT,status_code INTEGER,created_at TIMESTAMPTZ DEFAULT now(),completed_at TIMESTAMPTZ);`)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: &db.DB{DB: sqlDB}}, jwtKey: []byte("local-agent-test-admin-signing-key")}
	mux := http.NewServeMux()
	s.registerLocalAgentRoutes(mux)
	invoked := 0
	mux.HandleFunc("POST /api/local-agent/test", s.localAgentWithScope("tasks.create", func(w http.ResponseWriter, r *http.Request) { invoked++; w.WriteHeader(201) }))
	handler := s.requireAuth(mux)
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	encoded := base64.StdEncoding.EncodeToString(public)
	pairBody := []byte(`{"name":"测试设备","public_key":"` + encoded + `"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/local-agent/pair", bytes.NewReader(pairBody))
	r.Header.Set("X-ARTEX-Pair-Proof", base64.StdEncoding.EncodeToString(ed25519.Sign(private, []byte("ARTEX local-agent pair v1\n测试设备\n"+encoded))))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("配对：%d %s", w.Code, w.Body.String())
	}
	var id int64
	if err := sqlDB.QueryRow(`SELECT id FROM local_agent_devices WHERE public_key=$1`, encoded).Scan(&id); err != nil {
		t.Fatal(err)
	}
	call := func(nonce string) int {
		r := signedLocalAgentRequest(t, private, http.MethodPost, "/api/local-agent/test", nil)
		r.Header.Set("X-ARTEX-Device-ID", strconv.FormatInt(id, 10))
		if nonce != "" {
			r.Header.Set("X-ARTEX-Nonce", nonce)
			digest := sha256.Sum256(nil)
			message := r.Method + "\n" + r.URL.RequestURI() + "\n" + r.Header.Get("X-ARTEX-Timestamp") + "\n" + nonce + "\n" + hex.EncodeToString(digest[:])
			r.Header.Set("X-ARTEX-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(private, []byte(message))))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	if code := call(""); code != 403 {
		t.Fatalf("未审批设备：%d", code)
	}
	adminToken, _ := signJWT(s.jwtKey)
	admin := func(path, body string) int {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+adminToken)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	approvePath := "/api/local-agent/devices/" + strconv.FormatInt(id, 10) + "/approve"
	if code := admin(approvePath, `{"scopes":["tasks.read"]}`); code != 200 {
		t.Fatalf("只读审批：%d", code)
	}
	if code := call(""); code != 403 {
		t.Fatalf("缺少创建权限：%d", code)
	}
	if code := admin(approvePath, `{"scopes":["tasks.create"]}`); code != 200 {
		t.Fatalf("创建审批：%d", code)
	}
	if code := call("once-only-nonce-1234"); code != 201 {
		t.Fatalf("授权调用：%d", code)
	}
	if code := call("once-only-nonce-1234"); code != 409 {
		t.Fatalf("重放请求：%d", code)
	}
	if invoked != 1 {
		t.Fatalf("业务执行 %d 次", invoked)
	}
	var auditCount int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM local_agent_audit WHERE device_id=$1 AND status_code=201 AND completed_at IS NOT NULL`, id).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("操作审计数量=%d，错误=%v", auditCount, err)
	}
	if code := admin("/api/local-agent/devices/"+strconv.FormatInt(id, 10)+"/revoke", ""); code != 200 {
		t.Fatalf("撤销：%d", code)
	}
	if code := call(""); code != 403 {
		t.Fatalf("撤销后调用：%d", code)
	}
	if code := admin(approvePath, `{"scopes":["tasks.create"]}`); code != 404 {
		t.Fatalf("撤销设备被重新审批：%d", code)
	}
}

// 使用真实 Node 客户端与真实 PostgreSQL 验证协议和业务复用，任务执行保持关闭。
func TestLocalAgentCLIBusinessPG(t *testing.T) {
	dsn := os.Getenv("ARTEX_LOCAL_AGENT_TEST_DSN")
	if dsn == "" {
		t.Skip("未配置隔离 PostgreSQL")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_PG_DSN", dsn)
	t.Setenv("ARTEX_AGENT_CONFIG_DIR", filepath.Join(t.TempDir(), "identity"))
	if err := db.ConfigureProxySecrets(filepath.Join(t.TempDir(), "proxy.key")); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := newAdmissionTestServer(m, func(*Task) bool { return false })
	s.skillDir = t.TempDir()
	s.jwtKey = []byte("isolated-cli-business-test-key")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	script, err := filepath.Abs(filepath.Join("..", "scripts", "artex-agent.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	cli := func(args ...string) []byte {
		t.Helper()
		command := exec.CommandContext(t.Context(), node, append([]string{script}, args...)...)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		out, err := command.Output()
		if err != nil {
			t.Fatalf("CLI %v: %v: %s", args, err, stderr.Bytes())
		}
		return out
	}
	var paired struct {
		DeviceID string `json:"deviceId"`
	}
	if err := json.Unmarshal(cli("auth", "login", "--url", srv.URL, "--name", "本地联调"), &paired); err != nil {
		t.Fatal(err)
	}
	token, _ := signJWT(s.jwtKey)
	admin := func(action, body string) {
		t.Helper()
		r, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/local-agent/devices/"+paired.DeviceID+"/"+action, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		response, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			b, _ := io.ReadAll(response.Body)
			t.Fatalf("管理员操作：%d %s", response.StatusCode, b)
		}
	}
	admin("approve", `{"scopes":["tasks.read","tasks.create","skills.read","skills.upload"]}`)
	cli("auth", "status")
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(cli("tasks", "create", "--description", "本地CLI协议联调", "--goal", "仅验证入队"), &created); err != nil || created.ID == "" {
		t.Fatalf("创建失败：%v", err)
	}
	cli("tasks", "get", "--id", created.ID)
	cli("tasks", "list")
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	manifest, _ := zw.Create("SKILL.md")
	_, _ = manifest.Write([]byte("---\nname: local-cli-proof\ndescription: 联调测试\n---\n测试内容\n"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "skill.zip")
	if err := os.WriteFile(file, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	cli("skills", "upload", "--file", file)
	if _, err := os.Stat(filepath.Join(s.skillDir, "local-cli-proof", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	cli("skills", "list")
	var count int
	if err := m.pg.QueryRow(`SELECT count(*) FROM local_agent_audit WHERE device_id=$1 AND status_code IN (200,201) AND completed_at IS NOT NULL`, paired.DeviceID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("业务审计数量=%d: %v", count, err)
	}
	admin("revoke", "")
	command := exec.CommandContext(t.Context(), node, script, "tasks", "list")
	if err := command.Run(); err == nil {
		t.Fatal("撤销设备仍可查询任务")
	}
}

func TestLocalAgentSkillDownload(t *testing.T) {
	s := &Server{jwtKey: []byte("download-test-admin-key")}
	mux := http.NewServeMux()
	s.registerLocalAgentRoutes(mux)
	h := s.requireAuth(mux)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/local-agent/skill", nil)
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("下载必须登录")
	}
	token, _ := signJWT(s.jwtKey)
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("下载失败：%d", w.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"artex-agent/SKILL.md", "artex-agent/scripts/artex-agent.mjs"} {
		if _, err := zr.Open(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalAgentSignatureIntegrity(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(public)
	body := []byte(`{"description":"签名验证"}`)
	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"方法", func(r *http.Request) { r.Method = http.MethodGet }},
		{"路径", func(r *http.Request) { r.URL.Path = "/api/local-agent/skills" }},
		{"查询参数", func(r *http.Request) { r.URL.RawQuery = "overwrite=true" }},
		{"请求体", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(`{}`)) }},
		{"过期", func(r *http.Request) {
			r.Header.Set("X-ARTEX-Timestamp", strconv.FormatInt(time.Now().Add(-6*time.Minute).Unix(), 10))
		}},
		{"未来时间", func(r *http.Request) {
			r.Header.Set("X-ARTEX-Timestamp", strconv.FormatInt(time.Now().Add(6*time.Minute).Unix(), 10))
		}},
		{"短nonce", func(r *http.Request) { r.Header.Set("X-ARTEX-Nonce", "short") }},
		{"非法nonce", func(r *http.Request) { r.Header.Set("X-ARTEX-Nonce", "invalid\nnonce___") }},
		{"缺少签名", func(r *http.Request) { r.Header.Del("X-ARTEX-Signature") }},
	}
	r := signedLocalAgentRequest(t, private, http.MethodPost, "/api/local-agent/tasks", body)
	if err := localAgentVerifySignature(r, encoded, 1024); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r.Body)
	if !bytes.Equal(got, body) {
		t.Fatalf("处理器未收到原始请求体：%q", got)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := signedLocalAgentRequest(t, private, http.MethodPost, "/api/local-agent/tasks", body)
			tt.mutate(r)
			if err := localAgentVerifySignature(r, encoded, 1024); err == nil {
				t.Fatal("篡改请求被接受")
			}
		})
	}
	if err := localAgentVerifySignature(signedLocalAgentRequest(t, private, http.MethodPost, "/api/local-agent/tasks", body), encoded, 1); err == nil {
		t.Fatal("超大请求被接受")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := localAgentVerifySignature(signedLocalAgentRequest(t, private, http.MethodPost, "/api/local-agent/tasks", body), base64.StdEncoding.EncodeToString(other), 1024); err == nil {
		t.Fatal("错误设备公钥被接受")
	}
}

func TestLocalAgentAdminRoutesRequireJWT(t *testing.T) {
	s := &Server{jwtKey: []byte("local-agent-admin-route-test-key")}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	for _, path := range []string{"/api/local-agent/devices", "/api/local-agent/devices/1/approve", "/api/local-agent/devices/1/revoke", "/api/local-agent/unknown"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, nil)
			w := httptest.NewRecorder()
			s.requireAuth(next).ServeHTTP(w, r)
			if w.Code != 401 {
				t.Fatalf("未登录得到 %d", w.Code)
			}
			r.Header.Set("Authorization", "Bearer "+token)
			w = httptest.NewRecorder()
			s.requireAuth(next).ServeHTTP(w, r)
			if w.Code != 204 {
				t.Fatalf("管理员得到 %d", w.Code)
			}
		})
	}
}
