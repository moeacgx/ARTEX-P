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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	localagent "github.com/Autumn-27/artex/integrations/local-agent"
)

func TestLocalAgentProxyAuthenticationBoundary(t *testing.T) {
	for _, path := range []string{"/api/local-agent/proxies", "/api/local-agent/proxies/1", "/api/local-agent/proxies/1/test"} {
		if !isLocalAgentSignedPath(path) {
			t.Fatalf("设备签名业务路径被管理员 JWT 拦截: %s", path)
		}
	}
	for _, path := range []string{"/api/local-agent/proxies-admin", "/api/local-agent/devices", "/api/local-agent/devices/1/approve", "/api/local-agent/skill.zip"} {
		if isLocalAgentSignedPath(path) {
			t.Fatalf("管理员路径被错误放行: %s", path)
		}
	}
	s := &Server{}
	mux := http.NewServeMux()
	s.registerLocalAgentRoutes(mux)
	for _, endpoint := range []struct{ method, path string }{
		{"GET", "/api/local-agent/proxies"},
		{"GET", "/api/local-agent/proxies/1"},
		{"POST", "/api/local-agent/proxies/1/test"},
	} {
		w := httptest.NewRecorder()
		s.requireAuth(mux).ServeHTTP(w, httptest.NewRequest(endpoint.method, endpoint.path, nil))
		// 无设备签名必须被本地 Agent 鉴权拒绝，不能匿名到达业务处理器。
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("无签名请求未拒绝 %s: %d", endpoint.path, w.Code)
		}
	}
}

