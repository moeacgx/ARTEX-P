// Package localagent 提供可独立安装的本地 Agent Skill 包。
package localagent

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
)

//go:embed artex-agent
var bundle embed.FS

// Info 标识当前程序内嵌的 Skill 内容，与程序版本和构建时间无关。
type Info struct {
	Version string `json:"version"`
}

type manifest struct {
	Info
	Files []string `json:"files"`
}

func bundleManifest(source fs.FS) (manifest, error) {
	result := manifest{Files: []string{}}
	digest := sha256.New()
	err := fs.WalkDir(source, "artex-agent", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		relative := name[len("artex-agent/"):]
		// Git 换行设置不应让同一份技能被误判为不同版本。
		fileDigest := sha256.Sum256(bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n")))
		fmt.Fprintf(digest, "%s\x00%x\n", relative, fileDigest)
		result.Files = append(result.Files, relative)
		return nil
	})
	result.Version = hex.EncodeToString(digest.Sum(nil))
	return result, err
}

// Metadata 返回内嵌包的内容摘要；新增或修改任意包文件都会更新标识。
func Metadata() (Info, error) {
	manifest, err := bundleManifest(bundle)
	return manifest.Info, err
}

// Archive 从编译内嵌的只读文件生成 ZIP，不依赖部署机器上的源码目录。
func Archive() ([]byte, error) {
	metadata, err := bundleManifest(bundle)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	w := zip.NewWriter(&body)
	err = fs.WalkDir(bundle, "artex-agent", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := bundle.ReadFile(name)
		if err != nil {
			return err
		}
		file, err := w.Create(name)
		if err != nil {
			return err
		}
		_, err = file.Write(content)
		return err
	})
	if err != nil {
		return nil, err
	}
	file, err := w.Create("artex-agent/skill-manifest.json")
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(file).Encode(metadata); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}
