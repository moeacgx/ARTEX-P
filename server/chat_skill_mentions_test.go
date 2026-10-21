package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

type skillMentionTestPage struct {
	Items []struct {
		Kind        string `json:"kind"`
		ID          string `json:"id"`
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func writeChatSkillFixture(t *testing.T, root, directory, manifest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, directory, "SKILL.md"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func searchChatSkillFixture(s *Server, query, cursor string) *httptest.ResponseRecorder {
	values := url.Values{"kind": {"skill"}, "q": {query}, "cursor": {cursor}}
	w := httptest.NewRecorder()
	s.searchChatMentions(w, httptest.NewRequest(http.MethodGet, "/api/chat/mentions?"+values.Encode(), nil))
	return w
}

func decodeChatSkillPage(t *testing.T, w *httptest.ResponseRecorder) skillMentionTestPage {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("技能搜索失败：%d %s", w.Code, w.Body)
	}
	var page skillMentionTestPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("技能搜索应返回字符串 ID：%v；响应：%s", err, w.Body)
	}
	return page
}

func TestChatSkillMentionParsing(t *testing.T) {
	refs, err := parseChatMentions("请使用 @[技能#fofa-search] 和 @[技能#中文技能] 检查 @[漏洞#12]，再次使用 @[技能#fofa-search]")
	if err != nil || len(refs) != 3 || refs[0].Kind != "skill" || refs[1].Kind != "skill" || refs[2].Kind != "finding" {
		t.Fatalf("技能与数字引用未正确解析、去重：%+v %v", refs, err)
	}
	if refs[0].SkillName != "fofa-search" || refs[1].SkillName != "中文技能" || refs[2].ID != 12 {
		t.Fatalf("引用的目录名或数字 ID 不正确：%+v", refs)
	}
}

func TestChatSkillMentionSearchWithoutDatabase(t *testing.T) {
	dir := t.TempDir()
	writeChatSkillFixture(t, dir, "中文技能", "---\nname: actual-skill-name\ndescription: |\n  中文搜索描述\n  第二行引用说明\n---\n正文不能出现在搜索结果\n")
	s := &Server{skillDir: dir}
	page := decodeChatSkillPage(t, searchChatSkillFixture(s, "", ""))
	if len(page.Items) != 1 || page.Items[0].Kind != "skill" || page.Items[0].ID != "中文技能" || page.Items[0].Label != "中文技能" || page.Items[0].Description != "中文搜索描述\n第二行引用说明" {
		t.Fatalf("技能目录标识或真实多行描述不正确：%+v", page)
	}
}

func TestChatSkillMentionParsingLimitAndInvalidNames(t *testing.T) {
	var message strings.Builder
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&message, "@[技能#skill-%d] ", i)
	}
	for i := 1; i <= 4; i++ {
		fmt.Fprintf(&message, "@[资产#%d] ", i)
	}
	message.WriteString("@[技能#skill-0] @[资产#1 重复]")
	if refs, err := parseChatMentions(message.String()); err != nil || len(refs) != 10 {
		t.Fatalf("10 个混合引用和重复引用应被接受：%+v %v", refs, err)
	}
	if _, err := parseChatMentions(message.String() + " @[技能#第十一个技能]"); err == nil {
		t.Fatal("技能与记录应共享 10 条上限")
	}
	for _, name := range []string{"", "..", "../outside", "a/b", `a\b`, "Upper", "has space", "%2e%2e", "a?b", "a#b", strings.Repeat("a", 65)} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseChatMentions("@[技能#" + name + "]"); err == nil {
				t.Fatalf("不能静默忽略非法技能路径：%q", name)
			}
		})
	}
}

func TestChatSkillMentionSearchFields(t *testing.T) {
	dir := t.TempDir()
	writeChatSkillFixture(t, dir, "目录技能", "---\nname: actual-lookup\ndescription: |\n  MixedCase Metadata\n  第二行中文说明\n---\n秘密正文搜索词\n")
	writeChatSkillFixture(t, dir, "unrelated", "---\nname: unrelated\ndescription: 其它技能\n---\n正文\n")
	s := &Server{skillDir: dir}
	for _, query := range []string{"目录技", "ACTUAL-LOOKUP", "mixedcase", "第二行中文"} {
		t.Run(query, func(t *testing.T) {
			page := decodeChatSkillPage(t, searchChatSkillFixture(s, query, ""))
			if len(page.Items) != 1 || page.Items[0].ID != "目录技能" {
				t.Fatalf("未按目录、真实名称或描述搜索：%+v", page)
			}
		})
	}
	for _, query := range []string{"不存在", "秘密正文搜索词", "%", "_"} {
		page := decodeChatSkillPage(t, searchChatSkillFixture(s, query, ""))
		if len(page.Items) != 0 {
			t.Fatalf("不应搜索正文或将通配符作为全量匹配：%q %+v", query, page)
		}
	}
}

