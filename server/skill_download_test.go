package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func downloadSkillFixture(t *testing.T, name string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, name)
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: 下载测试\n---\n正文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Server{skillDir: dir, jwtKey: []byte("skill-download-test-signing-key")}, root
}

func downloadSkillRequest(s *Server, name string, ctx context.Context) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/skills/"+url.PathEscape(name)+"/download", nil).WithContext(ctx)
	r.SetPathValue("name", name)
	w := httptest.NewRecorder()
	s.fsDownloadSkill(w, r)
	return w
}

func assertDownloadRejected(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("状态 = %d，期望 %d；响应 = %s", w.Code, status, w.Body)
	}
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/zip") || bytes.HasPrefix(w.Body.Bytes(), []byte("PK\x03\x04")) {
		t.Fatal("失败响应不应发送 ZIP 或部分压缩包")
	}
}

func TestDownloadSkillArchiveAndUploadRoundTrip(t *testing.T) {
	const name = "中文技能"
	s, root := downloadSkillFixture(t, name)
	files := map[string][]byte{
		"参考/说明 文档.md":       []byte("中文参考内容\n"),
		"assets/binary.bin": {0, 1, 255, 13, 10, 0},
		"scripts/run.sh":    []byte("#!/bin/sh\nprintf 'ok\\n'\n"),
	}
	for rel, body := range files {
		file := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if rel == "scripts/run.sh" {
			mode = 0o755
		}
		if err := os.WriteFile(file, body, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "空目录", "子目录"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := downloadSkillRequest(s, name, context.Background())
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", w.Code, w.Body)
	}
	if w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("错误的下载类型：%q", w.Header().Get("Content-Type"))
	}
	disposition, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || params["filename"] != name+".zip" {
		t.Fatalf("下载文件名无效：%q (%v)", w.Header().Get("Content-Disposition"), err)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]*zip.File{}
	for _, entry := range zr.File {
		if !strings.HasPrefix(entry.Name, name+"/") || strings.Contains(entry.Name, "\\") {
			t.Fatalf("压缩包条目应位于技能顶层目录：%q", entry.Name)
		}
		entries[entry.Name] = entry
	}
	md, err := os.ReadFile(filepath.Join(root, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	files["SKILL.md"] = md
	for rel, want := range files {
		entry := entries[name+"/"+rel]
		if entry == nil {
			t.Fatalf("压缩包缺少 %q", rel)
		}
		rc, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("压缩包内容不一致：%q (%v)", rel, err)
		}
		if rel == "scripts/run.sh" && runtime.GOOS != "windows" && entry.Mode().Perm()&0o111 == 0 {
			t.Fatal("下载丢失脚本可执行权限")
		}
	}
	if entry := entries[name+"/空目录/子目录/"]; entry == nil || !entry.FileInfo().IsDir() {
		t.Fatal("下载丢失嵌套空目录")
	}
	installed := t.TempDir()
	upload, _ := uploadZip(t, installed, name+".zip", w.Body.Bytes())
	if upload.Code != http.StatusCreated {
		t.Fatalf("重新导入失败：%d %s", upload.Code, upload.Body)
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(installed, name, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("重新导入后内容不一致：%q (%v)", rel, err)
		}
	}
	if info, err := os.Stat(filepath.Join(installed, name, "空目录", "子目录")); err != nil || !info.IsDir() {
		t.Fatalf("重新导入应保留空目录：%v", err)
	}
}

func TestDownloadSkillRouteRequiresLogin(t *testing.T) {
	s, _ := downloadSkillFixture(t, "route-skill")
	h := s.Handler()
	for _, loggedIn := range []bool{false, true} {
		t.Run(fmt.Sprint(loggedIn), func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/skills/route-skill/download", nil)
			want := http.StatusUnauthorized
			if loggedIn {
				token, err := signJWT(s.jwtKey)
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Authorization", "Bearer "+token)
				want = http.StatusOK
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("状态 = %d，期望 %d；响应 = %s", w.Code, want, w.Body)
			}
		})
	}
}

