// Package localagent 提供可独立安装的本地 Agent Skill 包。
package localagent

import (
	"archive/zip"
	"bytes"
	"embed"
	"io/fs"
)

//go:embed artex-agent
var bundle embed.FS

// Archive 从编译内嵌的只读文件生成 ZIP，不依赖部署机器上的源码目录。
func Archive() ([]byte, error) {
	var body bytes.Buffer
	w := zip.NewWriter(&body)
	err := fs.WalkDir(bundle, "artex-agent", func(name string, entry fs.DirEntry, err error) error {
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
	if err := w.Close(); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}