func TestChatSkillMentionSearchPagination(t *testing.T) {
	dir := t.TempDir()
	var expected []string
	for i := 42; i >= 0; i-- {
		name := fmt.Sprintf("skill-%02d", i)
		expected = append(expected, name)
		writeChatSkillFixture(t, dir, name, "---\nname: "+name+"\ndescription: 分页技能\n---\n正文\n")
	}
	sort.Strings(expected)
	s := &Server{skillDir: dir}
	var got []string
	cursor := ""
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > 2 {
			t.Fatal("43 条技能分页未在第三页结束")
		}
		page := decodeChatSkillPage(t, searchChatSkillFixture(s, "分页", cursor))
		wantCount := 20
		if pageNumber == 2 {
			wantCount = 3
		}
		if len(page.Items) != wantCount {
			t.Fatalf("第 %d 页条数为 %d，期望 %d", pageNumber, len(page.Items), wantCount)
		}
		for _, item := range page.Items {
			got = append(got, item.ID)
		}
		if pageNumber == 0 {
			if page.NextCursor == "" {
				t.Fatal("缺少下一页游标")
			}
			if w := searchChatSkillFixture(s, "changed", page.NextCursor); w.Code != http.StatusBadRequest {
				t.Fatalf("游标未绑定查询关键词：%d %s", w.Code, w.Body)
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if strings.Join(got, ",") != strings.Join(expected, ",") {
		t.Fatalf("分页目录顺序、完整性或去重不正确：%v", got)
	}
	for _, test := range []struct{ query, cursor string }{{"", "invalid-cursor"}, {strings.Repeat("字", 201), ""}} {
		if w := searchChatSkillFixture(s, test.query, test.cursor); w.Code != http.StatusBadRequest {
			t.Fatalf("非法关键词或游标应返回 400：%d %s", w.Code, w.Body)
		}
	}
}

func TestChatSkillMentionMissingFiles(t *testing.T) {
	dir := t.TempDir()
	for _, skillDir := range []string{"", filepath.Join(dir, "does-not-exist"), dir} {
		page := decodeChatSkillPage(t, searchChatSkillFixture(&Server{skillDir: skillDir}, "", ""))
		if len(page.Items) != 0 {
			t.Fatalf("空目录应返回空列表：%+v", page)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "no-manifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeChatSkillFixture(t, dir, "deleted-skill", "---\nname: deleted-skill\ndescription: 删除测试\n---\n正文\n")
	s := &Server{m: &Manager{}, skillDir: dir}
	page := decodeChatSkillPage(t, searchChatSkillFixture(s, "", ""))
	if len(page.Items) != 1 || page.Items[0].ID != "deleted-skill" {
		t.Fatalf("没有 SKILL.md 的目录不应成为技能：%+v", page)
	}
	if err := os.Remove(filepath.Join(dir, "deleted-skill", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if page := decodeChatSkillPage(t, searchChatSkillFixture(s, "", "")); len(page.Items) != 0 {
		t.Fatalf("不能返回已删除的技能：%+v", page)
	}
	w := httptest.NewRecorder()
	if _, ok := s.prepareChatMentionMessage(w, "@[技能#deleted-skill]"); ok || w.Code != http.StatusBadRequest {
		t.Fatalf("发送前被删除的技能应返回 400：%d %s", w.Code, w.Body)
	}
}

func TestChatSkillMentionSearchRequiresLogin(t *testing.T) {
	dir := t.TempDir()
	writeChatSkillFixture(t, dir, "route-skill", "---\nname: route-skill\ndescription: 路由测试\n---\n正文\n")
	s := &Server{skillDir: dir, jwtKey: []byte("chat-skill-mention-signing-key")}
	h := s.Handler()
	for _, loggedIn := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodGet, "/api/chat/mentions?kind=skill", nil)
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
			t.Fatalf("登录=%v，状态=%d，期望=%d；%s", loggedIn, w.Code, want, w.Body)
		}
	}
}

func TestChatSkillMentionPrepareMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	writeChatSkillFixture(t, dir, "中文目录", "---\nname: actual-invocation\ndescription: 真实服务端描述\n---\n不应直接注入的技能正文独有标记\n")
	s := &Server{m: &Manager{}, skillDir: dir}
	w := httptest.NewRecorder()
	message := "请参考 @[技能#中文目录]"
	got, ok := s.prepareChatMentionMessage(w, message)
	if !ok || !strings.HasPrefix(got, message) || !strings.Contains(got, "Skill") || !strings.Contains(got, "真实服务端描述") {
		t.Fatalf("技能引用应附带加载提示和真实元数据：ok=%v，%s，响应：%s", ok, got, w.Body)
	}
	if !regexp.MustCompile(`"invocation_name"\s*:\s*"actual-invocation"`).MatchString(got) {
		t.Fatalf("必须使用 frontmatter 中的真实工具调用名称：%s", got)
	}
	if strings.Contains(got, "不应直接注入的技能正文独有标记") {
		t.Fatal("引用技能不能绕过 Skill 工具注入正文")
	}
	plain := "普通聊天 @技能 user@example.com"
	if got, ok := s.prepareChatMentionMessage(httptest.NewRecorder(), plain); !ok || got != plain {
		t.Fatalf("无引用消息被改变：%q %v", got, ok)
	}
	for _, message := range []string{"@[技能#missing]", "@[技能#../outside]", `@[技能#..\outside]`, "@[技能#]"} {
		w := httptest.NewRecorder()
		if _, ok := s.prepareChatMentionMessage(w, message); ok || w.Code != http.StatusBadRequest {
			t.Fatalf("缺失或非法技能引用必须在提交消息前拒绝：%q %d %s", message, w.Code, w.Body)
		}
	}
}

func TestChatSkillMentionRejectsOutsideSymlinks(t *testing.T) {
	for _, manifestLink := range []bool{false, true} {
		t.Run(fmt.Sprintf("manifest=%v", manifestLink), func(t *testing.T) {
			dir := t.TempDir()
			outside := t.TempDir()
			writeChatSkillFixture(t, outside, "outside-skill", "---\nname: outside-skill\ndescription: 不可暴露的目录外元数据\n---\n外部正文\n")
			source := filepath.Join(outside, "outside-skill")
			target := filepath.Join(dir, "linked-skill")
			if manifestLink {
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
				source = filepath.Join(source, "SKILL.md")
				target = filepath.Join(target, "SKILL.md")
			}
			if err := os.Symlink(source, target); err != nil {
				t.Skipf("当前环境无法创建符号链接：%v", err)
			}
			s := &Server{m: &Manager{}, skillDir: dir}
			search := searchChatSkillFixture(s, "", "")
			wantPrepare := http.StatusBadRequest
			if manifestLink {
				// 运行时会读取清单链接，无法安全确认同名覆盖时必须拒绝整个目录。
				if search.Code != http.StatusInternalServerError || strings.Contains(search.Body.String(), "不可暴露的目录外元数据") {
					t.Fatalf("不安全清单必须拒绝目录读取且不得泄露元数据：%d %s", search.Code, search.Body)
				}
				wantPrepare = http.StatusInternalServerError
			} else {
				page := decodeChatSkillPage(t, search)
				if len(page.Items) != 0 {
					t.Fatalf("不能列出目录外符号链接指向的技能元数据：%+v", page)
				}
			}
			w := httptest.NewRecorder()
			if message, ok := s.prepareChatMentionMessage(w, "@[技能#linked-skill]"); ok || w.Code != wantPrepare || strings.Contains(message, "不可暴露的目录外元数据") {
				t.Fatalf("不能解析目录外符号链接：ok=%v code=%d message=%s", ok, w.Code, message)
			}
		})
	}
}

func TestChatSkillMentionDuplicateInvocationNames(t *testing.T) {
	dir := t.TempDir()
	writeChatSkillFixture(t, dir, "z-final-skill", "---\nname: shared-invocation\ndescription: 排序后的生效技能\n---\n最新正文\n")
	writeChatSkillFixture(t, dir, "a-first-skill", "---\nname: shared-invocation\ndescription: 已被覆盖的旧技能\n---\n旧正文\n")
	s := &Server{m: &Manager{}, skillDir: dir}
	page := decodeChatSkillPage(t, searchChatSkillFixture(s, "", ""))
	if len(page.Items) != 1 || page.Items[0].ID != "z-final-skill" || page.Items[0].Description != "排序后的生效技能" {
		t.Fatalf("同名技能应与运行时一致，仅保留排序后加载的目录：%+v", page)
	}
	w := httptest.NewRecorder()
	if _, ok := s.prepareChatMentionMessage(w, "@[技能#a-first-skill]"); ok || w.Code != http.StatusBadRequest {
		t.Fatalf("不能将被覆盖目录引用到其它技能正文：%d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	if message, ok := s.prepareChatMentionMessage(w, "@[技能#z-final-skill]"); !ok || !strings.Contains(message, "shared-invocation") || !strings.Contains(message, "排序后的生效技能") {
		t.Fatalf("生效目录引用未解析为实际调用名称：%v %s %s", ok, message, w.Body)
	}
}

func TestChatSkillMentionBareManifestOverride(t *testing.T) {
	dir := t.TempDir()
	writeChatSkillFixture(t, dir, "a-directory", "---\nname: shared-invocation\ndescription: 已被裸文件覆盖的目录\n---\n目录正文\n")
	for name, manifest := range map[string]string{
		"z-override.md": "---\nname: shared-invocation\ndescription: 裸文件覆盖项\n---\n裸文件正文\n",
		"bare-only.md":  "---\nname: bare-only\ndescription: 没有同名目录的裸文件\n---\n正文\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{m: &Manager{}, skillDir: dir}
	page := decodeChatSkillPage(t, searchChatSkillFixture(s, "", ""))
	if len(page.Items) != 0 {
		t.Fatalf("裸文件覆盖后目录不可选，裸文件本身不能冒充技能目录：%+v", page)
	}
	for _, name := range []string{"a-directory", "bare-only", "z-override"} {
		w := httptest.NewRecorder()
		if _, ok := s.prepareChatMentionMessage(w, "@[技能#"+name+"]"); ok || w.Code != http.StatusBadRequest {
			t.Fatalf("不可选择的目录必须拒绝引用：%q %d %s", name, w.Code, w.Body)
		}
	}
}

func TestChatSkillMentionMetadataFallbacks(t *testing.T) {
	dir := t.TempDir()
	writeChatSkillFixture(t, dir, "legacy-skill", "---\nwhen_to_use: |\n  旧格式使用场景\n  第二行说明\n---\n旧格式正文\n")
	writeChatSkillFixture(t, dir, "no-description", "---\nname: empty-description\n---\n无描述正文\n")
	writeChatSkillFixture(t, dir, "preferred-description", "---\nname: primary-description\ndescription: 标准描述优先\nwhen_to_use: 不应覆盖标准描述\n---\n正文\n")
	s := &Server{m: &Manager{}, skillDir: dir}
	page := decodeChatSkillPage(t, searchChatSkillFixture(s, "旧格式使用场景", ""))
	if len(page.Items) != 1 || page.Items[0].ID != "legacy-skill" || page.Items[0].Description != "旧格式使用场景\n第二行说明" {
		t.Fatalf("旧格式 when_to_use 应作为描述参与展示和搜索：%+v", page)
	}
	w := httptest.NewRecorder()
	message, ok := s.prepareChatMentionMessage(w, "@[技能#legacy-skill]")
	if !ok || !regexp.MustCompile(`"invocation_name"\s*:\s*"legacy-skill"`).MatchString(message) {
		t.Fatalf("缺少 name 应使用技能目录作为工具调用名称：%v %s %s", ok, message, w.Body)
	}
	page = decodeChatSkillPage(t, searchChatSkillFixture(s, "no-description", ""))
	if len(page.Items) != 1 || page.Items[0].Description != "" {
		t.Fatalf("没有描述的有效技能也应能选择：%+v", page)
	}
	w = httptest.NewRecorder()
	if message, ok := s.prepareChatMentionMessage(w, "@[技能#no-description]"); !ok || !strings.Contains(message, "empty-description") {
		t.Fatalf("没有描述不应阻止技能引用：%v %s %s", ok, message, w.Body)
	}
	page = decodeChatSkillPage(t, searchChatSkillFixture(s, "preferred-description", ""))
	if len(page.Items) != 1 || page.Items[0].Description != "标准描述优先" {
		t.Fatalf("存在 description 时不能使用旧字段覆盖：%+v", page)
	}
}

func TestChatSkillMentionCatalogCancellation(t *testing.T) {
	dir := t.TempDir()
	writeChatSkillFixture(t, dir, "cancel-skill", "---\nname: cancel-skill\ndescription: 取消测试\n---\n正文\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if catalog, err := loadChatSkillCatalog(ctx, dir); !errors.Is(err, context.Canceled) || len(catalog) != 0 {
		t.Fatalf("已取消请求不能继续读取并返回技能目录：catalog=%v err=%v", catalog, err)
	}
}

func TestChatSkillMentionOversizedManifest(t *testing.T) {
	dir := t.TempDir()
	writeChatSkillFixture(t, dir, "oversized", "---\nname: oversized\ndescription: 不应读取超大文件\n---\n")
	if err := os.Truncate(filepath.Join(dir, "oversized", "SKILL.md"), maxSkillFileBytes+1); err != nil {
		t.Fatal(err)
	}
	writeChatSkillFixture(t, dir, "normal-skill", "---\nname: normal-skill\ndescription: 正常技能\n---\n正文\n")
	s := &Server{m: &Manager{}, skillDir: dir}
	search := searchChatSkillFixture(s, "", "")
	if search.Code != http.StatusInternalServerError || strings.Contains(search.Body.String(), "不应读取超大文件") {
		t.Fatalf("无法读取超大清单时不能返回可能与运行时不一致的目录：%d %s", search.Code, search.Body)
	}
	w := httptest.NewRecorder()
	if _, ok := s.prepareChatMentionMessage(w, "@[技能#oversized]"); ok || w.Code != http.StatusInternalServerError {
		t.Fatalf("超大清单不能绕过搜索限制直接引用：%d %s", w.Code, w.Body)
	}
}

func TestChatSkillMentionOversizedOverrideFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeChatSkillFixture(t, dir, "a-safe-skill", "---\nname: shared-invocation\ndescription: 已被运行时覆盖的旧描述\n---\n安全正文\n")
	writeChatSkillFixture(t, dir, "z-oversized-skill", "---\nname: shared-invocation\ndescription: 排序后覆盖前项的超大技能\n---\n")
	if err := os.Truncate(filepath.Join(dir, "z-oversized-skill", "SKILL.md"), maxSkillFileBytes+1); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{}, skillDir: dir}
	search := searchChatSkillFixture(s, "a-safe-skill", "")
	if search.Code != http.StatusInternalServerError || strings.Contains(search.Body.String(), "已被运行时覆盖的旧描述") {
		t.Fatalf("不能因跳过超大同名清单而展示错误的生效目录：%d %s", search.Code, search.Body)
	}
	w := httptest.NewRecorder()
	if message, ok := s.prepareChatMentionMessage(w, "@[技能#a-safe-skill]"); ok || w.Code != http.StatusInternalServerError || strings.Contains(message, "shared-invocation") {
		t.Fatalf("不能将旧目录引用解析成运行时另一个技能：ok=%v code=%d message=%s", ok, w.Code, message)
	}
}

func TestChatSkillMentionCatalogTotalReadLimit(t *testing.T) {
	dir := t.TempDir()
	// 使用稀疏文件构造真实文件大小边界，避免在测试代码中持有大块正文。
	for i := 0; i <= maxSkillTotalBytes/maxSkillFileBytes; i++ {
		name := fmt.Sprintf("limit-skill-%d", i)
		writeChatSkillFixture(t, dir, name, "---\nname: "+name+"\ndescription: 总量限制\n---\n")
		if err := os.Truncate(filepath.Join(dir, name, "SKILL.md"), maxSkillFileBytes); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := loadChatSkillCatalog(context.Background(), dir)
	if err == nil || len(catalog) != 0 {
		t.Fatalf("读取总量超限必须失败且不返回部分目录：catalog=%v err=%v", catalog, err)
	}
	if !strings.Contains(err.Error(), "总量") {
		t.Fatalf("应由总量限制拒绝，而不是其它读取错误：%v", err)
	}
}
