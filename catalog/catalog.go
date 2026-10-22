// Package catalog 实现只读 GitHub 目录、内容校验和安全文件替换。
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxFileBytes = 4 << 20
const MaxPackageBytes = 20 << 20

type Source struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	Directory  string `json:"directory"`
}

func DefaultSource() Source { return Source{"moeacgx/ARTEX-P", "dev", "catalog"} }

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func SafePath(p string) bool {
	if p == "" || len(p) > 240 || !utf8.ValidString(p) || strings.ContainsAny(p, "\\:\x00?#%") || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.TrimSpace(part) != part {
			return false
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
			return false
		}
	}
	return true
}

func (s Source) Validate() error {
	if !repoPattern.MatchString(s.Repository) {
		return fmt.Errorf("仓库必须为 GitHub owner/repository")
	}
	if strings.TrimSpace(s.Ref) == "" || len(s.Ref) > 200 || strings.ContainsAny(s.Ref, "\x00\r\n") {
		return fmt.Errorf("仓库分支或提交无效")
	}
	if !SafePath(s.Directory) {
		return fmt.Errorf("目录路径无效")
	}
	return nil
}

type Item struct {
	Kind        string            `json:"kind"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Version     string            `json:"version"`
	License     string            `json:"license"`
	Files       map[string]string `json:"files"`
	Tools       []string          `json:"tools,omitempty"`
	Skills      []string          `json:"skills,omitempty"`
}

type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	Items         []Item `json:"items"`
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != 1 || len(m.Items) > 500 {
		return fmt.Errorf("目录格式版本无效或条目过多")
	}
	seen := map[string]bool{}
	for _, i := range m.Items {
		if (i.Kind != "skill" && i.Kind != "tool" && i.Kind != "agent") || !idPattern.MatchString(i.ID) || strings.TrimSpace(i.Name) == "" || i.Version == "" || len(i.Files) == 0 || len(i.Files) > 200 {
			return fmt.Errorf("目录条目无效：%s/%s", i.Kind, i.ID)
		}
		key := i.Kind + "/" + i.ID
		if seen[key] {
			return fmt.Errorf("重复条目：%s", key)
		}
		seen[key] = true
		fileNames := map[string]bool{}
		for f, h := range i.Files {
			if !SafePath(f) || !digestPattern.MatchString(h) {
				return fmt.Errorf("文件路径或SHA256无效：%s", f)
			}
			folded := strings.ToLower(f)
			if fileNames[folded] {
				return fmt.Errorf("文件路径存在大小写冲突：%s", f)
			}
			fileNames[folded] = true
		}
		if i.Kind == "skill" {
			if _, ok := i.Files["SKILL.md"]; !ok {
				return fmt.Errorf("技能缺少 SKILL.md")
			}
		} else {
			if len(i.Files) != 1 {
				return fmt.Errorf("工具和Agent包只能包含一个JSON模板")
			}
			if _, ok := i.Files[i.Kind+".json"]; !ok {
				return fmt.Errorf("缺少 %s.json", i.Kind)
			}
		}
		for _, d := range append(append([]string{}, i.Tools...), i.Skills...) {
			if !idPattern.MatchString(d) {
				return fmt.Errorf("依赖名称无效")
			}
		}
	}
	return nil
}

type Client struct{ HTTP *http.Client }

func NewClient() *Client {
	// 不跟随重定向到任意主机，所有请求只发往固定 GitHub 域名。
	return &Client{HTTP: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return fmt.Errorf("仓库请求不允许重定向")
	}}}
}

func (c *Client) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ARTEX-P-Catalog")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("读取仓库失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("读取仓库失败（HTTP %d）", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("仓库文件超过大小限制")
	}
	return b, nil
}

func (c *Client) Resolve(ctx context.Context, s Source, commit string) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	if commit != "" {
		if !shaPattern.MatchString(commit) {
			return "", fmt.Errorf("必须指定完整提交SHA")
		}
		return commit, nil
	}
	b, err := c.get(ctx, "https://api.github.com/repos/"+s.Repository+"/commits/"+url.PathEscape(s.Ref), 1<<20)
	if err != nil {
		return "", err
	}
	var v struct {
		SHA string `json:"sha"`
	}
	if err = json.Unmarshal(b, &v); err != nil || !shaPattern.MatchString(v.SHA) {
		return "", fmt.Errorf("仓库未返回有效提交SHA")
	}
	return v.SHA, nil
}

func (c *Client) Manifest(ctx context.Context, s Source, commit string) (Manifest, string, error) {
	sha, err := c.Resolve(ctx, s, commit)
	if err != nil {
		return Manifest{}, "", err
	}
	b, err := c.get(ctx, "https://raw.githubusercontent.com/"+s.Repository+"/"+sha+"/"+s.Directory+"/catalog.json", 1<<20)
	if err != nil {
		return Manifest{}, "", err
	}
	var m Manifest
	if err = json.Unmarshal(b, &m); err != nil {
		return m, "", fmt.Errorf("目录清单JSON无效")
	}
	if err = m.Validate(); err != nil {
		return m, "", err
	}
	return m, sha, nil
}

func (c *Client) Files(ctx context.Context, s Source, commit string, i Item) (map[string][]byte, error) {
	if !shaPattern.MatchString(commit) {
		return nil, fmt.Errorf("提交SHA无效")
	}
	out := map[string][]byte{}
	total := 0
	for _, f := range SortedFiles(i.Files) {
		b, err := c.get(ctx, "https://raw.githubusercontent.com/"+s.Repository+"/"+commit+"/"+s.Directory+"/"+i.Kind+"s/"+i.ID+"/"+f, MaxFileBytes)
		if err != nil {
			return nil, err
		}
		total += len(b)
		if total > MaxPackageBytes {
			return nil, fmt.Errorf("包超过大小限制")
		}
		if Digest(b) != i.Files[f] {
			return nil, fmt.Errorf("文件SHA256不匹配：%s", f)
		}
		out[f] = b
	}
	return out, nil
}

func Digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func Canonical(b []byte) []byte {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	out, _ := json.Marshal(v)
	return out
}
func SortedFiles[T any](files map[string]T) []string {
	keys := make([]string, 0, len(files))
	for f := range files {
		keys = append(keys, f)
	}
	sort.Strings(keys)
	return keys
}
func FileHash(files map[string][]byte) string {
	hashes := map[string]string{}
	for f, b := range files {
		hashes[f] = Digest(b)
	}
	b, _ := json.Marshal(hashes)
	return Digest(b)
}

// ReadTree 拒绝符号链接，不读取目录外文件，并限制本地冲突检查成本。
func ReadTree(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	total := int64(0)
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("技能目录含符号链接")
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
			return fmt.Errorf("技能文件类型或大小不受支持")
		}
		total += info.Size()
		if total > MaxPackageBytes || len(files) >= 200 {
			return fmt.Errorf("技能包超出目录更新限制")
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if !SafePath(filepath.ToSlash(rel)) {
			return fmt.Errorf("技能文件路径无效")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	return files, err
}

// ReplaceTree 先完整暂存再换名；持久化元数据失败时恢复旧目录。
// backup 在成功后保留，用于显式回滚；调用方负责保存其路径。
func ReplaceTree(target string, files map[string][]byte, persist func(backup string) error) (err error) {
	parent := filepath.Dir(target)
	if err = os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	stateDir, err := StateDir(parent)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(stateDir, "stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for f, b := range files {
		if !SafePath(f) {
			return fmt.Errorf("非法文件路径")
		}
		dst := filepath.Join(stage, filepath.FromSlash(f))
		if err = os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err = os.WriteFile(dst, b, 0644); err != nil {
			return err
		}
	}
	backup := ""
	if info, e := os.Lstat(target); e == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("目标不是普通目录")
		}
		backup, err = os.MkdirTemp(stateDir, "backup-")
		if err != nil {
			return err
		}
		if err = os.Remove(backup); err != nil {
			return err
		}
		if err = os.Rename(target, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if err = os.Rename(stage, target); err != nil {
		if backup != "" {
			if e := os.Rename(backup, target); e != nil {
				return fmt.Errorf("安装失败：%v；恢复失败：%w", err, e)
			}
		}
		return err
	}
	if err = persist(backup); err != nil {
		if e := os.RemoveAll(target); e != nil {
			return fmt.Errorf("元数据保存失败：%v；回滚移除失败：%w", err, e)
		}
		if backup != "" {
			if e := os.Rename(backup, target); e != nil {
				return fmt.Errorf("元数据保存失败：%v；恢复失败：%w", err, e)
			}
		}
		return err
	}
	return nil
}

// StateDir 将事务目录隔离一层，技能加载器不会把暂存/备份作为技能读取。
func StateDir(skillDir string) (string, error) {
	dir := filepath.Join(skillDir, ".catalog-state")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("技能事务目录不是普通目录")
	}
	return dir, nil
}
