package server

import (
	"net/http"
	"strconv"
)

func (s *Server) localAgentFindingStats(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("task") == "" {
		s.findingStats(w, r)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("task"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "任务 ID 必须为正整数")
		return
	}
	pg := s.pg(w)
	if pg == nil {
		return
	}
	stats, err := pg.FindingStatsForTask(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// 仅暴露创建对话所需的目录，不返回提示词、变量或其他 Agent 配置。
func (s *Server) localAgentConversationAgents(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	agents, err := pg.ListAgents()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		Key         string `json:"key"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	items := make([]item, 0, len(agents))
	for _, agent := range agents {
		items = append(items, item{agent.Key, agent.Name, agent.Description})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": items})
}

// 本地 Agent 只需服务器目录，不能返回可能携带凭据的环境变量、参数及 URL。
func (s *Server) localAgentMCPServers(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	servers, err := pg.ListMCP()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		ID        int64    `json:"id"`
		Name      string   `json:"name"`
		Transport string   `json:"transport"`
		Enabled   bool     `json:"enabled"`
		Insecure  bool     `json:"insecure"`
		Tools     []string `json:"tools"`
	}
	items := make([]item, 0, len(servers))
	for _, server := range servers {
		items = append(items, item{server.ID, server.Name, server.Transport, server.Enabled, server.Insecure, server.Tools})
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": items})
}