func TestLocalAgentProxyPG(t *testing.T) {
	dsn := os.Getenv("ARTEX_LOCAL_AGENT_TEST_DSN")
	if dsn == "" {
		t.Skip("未配置隔离 PostgreSQL")
	}
	if err := db.ConfigureProxySecrets(filepath.Join(t.TempDir(), "proxy.key")); err != nil {
		t.Fatal(err)
	}
	if err := db.InitializeProxySecrets(); err != nil {
		t.Fatal(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	pg.SetMaxOpenConns(1)
	if _, err := pg.Exec(`CREATE TEMP TABLE local_agent_devices (
		id BIGSERIAL PRIMARY KEY,name TEXT NOT NULL,public_key TEXT UNIQUE NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',scopes TEXT[] NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),approved_at TIMESTAMPTZ,last_used_at TIMESTAMPTZ,
		skill_version TEXT NOT NULL DEFAULT '',skill_reported_at TIMESTAMPTZ);
		CREATE TEMP TABLE local_agent_nonces(public_key TEXT,nonce TEXT,created_at TIMESTAMPTZ DEFAULT now(),PRIMARY KEY(public_key,nonce));
		CREATE TEMP TABLE local_agent_audit(id BIGSERIAL PRIMARY KEY,device_id BIGINT,method TEXT,path TEXT,status_code INTEGER,created_at TIMESTAMPTZ DEFAULT now(),completed_at TIMESTAMPTZ);`); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodConnect {
			// 测试不访问外网；固定 HTTPS 默认目标在此明确失败。
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ip=8.8.8.8\nwarp=off\n")
	}))
	defer proxy.Close()
	p, err := pg.CreateProxy(&db.Proxy{Name: "本地 Agent 检测", Kind: db.ProxyKindHTTP, URL: strings.Replace(proxy.URL, "://", "://alice:local-proxy-secret@", 1), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Exec(`DELETE FROM outbound_proxies WHERE id=$1`, p.ID)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var deviceID int64
	if err := pg.QueryRow(`INSERT INTO local_agent_devices(name,public_key,status) VALUES('测试设备',$1,'approved') RETURNING id`, base64.StdEncoding.EncodeToString(public)).Scan(&deviceID); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: pg}}
	mux := http.NewServeMux()
	s.registerLocalAgentRoutes(mux)
	handler := s.requireAuth(mux)
	call := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		r := signedLocalAgentRequest(t, private, method, path, nil)
		r.Header.Set("X-ARTEX-Device-ID", strconv.FormatInt(deviceID, 10))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	path := "/api/local-agent/proxies/" + strconv.FormatInt(p.ID, 10)
	for _, endpoint := range []struct{ method, path, scope string }{
		{"GET", "/api/local-agent/proxies", "proxies.read"},
		{"GET", path, "proxies.read"},
		{"POST", path + "/test", "proxies.test"},
	} {
		w := call(endpoint.method, endpoint.path)
		if w.Code != 403 || !strings.Contains(w.Body.String(), endpoint.scope) {
			t.Fatalf("缺权限未拒绝 %s: %d %s", endpoint.path, w.Code, w.Body.String())
		}
	}
	if requests.Load() != 0 {
		t.Fatal("缺少检测权限仍发起出站请求")
	}
	if _, err := pg.Exec(`UPDATE local_agent_devices SET scopes=ARRAY['proxies.read'] WHERE id=$1`, deviceID); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"/api/local-agent/proxies", path} {
		w := call("GET", endpoint)
		if w.Code != 200 || strings.Contains(w.Body.String(), "local-proxy-secret") {
			t.Fatalf("读取未成功或泄漏凭据: %d %s", w.Code, w.Body.String())
		}
	}
	if w := call("POST", path+"/test"); w.Code != 403 {
		t.Fatal("读取权限错误包含检测权限")
	}
	if _, err := pg.Exec(`UPDATE local_agent_devices SET scopes=ARRAY['proxies.test'] WHERE id=$1`, deviceID); err != nil {
		t.Fatal(err)
	}
	w := call("POST", path+"/test")
	if w.Code != 502 {
		t.Fatalf("固定目标失败仍报告健康: %d %s", w.Code, w.Body.String())
	}
	if requests.Load() != 1 || strings.Contains(w.Body.String(), "local-proxy-secret") {
		t.Fatal("重复探测或输出泄漏代理凭据")
	}
	var auditCount int
	if err := pg.QueryRow(`SELECT count(*) FROM local_agent_audit WHERE device_id=$1 AND status_code=502 AND completed_at IS NOT NULL`, deviceID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("检测审计数量=%d 错误=%v", auditCount, err)
	}
	checked, ip, code := s.probeProxyCenterIP(t.Context(), p.ID, "http://unresolvable.invalid/trace")
	if code != "" || ip != "8.8.8.8" || checked.Status != db.ProxyStatusHealthy || checked.LastCheckedAt == nil || requests.Load() != 2 {
		t.Fatalf("本次出口结果错误: IP=%q 代码=%q 请求=%d", ip, code, requests.Load())
	}
}

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

