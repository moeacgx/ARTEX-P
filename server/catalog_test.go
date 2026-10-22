package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/catalog"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/skill"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type catalogTestTransport func(*http.Request) (*http.Response, error)

func (f catalogTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func catalogTestServer(t *testing.T) *Server {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("未配置隔离PostgreSQL：%v", err)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	schema := fmt.Sprintf("catalog_http_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	cfg.RuntimeParams["search_path"] = schema
	pool := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = pool.Close() })
	pg := &db.DB{DB: pool}
	b, err := os.ReadFile("../db/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pg.Exec(string(b)); err != nil {
		t.Fatal(err)
	}
	if err = pg.MigrateLLMModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &Server{m: &Manager{pg: pg}, skillDir: filepath.Join(t.TempDir(), "skills")}
}

func catalogJSONRequest(t *testing.T, handler http.HandlerFunc, req any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/api/catalog/test", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func catalogSkillFixture(t *testing.T) {
	t.Helper()
	old := catalogClient
	t.Cleanup(func() { catalogClient = old })
	catalogClient = catalog.NewClient()
	catalogClient.HTTP.Transport = catalogTestTransport(func(r *http.Request) (*http.Response, error) {
		version := "1"
		if strings.Contains(r.URL.Path, "/"+strings.Repeat("2", 40)+"/") {
			version = "2"
		}
		body := []byte("---\nname: proof\ndescription: 证据\n---\n版本" + version)
		var response []byte
		if r.URL.Host == "api.github.com" {
			response = []byte(`{"sha":"` + strings.Repeat("1", 40) + `"}`)
		} else if strings.HasSuffix(r.URL.Path, "catalog.json") {
			response, _ = json.Marshal(catalog.Manifest{SchemaVersion: 1, Items: []catalog.Item{{Kind: "skill", ID: "proof", Name: "证据", Version: version, Files: map[string]string{"SKILL.md": catalog.Digest(body)}}}})
		} else {
			response = body
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(response))}, nil
	})
}

