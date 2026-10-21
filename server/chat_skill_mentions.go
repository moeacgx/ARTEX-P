package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/norma/skill"
)

type chatSkillMetadata struct {
	SkillName      string `json:"skill_name"`
	InvocationName string `json:"invocation_name"`
	Description    string `json:"description"`
}

type chatSkillMention struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type chatSkillCursor struct {
	Query string `json:"query"`
	After string `json:"after"`
}

func (s *Server) searchChatSkillMentions(w http.ResponseWriter, r *http.Request, query string) {
	w.Header().Set("Cache-Control", "no-store")
	var after chatSkillCursor
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		blob, err := base64.RawURLEncoding.DecodeString(cursor)
		if len(cursor) > 2048 || err != nil || json.Unmarshal(blob, &after) != nil ||
			after.Query != query || !validSkillName(after.After) {
			writeErr(w, http.StatusBadRequest, "分页位置无效，请重新搜索技能")
			return
		}
	}
	catalog, err := loadChatSkillCatalog(r.Context(), s.skillDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "技能目录暂不可读，请稍后重试")
		return
	}
	names := make([]string, 0, len(catalog))
	needle := strings.ToLower(query)
	for name, metadata := range catalog {
		if name > after.After && strings.Contains(strings.ToLower(name+"\n"+metadata.InvocationName+"\n"+metadata.Description), needle) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	page := struct {
		Items      []chatSkillMention `json:"items"`
		NextCursor string             `json:"next_cursor,omitempty"`
	}{Items: make([]chatSkillMention, 0)}
	for _, name := range names {
		if len(page.Items) == 20 {
			blob, _ := json.Marshal(chatSkillCursor{Query: query, After: page.Items[19].ID})
			page.NextCursor = base64.RawURLEncoding.EncodeToString(blob)
			break
		}
		page.Items = append(page.Items, chatSkillMention{
			Kind: "skill", ID: name, Label: name, Description: shortSkillDescription(catalog[name].Description, 240),
		})
	}
	writeJSON(w, http.StatusOK, page)
}

// loadChatSkillCatalog 复用运行时 YAML 解析及同名覆盖规则，但不返回正文或服务端路径。
// 查询不能触发技能调用、MCP 解锁、调用计数或可见性绑定变更。
func loadChatSkillCatalog(ctx context.Context, dir string) (map[string]chatSkillMetadata, error) {
	catalog := make(map[string]chatSkillMetadata)
	if dir == "" {
		return catalog, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	root, err := os.OpenRoot(dir)
	if os.IsNotExist(err) {
		return catalog, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := (skillArchiveFS{root, ctx}).ReadDir(".")
	if err != nil {
		return nil, err
	}
	byInvocation := make(map[string]chatSkillMetadata)
	var total int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		rel := name
		if entry.IsDir() {
			rel = filepath.Join(name, "SKILL.md")
		} else if !strings.HasSuffix(name, ".md") {
			continue
		}
		info, err := root.Lstat(rel)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// 不能跳过无法安全解析的候选：它在运行时可能覆盖同名的旧技能。
		if !info.Mode().IsRegular() || info.Size() > maxSkillFileBytes {
			return nil, errors.New("技能清单不是常规文件或超出读取限制")
		}
		total += info.Size()
		if total > maxSkillTotalBytes {
			return nil, errors.New("技能目录读取总量超出限制")
		}
		data, err := readChatSkillManifest(ctx, root, rel, info)
		if err != nil {
			return nil, err
		}
		metadata := skill.Parse(string(data))
		if metadata.Name == "" {
			metadata.Name = strings.TrimSuffix(name, ".md")
		}
		description := metadata.Description
		if description == "" {
			description = metadata.WhenToUse
		}
		item := chatSkillMetadata{InvocationName: metadata.Name, Description: shortSkillDescription(description, 16000)}
		if entry.IsDir() && validSkillName(name) {
			item.SkillName = name
		}
		// 与运行时 Registry 一致：后加载的同名技能覆盖前者。
		// 裸 .md 参与覆盖，但不冒充可在技能管理页选择的目录。
		byInvocation[metadata.Name] = item
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, item := range byInvocation {
		if item.SkillName != "" {
			catalog[item.SkillName] = item
		}
	}
	return catalog, nil
}

func readChatSkillManifest(ctx context.Context, root *os.Root, rel string, expected os.FileInfo) ([]byte, error) {
	file, err := openSkillArchiveFile(root, rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, expected) {
		return nil, errors.New("技能清单发生变化")
	}
	data, err := io.ReadAll(io.LimitReader(skillArchiveReader{ctx, file}, expected.Size()+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != expected.Size() {
		return nil, errors.New("技能清单大小发生变化")
	}
	return data, nil
}

func shortSkillDescription(description string, limit int) string {
	runes := []rune(strings.TrimSpace(description))
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return string(runes)
}
