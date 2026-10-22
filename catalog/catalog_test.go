package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type catalogRoundTrip func(*http.Request) (*http.Response, error)

func (f catalogRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCatalogFrozenCommitAndHashValidation(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	body := []byte("---\nname: sample\ndescription: 示例\n---\n内容")
	i := Item{Kind: "skill", ID: "sample", Name: "示例", Version: "1", Files: map[string]string{"SKILL.md": Digest(body)}}
	m, _ := json.Marshal(Manifest{1, []Item{i}})
	c := NewClient()
	requests := []string{}
	c.HTTP.Transport = catalogRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.String())
		var b []byte
		switch {
		case r.URL.Host == "api.github.com":
			b = []byte(`{"sha":"` + sha + `"}`)
		case strings.HasSuffix(r.URL.Path, "catalog.json"):
			b = m
		default:
			if !strings.Contains(r.URL.Path, "/"+sha+"/") {
				t.Fatalf("未固定提交：%s", r.URL)
			}
			b = body
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})
	mf, commit, err := c.Manifest(t.Context(), DefaultSource(), "")
	if err != nil || commit != sha {
		t.Fatalf("读取清单: %s %v", commit, err)
	}
	files, err := c.Files(t.Context(), DefaultSource(), commit, mf.Items[0])
	if err != nil || string(files["SKILL.md"]) != string(body) {
		t.Fatalf("下载校验: %v", err)
	}
	i.Files["SKILL.md"] = strings.Repeat("0", 64)
	if _, err = c.Files(context.Background(), DefaultSource(), commit, i); err == nil {
		t.Fatal("损坏文件未被拒绝")
	}
	if len(requests) != 4 {
		t.Fatalf("请求次数=%d", len(requests))
	}
}

func TestCatalogRejectsPathsAndManifestAmbiguity(t *testing.T) {
	for _, p := range []string{"../x", "/x", "a/../../x", `a\b`, ".git/config", "a?token=x", "a%2fb", "a#x", "a//b", "CON.txt", "a.", "a\nb", "a\u202eb"} {
		if SafePath(p) {
			t.Errorf("非法路径通过：%q", p)
		}
	}
	for _, repo := range []string{"https://example.com/a", "a/b/c", "../x", "a/b?x=y"} {
		s := DefaultSource()
		s.Repository = repo
		if s.Validate() == nil {
			t.Errorf("非法仓库通过：%q", repo)
		}
	}
	i := Item{Kind: "skill", ID: "sample", Name: "示例", Version: "1", Files: map[string]string{"SKILL.md": strings.Repeat("a", 64)}}
	if err := (Manifest{1, []Item{i, i}}).Validate(); err == nil {
		t.Fatal("重复条目通过")
	}
	i.Files["../escape"] = strings.Repeat("b", 64)
	if err := (Manifest{1, []Item{i}}).Validate(); err == nil {
		t.Fatal("清单穿越路径通过")
	}
}

func TestCatalogReplaceTreeRestoresOnPersistenceFailure(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "sample")
	old := map[string][]byte{"SKILL.md": []byte("旧技能"), "references/proof.txt": []byte("证据")}
	if err := ReplaceTree(target, old, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("模拟元数据写入失败")
	err := ReplaceTree(target, map[string][]byte{"SKILL.md": []byte("新技能")}, func(backup string) error {
		if filepath.Dir(backup) != filepath.Join(parent, ".catalog-state") {
			t.Fatalf("备份未隔离：%s", backup)
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("错误=%v", err)
	}
	got, err := ReadTree(target)
	if err != nil || FileHash(got) != FileHash(old) {
		t.Fatalf("旧技能未完整恢复：%v %v", got, err)
	}
	if err = ReplaceTree(filepath.Join(parent, "new"), old, func(string) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(parent, "new")); !os.IsNotExist(err) {
		t.Fatal("失败的新安装应撤销")
	}
}

func TestCatalogDownloadSizeLimit(t *testing.T) {
	c := NewClient()
	c.HTTP.Transport = catalogRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", MaxFileBytes+1)))}, nil
	})
	i := Item{Kind: "skill", ID: "sample", Files: map[string]string{"SKILL.md": strings.Repeat("a", 64)}}
	if _, err := c.Files(t.Context(), DefaultSource(), strings.Repeat("a", 40), i); err == nil {
		t.Fatal("超限文件未被拒绝")
	}
}