func previewCatalogSkill(t *testing.T, s *Server, version string) catalogPreview {
	t.Helper()
	w := catalogJSONRequest(t, s.catalogPreview, catalogRequest{Kind: "skill", ID: "proof", Commit: strings.Repeat(version, 40)})
	if w.Code != 200 {
		t.Fatalf("预览失败：%d %s", w.Code, w.Body)
	}
	var p catalogPreview
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func installCatalogPreview(t *testing.T, s *Server, p catalogPreview) *httptest.ResponseRecorder {
	t.Helper()
	return catalogJSONRequest(t, s.catalogInstall, catalogRequest{Kind: p.Item.Kind, ID: p.Item.ID, Commit: p.Commit, ExpectedLocalHash: p.LocalHash, ExpectedSourceHash: p.SourceHash})
}

func TestCatalogHTTPConflictUpdateRollbackAndLoaderIsolation(t *testing.T) {
	s := catalogTestServer(t)
	catalogSkillFixture(t)
	p := previewCatalogSkill(t, s, "1")
	if p.LocalHash != "absent" || p.Conflict || !strings.Contains(p.Contents["SKILL.md"], "版本1") {
		t.Fatalf("首次预览无效：%+v", p)
	}
	if w := installCatalogPreview(t, s, p); w.Code != 200 {
		t.Fatalf("首次安装：%d %s", w.Code, w.Body)
	}
	root := filepath.Join(s.skillDir, "proof")
	first, err := os.ReadFile(filepath.Join(root, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	second := previewCatalogSkill(t, s, "2")
	// 既有技能编辑接口与目录安装使用同一锁；预览后的修改不能被静默覆盖。
	r := httptest.NewRequest(http.MethodPut, "/api/skills/proof/files/SKILL.md", strings.NewReader(`{"content":"用户修改"}`))
	r.SetPathValue("name", "proof")
	r.SetPathValue("file", "SKILL.md")
	w := httptest.NewRecorder()
	s.fsWriteFile(w, r)
	if w.Code != 200 {
		t.Fatalf("本地编辑：%d %s", w.Code, w.Body)
	}
	if w = installCatalogPreview(t, s, second); w.Code != 409 {
		t.Fatalf("过期预览应409：%d %s", w.Code, w.Body)
	}
	if body, _ := os.ReadFile(filepath.Join(root, "SKILL.md")); string(body) != "用户修改" {
		t.Fatal("本地修改被覆盖")
	}
	conflict := previewCatalogSkill(t, s, "2")
	if !conflict.Conflict || conflict.LocalContents["SKILL.md"] != "用户修改" {
		t.Fatal("冲突预览缺少本地正文")
	}
	if err = os.WriteFile(filepath.Join(root, "SKILL.md"), first, 0644); err != nil {
		t.Fatal(err)
	}
	second = previewCatalogSkill(t, s, "2")
	if w = installCatalogPreview(t, s, second); w.Code != 200 {
		t.Fatalf("更新失败：%d %s", w.Code, w.Body)
	}
	reg, err := skill.LoadDir(s.skillDir)
	if err != nil || len(reg.List()) != 1 || reg.List()[0].Dir != root {
		t.Fatalf("暂存/备份被加载成技能：%+v %v", reg.List(), err)
	}
	rec, err := loadCatalogRecord(s.m.pg, "skill", "proof")
	if err != nil || rec.Previous == nil {
		t.Fatalf("缺少回滚记录：%+v %v", rec, err)
	}
	w = catalogJSONRequest(t, s.catalogRollback, catalogRequest{Kind: "skill", ID: "proof", ExpectedLocalHash: rec.Hash})
	if w.Code != 200 {
		t.Fatalf("回滚：%d %s", w.Code, w.Body)
	}
	if body, _ := os.ReadFile(filepath.Join(root, "SKILL.md")); !bytes.Equal(body, first) {
		t.Fatal("回滚未恢复原内容")
	}
}

func TestCatalogHTTPSkillMetadataFailureAndSourceChange(t *testing.T) {
	s := catalogTestServer(t)
	catalogSkillFixture(t)
	p := previewCatalogSkill(t, s, "1")
	if w := installCatalogPreview(t, s, p); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	p = previewCatalogSkill(t, s, "2")
	if _, err := s.m.pg.Exec(`CREATE FUNCTION fail_catalog_metadata() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='catalog.installed.skill.proof' THEN RAISE EXCEPTION '模拟安装记录失败'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_catalog_metadata BEFORE INSERT OR UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION fail_catalog_metadata()`); err != nil {
		t.Fatal(err)
	}
	w := installCatalogPreview(t, s, p)
	if w.Code != 409 {
		t.Fatalf("元数据失败应409：%d %s", w.Code, w.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(s.skillDir, "proof", "SKILL.md")); !strings.Contains(string(b), "版本1") {
		t.Fatal("安装记录失败未恢复旧技能")
	}
	if _, err := s.m.pg.Exec(`DROP TRIGGER fail_catalog_metadata ON settings`); err != nil {
		t.Fatal(err)
	}
	source := catalog.DefaultSource()
	source.Ref = "main"
	b, _ := json.Marshal(source)
	if err := s.m.pg.CatalogSetSource(context.Background(), string(b)); err != nil {
		t.Fatal(err)
	}
	w = installCatalogPreview(t, s, p)
	if w.Code != 409 {
		t.Fatalf("切换源后安装应拒绝：%d %s", w.Code, w.Body)
	}
}

func TestCatalogSkillMutationsShareInstallationLock(t *testing.T) {
	s := &Server{skillDir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(s.skillDir, "proof"), 0755); err != nil {
		t.Fatal(err)
	}
	skillMutationMu.Lock()
	locked := true
	defer func() {
		if locked {
			skillMutationMu.Unlock()
		}
	}()
	done := make(chan int, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"content":"编辑"}`))
		r.SetPathValue("name", "proof")
		r.SetPathValue("file", "SKILL.md")
		w := httptest.NewRecorder()
		s.fsWriteFile(w, r)
		done <- w.Code
	}()
	select {
	case <-done:
		t.Fatal("编辑绕过了安装锁")
	case <-time.After(30 * time.Millisecond):
	}
	skillMutationMu.Unlock()
	locked = false
	select {
	case status := <-done:
		if status != 200 {
			t.Fatalf("编辑失败:%d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("安装锁释放后编辑未恢复")
	}
}

func TestCatalogDownloadOnlyContainsVerifiedRepositoryFiles(t *testing.T) {
	s := catalogTestServer(t)
	catalogSkillFixture(t)
	p := previewCatalogSkill(t, s, "1")
	if w := installCatalogPreview(t, s, p); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err := os.WriteFile(filepath.Join(s.skillDir, "proof", "private.txt"), []byte("实例私有数据"), 0600); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/catalog/download?kind=skill&id=proof&commit="+p.Commit, nil)
	w := httptest.NewRecorder()
	s.catalogDownload(w, r)
	if w.Code != 200 {
		t.Fatalf("远程包下载：%d %s", w.Code, w.Body)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "proof/SKILL.md" {
		t.Fatalf("下载混入本地文件：%+v", zr.File)
	}
	if w.Header().Get("X-Catalog-Commit") != p.Commit {
		t.Fatal("下载未携带精确提交")
	}
}

func TestCatalogPayloadUsesExistingSkillNameRules(t *testing.T) {
	for _, name := range []string{"proof_v1", "proof--v1", "proof-", "Proof", strings.Repeat("a", 65)} {
		t.Run(name, func(t *testing.T) {
			files := map[string][]byte{"SKILL.md": []byte("---\nname: " + name + "\n---\n说明")}
			if _, err := validateCatalogPayload(catalog.Item{Kind: "skill", ID: name}, files); err == nil {
				t.Fatal("目录允许安装现有技能接口无法管理的名称")
			}
			agent := catalog.Item{Kind: "agent", ID: "proof_agent", Skills: []string{name}}
			if _, err := validateCatalogPayload(agent, map[string][]byte{"agent.json": []byte(`{"key":"proof_agent","name":"证据助手","prompt":"整理证据"}`)}); err == nil {
				t.Fatal("Agent允许绑定不符合现有名称规范的技能依赖")
			}
		})
	}
	files := map[string][]byte{"SKILL.md": []byte("---\nname: proof-v1\n---\n说明")}
	if _, err := validateCatalogPayload(catalog.Item{Kind: "skill", ID: "proof-v1", Skills: []string{"other-proof"}}, files); err != nil {
		t.Fatalf("合法技能名称被拒绝：%v", err)
	}
}

func TestCatalogPlatformSkillMutationsShareInstallationLock(t *testing.T) {
	for _, create := range []bool{true, false} {
		t.Run(map[bool]string{true: "create_skill", false: "update_skill_file"}[create], func(t *testing.T) {
			s := &Server{skillDir: t.TempDir()}
			root := filepath.Join(s.skillDir, "proof")
			if !create {
				if err := os.Mkdir(root, 0755); err != nil {
					t.Fatal(err)
				}
			}
			skillMutationMu.Lock()
			locked := true
			defer func() {
				if locked {
					skillMutationMu.Unlock()
				}
			}()
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				tool := s.toolUpdateSkillFile()
				in := json.RawMessage(`{"name":"proof","content":"Agent修改"}`)
				if create {
					tool = s.toolCreateSkill()
					in = json.RawMessage(`{"name":"proof","description":"Agent创建"}`)
				}
				close(started)
				result, err := tool.Call(context.Background(), in, nil)
				if err == nil && !result.IsError {
					err = fmt.Errorf("Agent写入未在锁内重新判断技能存在性")
				}
				done <- err
			}()
			<-started
			select {
			case <-done:
				t.Fatal("Agent技能写入绕过了目录安装锁")
			case <-time.After(30 * time.Millisecond):
			}
			// 模拟安装在持锁期间改变目录，工具必须在获得锁后复核存在性。
			if create {
				if err := os.Mkdir(root, 0755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Rename(root, root+"-old"); err != nil {
				t.Fatal(err)
			}
			skillMutationMu.Unlock()
			locked = false
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("安装锁释放后Agent工具未恢复")
			}
			if _, err := os.Stat(filepath.Join(root, "SKILL.md")); !os.IsNotExist(err) {
				t.Fatalf("存在性变化后仍写入了技能：%v", err)
			}
		})
	}
}

func TestCatalogHTTPAgentSkillWriteInvalidatesPreview(t *testing.T) {
	s := catalogTestServer(t)
	catalogSkillFixture(t)
	if w := installCatalogPreview(t, s, previewCatalogSkill(t, s, "1")); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	p := previewCatalogSkill(t, s, "2")
	result, err := s.toolUpdateSkillFile().Call(context.Background(), json.RawMessage(`{"name":"proof","content":"Agent最新修改"}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("Agent写入失败：%v %+v", err, result)
	}
	if w := installCatalogPreview(t, s, p); w.Code != 409 {
		t.Fatalf("Agent写入后过期安装应409：%d %s", w.Code, w.Body)
	}
	if b, err := os.ReadFile(filepath.Join(s.skillDir, "proof", "SKILL.md")); err != nil || string(b) != "Agent最新修改" {
		t.Fatalf("Agent最新写入被覆盖：%v %q", err, b)
	}
}
