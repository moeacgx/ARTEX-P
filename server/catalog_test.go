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
