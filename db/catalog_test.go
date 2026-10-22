package db

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/catalog"
)

func catalogDBFixture(t *testing.T) *DB {
	t.Helper()
	d, _ := fofaSettingsTestDB(t)
	if _, err := d.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	if err := d.MigrateLLMModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	return d
}

func catalogSourceJSON() string { b, _ := json.Marshal(catalog.DefaultSource()); return string(b) }

func TestCatalogToolAtomicUpdatePreservesLocalSettings(t *testing.T) {
	d := catalogDBFixture(t)
	ctx := t.Context()
	template := CatalogTool{Key: "catalog_test", Description: "旧描述", Kind: "script", Schema: json.RawMessage(`{"type":"object","properties":{}}`), Exec: json.RawMessage(`{"code":"print('old')","timeout_ms":5000}`), Deferred: true}
	payload, _ := json.Marshal(template)
	if err := d.CatalogApply(ctx, "tool", template.Key, "absent", payload, nil, nil, "catalog.test.install", "v1", catalogSourceJSON()); err != nil {
		t.Fatal(err)
	}
	tool, err := d.GetTool(template.Key)
	if err != nil || tool == nil || tool.Enabled || len(tool.Agents) > 0 {
		t.Fatalf("新工具必须禁用且无绑定：%+v %v", tool, err)
	}
	tool.Enabled = true
	tool.Agents = []string{"worker"}
	tool.Exec = json.RawMessage(`{"code":"print('old')","timeout_ms":5000,"headers":{"Authorization":"local-secret"},"proxy":"http://local-proxy"}`)
	if err = d.UpdateCustomTool(tool); err != nil {
		t.Fatal(err)
	}
	h := catalog.Digest(CatalogToolPayload(tool))
	template.Description = "新描述"
	template.Exec = json.RawMessage(`{"code":"print('new')","timeout_ms":6000,"headers":{"Authorization":"remote"}}`)
	payload, _ = json.Marshal(template)
	if err = d.CatalogApply(ctx, "tool", template.Key, h, payload, nil, nil, "catalog.test.install", "v2", catalogSourceJSON()); err != nil {
		t.Fatal(err)
	}
	updated, _ := d.GetTool(template.Key)
	if !updated.Enabled || len(updated.Agents) != 1 || updated.Agents[0] != "worker" || !strings.Contains(string(updated.Exec), "local-secret") || !strings.Contains(string(updated.Exec), "local-proxy") || !strings.Contains(string(updated.Exec), "new") {
		t.Fatalf("本地凭据/绑定未保留或代码未更新：%+v", updated)
	}
	if strings.Contains(string(CatalogToolPayload(updated)), "local-secret") {
		t.Fatal("预览payload泄露实例凭据")
	}
	if err = d.CatalogApply(ctx, "tool", template.Key, h, payload, nil, nil, "catalog.test.install", "stale", catalogSourceJSON()); err == nil {
		t.Fatal("过期本地摘要未被拒绝")
	}
	if _, err = d.Exec(`CREATE FUNCTION reject_catalog_setting() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='catalog.test.fail' THEN RAISE EXCEPTION '模拟元数据失败'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_catalog_setting BEFORE INSERT OR UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION reject_catalog_setting()`); err != nil {
		t.Fatal(err)
	}
	before := string(CatalogToolPayload(updated))
	template.Description = "不应提交"
	payload, _ = json.Marshal(template)
	if err = d.CatalogApply(ctx, "tool", template.Key, catalog.Digest([]byte(before)), payload, nil, nil, "catalog.test.fail", "broken", catalogSourceJSON()); err == nil {
		t.Fatal("应触发元数据失败")
	}
	got, _ := d.GetTool(template.Key)
	if string(CatalogToolPayload(got)) != before {
		t.Fatal("元数据失败后工具内容未回滚")
	}
}

