package localagent

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestContentVersionTracksFilesAndNormalizesLineEndings(t *testing.T) {
	source := fstest.MapFS{
		"artex-agent/SKILL.md":                {Data: []byte("# 技能\n")},
		"artex-agent/scripts/artex-agent.mjs": {Data: []byte("// 客户端\n")},
	}
	first, err := bundleManifest(source)
	if err != nil {
		t.Fatal(err)
	}
	source["artex-agent/SKILL.md"].Data = []byte("# 技能\r\n")
	second, err := bundleManifest(source)
	if err != nil || first.Version != second.Version {
		t.Fatalf("换行应兼容: %s %s %v", first.Version, second.Version, err)
	}
	source["artex-agent/SKILL.md"].Data = []byte("# 新技能\n")
	changed, err := bundleManifest(source)
	if err != nil || first.Version == changed.Version {
		t.Fatalf("内容更改必须更新版本: %v", err)
	}
	source["artex-agent/README.md"] = &fstest.MapFile{Data: []byte("说明")}
	added, err := bundleManifest(source)
	if err != nil || changed.Version == added.Version {
		t.Fatalf("新增文件必须更新版本: %v", err)
	}
}

func TestArchiveManifestMatchesClientActualContent(t *testing.T) {
	metadata, err := Metadata()
	if err != nil {
		t.Fatal(err)
	}
	archive, err := Archive()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	var recorded manifest
	for _, file := range reader.File {
		content, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(content)
		content.Close()
		if err != nil {
			t.Fatal(err)
		}
		if file.Name == "artex-agent/skill-manifest.json" {
			if err := json.Unmarshal(data, &recorded); err != nil {
				t.Fatal(err)
			}
		}
		target := filepath.Join(directory, filepath.FromSlash(file.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if recorded.Version != metadata.Version || len(recorded.Files) != len(reader.File)-1 {
		t.Fatalf("ZIP 清单与内嵌元数据不一致: %+v %+v", recorded, metadata)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("未安装 Node.js，跳过跨语言校验")
	}
	script := filepath.Join(directory, "artex-agent", "scripts", "artex-agent.mjs")
	// 用实际解压后的独立客户端确认 Go 与 JS 算法相同。
	command := exec.Command(node, "--input-type=module", "-", script)
	command.Stdin = strings.NewReader(`import { pathToFileURL } from "node:url"; const {getSkillVersion}=await import(pathToFileURL(process.argv[2])); process.stdout.write(await getSkillVersion());`)
	output, err := command.CombinedOutput()
	if err != nil || string(output) != metadata.Version {
		t.Fatalf("客户端内容版本不一致: %s %v", output, err)
	}
}