func TestLocalAgentSkillMigrationPG(t *testing.T) {
	dsn := os.Getenv("ARTEX_LOCAL_AGENT_TEST_DSN")
	if dsn == "" {
		t.Skip("未配置隔离 PostgreSQL")
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	// 临时旧表隔离现有数据，验证旧设备经过真实迁移仍然是版本未知。
	if _, err := sqlDB.Exec(`CREATE TEMP TABLE local_agent_devices(id BIGINT PRIMARY KEY);
		INSERT INTO local_agent_devices(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile(filepath.Join("..", "db", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{}
	for _, statement := range strings.Split(string(schema), ";") {
		if index := strings.Index(statement, "ALTER TABLE local_agent_devices ADD COLUMN"); index >= 0 {
			statements = append(statements, statement[index:])
		}
	}
	if len(statements) == 0 {
		t.Fatal("缺少旧设备版本迁移")
	}
	for range 2 {
		for _, statement := range statements {
			if _, err := sqlDB.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	var version string
	var reportedAt *time.Time
	if err := sqlDB.QueryRow(`SELECT skill_version,skill_reported_at FROM local_agent_devices WHERE id=1`).Scan(&version, &reportedAt); err != nil || version != "" || reportedAt != nil {
		t.Fatalf("旧设备版本=%q 上报=%v 错误=%v", version, reportedAt, err)
	}
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
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),approved_at TIMESTAMPTZ,last_used_at TIMESTAMPTZ,
		skill_version TEXT NOT NULL DEFAULT '',skill_reported_at TIMESTAMPTZ);
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
	// 上报必须经过验签；只下载新包不能改变设备的安装状态。
	skill, err := localagent.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	status := func(version string, tamper bool) map[string]any {
		t.Helper()
		path := "/api/local-agent/status"
		if version != "" {
			path += "?_skill_version=" + version
		}
		r := signedLocalAgentRequest(t, private, http.MethodGet, path, nil)
		r.Header.Set("X-ARTEX-Device-ID", strconv.FormatInt(id, 10))
		if tamper {
			r.URL.RawQuery = "_skill_version=" + skill.Version
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if tamper {
			if w.Code != 401 {
				t.Fatalf("篡改版本应拒绝：%d %s", w.Code, w.Body.String())
			}
			return nil
		}
		if w.Code != 200 {
			t.Fatalf("状态上报：%d %s", w.Code, w.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	oldVersion := strings.Repeat("0", 64)
	if result := status(oldVersion, false); result["skill_version"] != oldVersion || result["skill_update_available"] != true || result["skill_reported_at"] == nil {
		t.Fatalf("不同技能包未提示更新：%v", result)
	}
	status(oldVersion, true)
	download := httptest.NewRequest(http.MethodGet, "/api/local-agent/skill", nil)
	download.Header.Set("Authorization", "Bearer "+adminToken)
	downloadResponse := httptest.NewRecorder()
	handler.ServeHTTP(downloadResponse, download)
	if downloadResponse.Code != 200 {
		t.Fatalf("下载：%d", downloadResponse.Code)
	}
	var persisted string
	if err := sqlDB.QueryRow(`SELECT skill_version FROM local_agent_devices WHERE id=$1`, id).Scan(&persisted); err != nil || persisted != oldVersion {
		t.Fatalf("篡改或下载不应改变安装版本：%s %v", persisted, err)
	}
	if result := status(skill.Version, false); result["skill_update_available"] != false || result["skill_version"] != skill.Version {
		t.Fatalf("新版上报未消除更新提示：%v", result)
	}
	if result := status("", false); result["skill_version"] != "" || result["skill_reported_at"] != nil || result["skill_update_available"] != false {
		t.Fatalf("旧客户端应显示未知：%v", result)
	}
	list := httptest.NewRequest(http.MethodGet, "/api/local-agent/devices", nil)
	list.Header.Set("Authorization", "Bearer "+adminToken)
	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, list)
	var overview struct {
		Devices []localAgentDevice `json:"devices"`
		Skill   localagent.Info    `json:"skill"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &overview); err != nil || listed.Code != 200 || len(overview.Devices) != 1 || overview.Skill.Version != skill.Version {
		t.Fatalf("设备页面缺少版本信息：%d %s %v", listed.Code, listed.Body.String(), err)
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

func TestLocalAgentSkillVersionQuery(t *testing.T) {
	version := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, query, want string
		invalid           bool
	}{
		{"旧客户端", "path=hello%20world", "", false},
		{"已签名版本", "path=hello%20world&_skill_version=" + version, version, false},
		{"空版本", "_skill_version=", "", true},
		{"短版本", "_skill_version=abc", "", true},
		{"非摘要", "_skill_version=" + strings.Repeat("z", 64), "", true},
		{"大写摘要", "_skill_version=" + strings.Repeat("A", 64), "", true},
		{"重复参数", "_skill_version=" + version + "&_skill_version=" + version, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/local-agent/status?"+tc.query, nil)
			got, err := localAgentConsumeSkillVersion(r)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("版本=%q 错误=%v", got, err)
			}
			if !tc.invalid && r.URL.Query().Has("_skill_version") {
				t.Fatal("内部参数传入业务接口")
			}
			if strings.HasPrefix(tc.query, "path=") && r.URL.Query().Get("path") != "hello world" {
				t.Fatal("业务查询参数丢失")
			}
		})
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
	admin("approve", `{"scopes":["tasks.read","tasks.create","skills.read","skills.upload","skills.write","workspace.read","workspace.write","workspace.upload","workspace.download","assets.read","assets.write","conversations.read","conversations.create"]}`)
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
	cli("skills", "files", "--name", "local-cli-proof")
	cli("skills", "write", "--name", "local-cli-proof", "--path", "references/proof.txt", "--content", "技能往返测试")
	if out := cli("skills", "read", "--name", "local-cli-proof", "--path", "references/proof.txt"); !bytes.Contains(out, []byte("技能往返测试")) {
		t.Fatalf("技能读取未返回实际内容: %s", out)
	}
	downloadedSkill := filepath.Join(t.TempDir(), "download.zip")
	cli("skills", "download", "--name", "local-cli-proof", "--output", downloadedSkill)
	zr, err := zip.OpenReader(downloadedSkill)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if _, err := zr.Open("local-cli-proof/references/proof.txt"); err != nil {
		t.Fatalf("下载包缺少实际写入文件: %v", err)
	}
	cli("workspace", "mkdir", "--path", "cli-audit")
	cli("workspace", "write", "--path", "cli-audit/proof.txt", "--content", "工作区往返测试")
	if out := cli("workspace", "read", "--path", "cli-audit/proof.txt"); !bytes.Contains(out, []byte("工作区往返测试")) {
		t.Fatalf("工作区读取未返回实际内容: %s", out)
	}
	binary := []byte{0, 1, 127, 128, 255}
	uploaded := filepath.Join(t.TempDir(), "binary.dat")
	if err := os.WriteFile(uploaded, binary, 0600); err != nil {
		t.Fatal(err)
	}
	cli("workspace", "upload", "--path", "cli-audit", "--file", uploaded)
	downloaded := filepath.Join(t.TempDir(), "binary.dat")
	cli("workspace", "download", "--path", "cli-audit/binary.dat", "--output", downloaded)
	if content, err := os.ReadFile(downloaded); err != nil || sha256.Sum256(content) != sha256.Sum256(binary) {
		t.Fatalf("工作区二进制 SHA 不一致: %v", err)
	}
	assetsFile := filepath.Join(t.TempDir(), "assets.json")
	if err := os.WriteFile(assetsFile, []byte(`{"assets":[{"type":"ip","ip":"192.0.2.211"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var imported struct {
		Results []struct {
			ID int64 `json:"id"`
		} `json:"results"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(cli("assets", "upload", "--file", assetsFile), &imported); err != nil || len(imported.Errors) != 0 || len(imported.Results) != 1 {
		t.Fatalf("资产写入失败: %+v %v", imported, err)
	}
	defer m.pg.Exec(`DELETE FROM assets WHERE id=$1`, imported.Results[0].ID)
	exportedFile := filepath.Join(t.TempDir(), "export.json")
	cli("assets", "download", "--type", "ip", "--dsl", `ip="192.0.2.211"`, "--all", "--output", exportedFile)
	var exported struct {
		Assets []db.Asset `json:"assets"`
	}
	content, err := os.ReadFile(exportedFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &exported); err != nil || len(exported.Assets) != 1 || exported.Assets[0].IP != "192.0.2.211" {
		t.Fatalf("资产导出未返回实际写入内容: %s %v", content, err)
	}
	var catalog struct {
		Agents []struct {
			Key string `json:"key"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(cli("conversations", "agents"), &catalog); err != nil || len(catalog.Agents) == 0 {
		t.Fatalf("Agent 目录为空: %v", err)
	}
	cli("conversations", "create", "--agent-key", catalog.Agents[0].Key, "--title", "CLI隔离验证")
	var count int
	if err := m.pg.QueryRow(`SELECT count(*) FROM local_agent_audit WHERE device_id=$1 AND status_code IN (200,201) AND completed_at IS NOT NULL`, paired.DeviceID).Scan(&count); err != nil || count != 8 {
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