func TestCatalogAgentDependenciesAndModelPreferencesPreserved(t *testing.T) {
	d := catalogDBFixture(t)
	ctx := t.Context()
	if err := d.CreateCustomTool(&Tool{Key: "catalog_clock", Description: "时钟", Kind: "script", Schema: json.RawMessage(`{}`), Exec: json.RawMessage(`{"code":"print(1)"}`), Agents: []string{"other"}}); err != nil {
		t.Fatal(err)
	}
	a := CatalogAgent{Key: "catalog_assistant", Name: "证据助手", Description: "说明", Prompt: "整理已有证据"}
	b, _ := json.Marshal(a)
	if err := d.CatalogApply(ctx, "agent", a.Key, "absent", b, []string{"catalog_clock"}, []string{"evidence-summary"}, "catalog.test.agent", "v1", catalogSourceJSON()); err != nil {
		t.Fatal(err)
	}
	created, err := d.GetAgentByKey(a.Key)
	if err != nil || created == nil || created.Enabled {
		t.Fatalf("新Agent应禁用：%+v %v", created, err)
	}
	if err = d.SetAgentMaxTurns(a.Key, 17); err != nil {
		t.Fatal(err)
	}
	if err = d.SetAgentWebSearch(a.Key, true); err != nil {
		t.Fatal(err)
	}
	if err = d.SetAgentSkillVisibility(created.ID, []string{"evidence-summary", "local-skill"}); err != nil {
		t.Fatal(err)
	}
	oldPayload := CatalogAgentPayload(created, a.Prompt)
	a.Name = "新版助手"
	a.Prompt = "保留证据并说明失败"
	b, _ = json.Marshal(a)
	if err = d.CatalogApply(ctx, "agent", a.Key, catalog.Digest(oldPayload), b, []string{"catalog_clock"}, []string{"new-skill"}, "catalog.test.agent", "v2", catalogSourceJSON()); err != nil {
		t.Fatal(err)
	}
	updated, _ := d.GetAgentByKey(a.Key)
	if updated.MaxTurns != 17 || !updated.WebSearch || updated.Enabled || updated.Name != a.Name {
		t.Fatalf("本地偏好未保留：%+v", updated)
	}
	skills, err := d.AgentSkillNames(created.ID)
	if err != nil || len(skills) != 3 {
		t.Fatalf("技能绑定丢失：%v %v", skills, err)
	}
	tool, _ := d.GetTool("catalog_clock")
	if len(tool.Agents) != 2 || tool.Enabled {
		t.Fatalf("绑定应增补且不启用工具：%+v", tool)
	}
	if err = d.CatalogSetSource(ctx, `{"repository":"example/other","ref":"main","directory":"catalog"}`); err != nil {
		t.Fatal(err)
	}
	if err = d.CatalogApply(ctx, "agent", a.Key, catalog.Digest(CatalogAgentPayload(updated, a.Prompt)), b, nil, nil, "catalog.test.agent", "wrong-source", catalogSourceJSON()); err == nil {
		t.Fatal("源切换后过期预览未拒绝")
	}
}

func TestCatalogPromptUpdatesSerializeVersions(t *testing.T) {
	d := catalogDBFixture(t)
	agent, err := d.CreateAgent("catalog_prompt", "测试", "描述")
	if err != nil {
		t.Fatal(err)
	}
	const writers = 8
	start := make(chan struct{})
	done := make(chan error, writers)
	for n := 0; n < writers; n++ {
		go func() { <-start; _, err := d.SavePrompt(agent.ID, "提示词", "测试", "catalog-test"); done <- err }()
	}
	close(start)
	for n := 0; n < writers; n++ {
		if err := <-done; err != nil {
			t.Fatalf("并发提示词更新失败：%v", err)
		}
	}
	versions, err := d.ListPromptVersions(agent.ID)
	if err != nil || len(versions) != writers {
		t.Fatalf("提示词版本丢失：%v %v", versions, err)
	}
}
