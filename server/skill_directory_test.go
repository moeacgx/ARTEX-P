package server

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadSkillPreservesEmptyDirectories(t *testing.T) {
	dir := t.TempDir()
	data := buildZip(t,
		zipFile{name: "demo/SKILL.md", body: "---\nname: demo\n---\n", method: zip.Deflate},
		zipFile{name: "demo/参考/空目录/", method: zip.Store},
	)
	rr, _ := uploadZip(t, dir, "demo.zip", data)
	if rr.Code != 201 {
		t.Fatalf("上传失败：%d %s", rr.Code, rr.Body)
	}
	info, err := os.Stat(filepath.Join(dir, "demo", "参考", "空目录"))
	if err != nil || !info.IsDir() {
		t.Fatalf("空目录没有保留：%v", err)
	}
}

func TestDownloadSkillRejectsDirectoryPathTooLongForUpload(t *testing.T) {
	s, root := downloadSkillFixture(t, "long-path")
	// 目录条目末尾的斜杠也占用上传接口的路径长度额度。
	rel := strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 110)
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	assertDownloadRejected(t, downloadSkillRequest(s, "long-path", context.Background()), 400)
}

func TestUploadSkillDirectoryCannotBeManifest(t *testing.T) {
	data := buildZip(t, zipFile{name: "demo/SKILL.md/", method: zip.Store})
	rr, _ := uploadZip(t, t.TempDir(), "demo.zip", data)
	if rr.Code != 400 {
		t.Fatalf("目录不能充当 SKILL.md：%d %s", rr.Code, rr.Body)
	}
}