func TestDownloadSkillInvalidNamesAndMissing(t *testing.T) {
	s, _ := downloadSkillFixture(t, "valid-skill")
	for _, name := range []string{"", "..", "../valid-skill", "a/b", `a\b`, "has space", "Upper"} {
		t.Run(name, func(t *testing.T) {
			assertDownloadRejected(t, downloadSkillRequest(s, name, context.Background()), http.StatusBadRequest)
		})
	}
	assertDownloadRejected(t, downloadSkillRequest(s, "missing", context.Background()), http.StatusNotFound)
	if err := os.Mkdir(filepath.Join(s.skillDir, "no-manifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	assertDownloadRejected(t, downloadSkillRequest(s, "no-manifest", context.Background()), http.StatusBadRequest)
	if err := os.Mkdir(filepath.Join(s.skillDir, "no-manifest", "SKILL.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	assertDownloadRejected(t, downloadSkillRequest(s, "no-manifest", context.Background()), http.StatusBadRequest)
}

func TestDownloadSkillRejectsSymlinks(t *testing.T) {
	for _, kind := range []string{"root", "file", "directory", "manifest"} {
		t.Run(kind, func(t *testing.T) {
			s, root := downloadSkillFixture(t, "safe-skill")
			external := t.TempDir()
			secret := filepath.Join(external, "secret.txt")
			if err := os.WriteFile(secret, []byte("不得导出目录外的内容"), 0o644); err != nil {
				t.Fatal(err)
			}
			var target, link string
			switch kind {
			case "root":
				target, link = root, filepath.Join(s.skillDir, "linked-skill")
			case "file":
				target, link = secret, filepath.Join(root, "secret.txt")
			case "directory":
				target, link = external, filepath.Join(root, "external")
			case "manifest":
				if err := os.Remove(filepath.Join(root, "SKILL.md")); err != nil {
					t.Fatal(err)
				}
				target, link = secret, filepath.Join(root, "SKILL.md")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("当前环境不允许创建符号链接：%v", err)
			}
			name := "safe-skill"
			if kind == "root" {
				name = "linked-skill"
			}
			assertDownloadRejected(t, downloadSkillRequest(s, name, context.Background()), http.StatusBadRequest)
		})
	}
}

func TestDownloadSkillRejectsUnsafeEntryPath(t *testing.T) {
	s, root := downloadSkillFixture(t, "path-skill")
	// 文件系统可容纳的名称也必须符合技能导入约束，避免生成无法重导入的包。
	if err := os.WriteFile(filepath.Join(root, "bad#name.md"), []byte("不可导出"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertDownloadRejected(t, downloadSkillRequest(s, "path-skill", context.Background()), http.StatusBadRequest)
}

func TestDownloadSkillLimits(t *testing.T) {
	for _, kind := range []string{"file", "total", "entries", "compressed"} {
		t.Run(kind, func(t *testing.T) {
			s, root := downloadSkillFixture(t, "limited-skill")
			// 稀疏文件只用于大小预检，避免为拒绝场景实际写入上百 MB。
			createSized := func(name string, size int64) {
				t.Helper()
				f, err := os.Create(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				err = f.Truncate(size)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "file":
				createSized("oversize.bin", maxSkillFileBytes+1)
			case "total":
				for i := range 5 {
					createSized(fmt.Sprintf("part-%d.bin", i), maxSkillFileBytes)
				}
				createSized("extra.bin", 1)
			case "entries":
				for i := 0; i < maxSkillEntries; i++ {
					createSized(fmt.Sprintf("entry-%04d", i), 0)
				}
			case "compressed":
				f, err := os.Create(filepath.Join(root, "random.bin"))
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.CopyN(f, rand.Reader, maxSkillFileBytes)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			assertDownloadRejected(t, downloadSkillRequest(s, "limited-skill", context.Background()), http.StatusRequestEntityTooLarge)
		})
	}
}

func TestDownloadSkillCancelled(t *testing.T) {
	s, _ := downloadSkillFixture(t, "cancel-skill")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertDownloadRejected(t, downloadSkillRequest(s, "cancel-skill", ctx), http.StatusRequestTimeout)
}
